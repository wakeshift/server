// Command releasegate is the published server's release gate: what its
// quickstart promises, end to end, against the server docker compose runs
// from this tree. Run it from the tree's root, with Docker:
//
//	go run ./internal/releasegate
//
// It makes an app key in memory (never printed or written), then:
//
//  1. builds and starts docker-compose.yml's Postgres and server
//     (docker compose up --build), with the key's public half in
//     WAKESHIFT_APPS as the app "example" and a fresh WAKESHIFT_SECRET_KEY;
//  2. waits for /ready, which the server answers once its migrations ran
//     on the fresh database (as the owner role, not a superuser);
//  3. checks the app is registered: the Go SDK, with the key, asks the
//     server which app it is;
//  4. serves an MCP server for the app (go-sdk, with the Go SDK attached)
//     and, as an MCP Events client would, opens events/stream on it: the
//     subscription is made and held on the server (push);
//  5. emits an event with the SDK, and posts a webhook signed the Standard
//     Webhooks way to a publish URL made with a signing secret (an
//     unsigned post is refused);
//  6. reads both, in order, as notifications/events/event on the client's
//     stream.
//
// With -webhooks it also starts Webhooks (apps/try, the compose profile
// webhooks), registered with a second fresh key, and then (webhooks.go):
//
//  7. gets a link from its page, as a person does;
//  8. connects to the link's MCP URL as Claude Code, calls watch, and runs
//     the wait the result says to run (as Claude Code's Monitor does);
//  9. POSTs {"hello":"world"} (JSON) to the webhook URL, as curl does, and reads
//     the event on that wait.
//
// It stops the services (docker compose down -v) unless -keep is given,
// printing the server's log first when a step failed. It wakes no real
// agent: the bridges (connectors and plugins) are tested in their own
// repositories.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// The app the gate registers, and the event it subscribes to.
const (
	appID = "example"
	user  = "gate-user"
)

type buildArgs struct {
	Build string `json:"build" jsonschema:"The build"`
}

type buildDone struct {
	Status string `json:"status"`
}

