package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// What watch tells Claude Code (apps/try's end-to-end test reads the same):
// write the conversation's link to a file, then run a command with Monitor
// that waits for the conversation's messages, one JSON line each.
var (
	linkStep   = regexp.MustCompile(`If you haven't written this conversation's link to the file ~/\.wakeshift/links/([0-9a-f]{16}) yet, first write it .*? its whole content this one line: (\S+) \.`)
	monitorCmd = regexp.MustCompile(`with this command: (f=\$HOME/\.wakeshift/links/[0-9a-f]{16}; [^\n]*?c=; p=\$PPID; .*?; done; fi) \.`)
	hookURL    = regexp.MustCompile(`<code id="url">([^<]*)</code>`)
)

// gateWebhooks: steps 7 to 9, against Webhooks at base.
func gateWebhooks(ctx context.Context, base string) error {
	base = strings.TrimRight(base, "/")
	// Step 8 runs the command watch's result gives, as Claude Code's
	// Monitor would: only from a Webhooks on this machine.
	if u, err := neturl.Parse(base); err != nil || u.Scheme != "http" ||
		(u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
		return fmt.Errorf("-webhooks-url must be Webhooks on this machine (http://localhost:<port>), since the gate runs the wait command it returns")
	}
	if err := waitHealthy(ctx, base+"/health"); err != nil {
		return err
	}

	step("7. Webhooks: get a link from its page")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/links", nil)
	resp, err := noRedirect.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	page := resp.Header.Get("Location")
	code, ok := strings.CutPrefix(page, "/l/")
	if resp.StatusCode != http.StatusSeeOther || !ok || code == "" {
		return fmt.Errorf("POST /links: %d, Location %q (want 303 to /l/<code>)", resp.StatusCode, page)
	}
	body, err := get(ctx, base+page)
	if err != nil {
		return err
	}
	mcpURL := base + "/t/" + code + "/mcp"
	if !strings.Contains(body, "claude mcp add --transport http webhooks "+mcpURL) {
		return errors.New("the link's page doesn't show Claude Code's command for its MCP URL")
	}
	m := hookURL.FindStringSubmatch(body)
	if m == nil {
		return errors.New("the link's page shows no webhook URL")
	}
	webhook := html.UnescapeString(m[1])
	if !strings.HasPrefix(webhook, base+"/in/") {
		return errors.New("the webhook URL isn't on Webhooks' public URL")
	}

	step("8. an agent (as Claude Code) calls watch, and runs the wait it's given")
	client := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "2.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: mcpURL, MaxRetries: -1}, nil)
	if err != nil {
		return fmt.Errorf("connect to the link's MCP URL: %w", err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "watch", Arguments: map[string]any{}})
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	var said strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			said.WriteString(t.Text + "\n")
		}
	}
	if res.IsError || !strings.HasPrefix(said.String(), "Watching your webhook.") {
		return errors.New("watch didn't say it's watching")
	}
	// The link file goes in a HOME of the gate's own, never the user's.
	home, err := os.MkdirTemp("", "releasegate-home-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	for _, l := range linkStep.FindAllStringSubmatch(said.String(), -1) {
		dir := filepath.Join(home, ".wakeshift", "links")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, l[1]), []byte(l[2]+"\n"), 0o600); err != nil {
			return err
		}
	}
	cmd := monitorCmd.FindStringSubmatch(said.String())
	if cmd == nil {
		return errors.New("watch's result has no command to wait with")
	}
	lines, stop, err := monitor(home, cmd[1])
	if err != nil {
		return err
	}
	defer stop()

	step(`9. a JSON POST to the webhook URL (as curl -H 'Content-Type: application/json' -d '{"hello":"world"}') wakes it`)
	// Its wait is open before the post: give the first request a moment.
	time.Sleep(2 * time.Second)
	code2, err := post(ctx, webhook, []byte(`{"hello":"world"}`), nil)
	if err != nil || code2 != http.StatusAccepted {
		return fmt.Errorf("POST to the webhook URL: %d %v (want 202)", code2, err)
	}
	deadline := time.After(30 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return errors.New("the wait ended before the post arrived")
			}
			var ev struct {
				Type string `json:"type"`
				Data struct {
					Body map[string]any `json:"body"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(l), &ev) != nil || ev.Type != "event" {
				continue
			}
			if ev.Data.Body["hello"] != "world" {
				return fmt.Errorf("the event's body: %v", ev.Data.Body)
			}
			return nil
		case <-deadline:
			return errors.New("the post didn't reach the watching conversation in 30 seconds")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func waitHealthy(ctx context.Context, url string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: not up after 2 minutes (%v)", url, err)
		}
		time.Sleep(time.Second)
	}
}

func get(ctx context.Context, url string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("GET %s: %d", req.URL.Path, resp.StatusCode)
	}
	return string(b), err
}

// monitor runs the wait command in sh, as Claude Code's Monitor does (with
// HOME the gate's own), and sends each line it prints. stop kills it and
// its curl.
func monitor(home, command string) (<-chan string, func(), error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	lines := make(chan string, 16)
	done := make(chan struct{})
	read := make(chan struct{})
	go func() {
		defer close(read)
		defer close(lines)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-done: // nobody's reading: keep draining until the pipe closes
			}
		}
	}()
	stop := func() {
		close(done)
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		// The pipe reaches EOF once everything in the group has exited; give up
		// waiting after a few seconds rather than hang the gate.
		select {
		case <-read:
		case <-time.After(5 * time.Second):
		}
		cmd.Wait()
	}
	return lines, stop, nil
}