func main() {
	core := flag.String("core", "http://localhost:8370", "the server's URL, as docker-compose.yml publishes it (its WAKESHIFT_PUBLIC_URL)")
	compose := flag.Bool("compose", true, "start the server with docker compose (false: one is running at -core, with the app's key from WAKESHIFT_KEY)")
	keep := flag.Bool("keep", false, "leave the services running")
	webhooks := flag.Bool("webhooks", false, "also start Webhooks (the compose profile webhooks) and wake a watching client with a POST to a webhook URL")
	webhooksURL := flag.String("webhooks-url", "http://localhost:8380", "with -webhooks: Webhooks' URL, as docker-compose.yml publishes it (with -compose=false, one must already be running there)")
	flag.Parse()
	if err := run(*core, *compose, *keep, *webhooks, *webhooksURL); err != nil {
		fmt.Fprintln(os.Stderr, "releasegate: FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("releasegate: ok")
}

func step(format string, a ...any) { fmt.Printf("releasegate: "+format+"\n", a...) }

func run(core string, compose, keep, webhooks bool, webhooksURL string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	key := os.Getenv("WAKESHIFT_KEY")
	if compose {
		// The key and WAKESHIFT_APPS as cmd/appkey -app makes them.
		seed := make([]byte, ed25519.SeedSize)
		rand.Read(seed)
		var apps string
		if key, apps, err = appkey.ForServer(appID, "Example", seed); err != nil {
			return err
		}
		secret := make([]byte, 48)
		rand.Read(secret)
		env := append(os.Environ(), "WAKESHIFT_SECRET_KEY="+base64.StdEncoding.EncodeToString(secret))
		profile := []string{"compose"}
		what := "Postgres 17, the server"
		if webhooks {
			// Webhooks' own key, registered beside the gate's app, and the
			// secret that signs its links (as deploy/webhooks-env.sh makes them).
			wseed := make([]byte, ed25519.SeedSize)
			rand.Read(wseed)
			wkey, wapps, err := appkey.ForServer("webhooks", "Webhooks", wseed)
			if err != nil {
				return err
			}
			if apps, err = joinApps(apps, wapps); err != nil {
				return err
			}
			tsecret := make([]byte, 48)
			rand.Read(tsecret)
			env = append(env, "WEBHOOKS_KEY="+wkey, "WEBHOOKS_SECRET="+base64.StdEncoding.EncodeToString(tsecret))
			profile = append(profile, "--profile", "webhooks")
			what += ", Webhooks"
		}
		env = append(env, "WAKESHIFT_APPS="+apps)
		dc := func(args ...string) []string { return append(append([]string{}, profile...), args...) }
		if !keep {
			defer func() {
				if err != nil {
					docker(context.Background(), env, os.Stderr, dc("logs", "--no-color", "server")...)
					if webhooks {
						docker(context.Background(), env, os.Stderr, dc("logs", "--no-color", "webhooks")...)
					}
				}
				docker(context.Background(), env, os.Stdout, dc("down", "-v")...)
			}()
		}
		step("1. docker compose up --build (%s)", what)
		if err := docker(ctx, env, os.Stdout, dc("up", "-d", "--build", "--wait")...); err != nil {
			return fmt.Errorf("docker compose up: %w", err)
		}
	} else if key == "" {
		return errors.New("-compose=false needs the app's key in WAKESHIFT_KEY")
	}

	step("2. the server migrated a fresh database and is ready")
	if err := waitReady(ctx, core); err != nil {
		return err
	}

	step("3. the app is registered (WAKESHIFT_APPS)")
	wake, err := wakeshift.New(wakeshift.WithKey(key), wakeshift.WithCore(core), wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	defer wake.Close()
	if id, err := wake.AppID(ctx); err != nil || (compose && id != appID) {
		return fmt.Errorf("the key's app: %q, %v", id, err)
	}
	finished, err := wakeshift.DefineEvent(wake, "build.finished", wakeshift.EventDef[buildArgs, buildDone]{
		Description: "A build finished",
		Authorize: func(_ context.Context, s wakeshift.Subscriber, _ buildArgs) (bool, error) {
			return s.User == user, nil
		},
	})
	if err != nil {
		return err
	}
	endpoint, stop, err := serveApp(wake)
	if err != nil {
		return err
	}
	defer stop()

	step("4. an MCP Events client subscribes on push (events/stream)")
	stream, err := openStream(ctx, endpoint)
	if err != nil {
		return err
	}
	defer stream.Close()
	if n, err := stream.next(30 * time.Second); err != nil || n.Method != "notifications/events/active" {
		return fmt.Errorf("the stream's first message: %s %v", n.Method, err)
	}

	step("5a. the app emits an event")
	if _, err := finished.Emit(ctx, buildArgs{Build: "42"}, buildDone{Status: "emitted"}); err != nil {
		return fmt.Errorf("emit: %w", err)
	}
	if err := expectEvent(stream, "emitted"); err != nil {
		return err
	}

	step("5b. a signed webhook to a publish URL")
	whsec := make([]byte, 32)
	rand.Read(whsec)
	secret := "whsec_" + base64.StdEncoding.EncodeToString(whsec)
	pu, err := finished.CreatePublishURL(ctx, buildArgs{Build: "42"}, wakeshift.SigningSecret(secret))
	if err != nil {
		return fmt.Errorf("publish URL: %w", err)
	}
	body := []byte(`{"status":"webhook"}`)
	if code, err := post(ctx, pu.URL, body, nil); err != nil || code != http.StatusUnauthorized {
		return fmt.Errorf("an unsigned post to a signed publish URL: %d %v (want 401)", code, err)
	}
	if code, err := post(ctx, pu.URL, body, sign(whsec, body)); err != nil || code/100 != 2 {
		return fmt.Errorf("a signed post: %d %v", code, err)
	}

	step("6. both delivered on push, in order")
	if err := expectEvent(stream, "webhook"); err != nil {
		return err
	}
	if webhooks {
		return gateWebhooks(ctx, webhooksURL)
	}
	return nil
}

// joinApps is two WAKESHIFT_APPS lists as one.
func joinApps(a, b string) (string, error) {
	var x, y []map[string]any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		return "", err
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		return "", err
	}
	out, err := json.Marshal(append(x, y...))
	return string(out), err
}

// docker runs docker with args in the current directory (the tree's root).
func docker(ctx context.Context, env []string, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, out, os.Stderr
	return cmd.Run()
}

func waitReady(ctx context.Context, core string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", core+"/ready", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s/ready: not ready after 3 minutes (%v)", core, err)
		}
		time.Sleep(time.Second)
	}
}

// serveApp serves the app's MCP server, the Go SDK attached, on a local
// port: stateless, behind bearer tokens that name the user (a real server
// verifies its own).
func serveApp(wake *wakeshift.Client) (string, func(), error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "releasegate", Version: "1.0.0"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{
		Identify: func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
			if r.Token == nil {
				return wakeshift.Identity{}, nil
			}
			return wakeshift.Identity{User: r.Token.UserID}, nil
		},
		RequireUser: true,
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	hs := &http.Server{Handler: auth.RequireBearerToken(verify, nil)(attached.Handler(h)), ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln)
	return "http://" + ln.Addr().String() + "/mcp", func() { hs.Close() }, nil
}

// stream is an events/stream request's open response: the messages of a
// Streamable HTTP SSE response, as an MCP Events client reads them.
type stream struct {
	body io.ReadCloser
	msgs chan message
	errc chan error
	done chan struct{}
}

type message struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func openStream(ctx context.Context, endpoint string) (*stream, error) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/stream",
		"params": map[string]any{"name": "build.finished", "arguments": buildArgs{Build: "42"}, "cursor": nil}})
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+user)
	req.Header.Set("User-Agent", "wakeshift-releasegate/1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("events/stream: %d, not a stream: %s", resp.StatusCode, raw)
	}
	s := &stream{body: resp.Body, msgs: make(chan message, 100), errc: make(chan error, 1), done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data:")
			var m message
			// Heartbeats never queue, so a slow reader can't fill the queue
			// with them and hold up an event behind it.
			if !ok || json.Unmarshal([]byte(strings.TrimSpace(data)), &m) != nil || m.Method == "notifications/events/heartbeat" {
				continue
			}
			select {
			case s.msgs <- m:
			case <-s.done:
				return
			}
		}
		s.errc <- fmt.Errorf("the stream ended (%v)", sc.Err())
	}()
	return s, nil
}

func (s *stream) Close() {
	close(s.done)
	s.body.Close()
}

// next is the stream's next message (heartbeats aren't queued).
func (s *stream) next(wait time.Duration) (message, error) {
	t := time.After(wait)
	for {
		select {
		case m := <-s.msgs:
			if m.Method == "" {
				return m, fmt.Errorf("a final response: %s%s", m.Result, m.Error)
			}
			return m, nil
		case err := <-s.errc:
			return message{}, err
		case <-t:
			return message{}, fmt.Errorf("nothing in %s", wait)
		}
	}
}

// expectEvent reads the next event and checks its payload's status.
func expectEvent(s *stream, status string) error {
	m, err := s.next(30 * time.Second)
	if err != nil {
		return fmt.Errorf("waiting for the %s event: %w", status, err)
	}
	var p struct {
		Name string    `json:"name"`
		Data buildDone `json:"data"`
	}
	if m.Method != "notifications/events/event" || json.Unmarshal(m.Params, &p) != nil || p.Name != "build.finished" || p.Data.Status != status {
		return fmt.Errorf("want the %s event, got %s %s", status, m.Method, m.Params)
	}
	return nil
}

// sign is a Standard Webhooks signature's headers for body.
func sign(key, body []byte) http.Header {
	id := make([]byte, 12)
	rand.Read(id)
	msgID := "msg_" + base64.RawURLEncoding.EncodeToString(id)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID + "." + ts + "."))
	mac.Write(body)
	h := http.Header{}
	h.Set("webhook-id", msgID)
	h.Set("webhook-timestamp", ts)
	h.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return h
}

func post(ctx context.Context, url string, body []byte, h http.Header) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	for k, v := range h {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Not the URL: its token is the capability.
		var ue *neturl.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
