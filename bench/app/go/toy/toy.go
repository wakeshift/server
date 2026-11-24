// Package toy is what the bench's Go toy apps share (bench/app/CONTRACT.md):
// the contract's words, the event, the bench's side API and the plumbing
// around the MCP endpoint. Each port (gosdk, mcpgo) only builds its MCP
// server on its framework, with the three tools wrapped by the Go SDK, the
// way an app on that framework does.
//
// A port runs behind the bench's front (bench/app/front.ts), which records
// every call from the HTTP exchange. It reads, from the environment:
//
//	PORT                where to listen (127.0.0.1)
//	BENCH_KEY_FILE      the app key's file (mode 0600; never printed)
//	WAKESHIFT_CORE      the core
//	BENCH_CONTROL_FILE  the bench's control token's file (mode 0600)
//	BENCH_CONTRACT      contract.json
//	BENCH_MCP_NAME      the MCP server's name (default the contract's)
//	BENCH_WAIT          "1": serve the SDK's wait tool (BuiltinTools.Wait)
//
// It exits when its standard input closes (the front is gone).
package toy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/wakeshift/server/pkg/wakeshift"
)

// Contract is contract.json: the words every variant serves.
type Contract struct {
	Server struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"server"`
	Event struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Topic       string `json:"topic"`
		Text        string `json:"text"`
		TextTagged  string `json:"textTagged"`
	} `json:"event"`
	SubscribeTTL string `json:"subscribeTtl"`
	Tools        struct {
		Watch struct {
			Description string `json:"description"`
			Text        string `json:"text"`
		} `json:"watch"`
		Unwatch struct {
			Description string `json:"description"`
			Ended       string `json:"ended"`
			NotWatching string `json:"notWatching"`
		} `json:"unwatch"`
		ReportSeen struct {
			Description    string `json:"description"`
			IDsDescription string `json:"idsDescription"`
			Text           string `json:"text"`
		} `json:"report_seen"`
	} `json:"tools"`
}

// Args are bench.event's arguments: its topic is bench/<run>/<lane>.
type Args struct {
	Run  string `json:"run"`
	Lane string `json:"lane"`
}

// Payload is one bench event.
type Payload struct {
	Code   string `json:"code"`
	Seq    int    `json:"seq"`
	SentAt string `json:"sent_at"`
	Tag    string `json:"tag,omitempty"`
}

// Text is the event's line for the agent.
func (c *Contract) Text(p Payload) string {
	if p.Tag != "" {
		return strings.NewReplacer("{code}", p.Code, "{tag}", p.Tag).Replace(c.Event.TextTagged)
	}
	return strings.ReplaceAll(c.Event.Text, "{code}", p.Code)
}

// SeenText is report_seen's reply.
func (c *Contract) SeenText(n int) string {
	return strings.ReplaceAll(c.Tools.ReportSeen.Text, "{n}", fmt.Sprint(n))
}

// TTL is a subscription's lifetime ("1d").
func (c *Contract) TTL() time.Duration {
	if d, err := time.ParseDuration(c.SubscribeTTL); err == nil {
		return d
	}
	if strings.HasSuffix(c.SubscribeTTL, "d") {
		var n int
		if _, err := fmt.Sscanf(c.SubscribeTTL, "%dd", &n); err == nil {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	return 24 * time.Hour
}

// ServerName is the MCP server's name: BENCH_MCP_NAME, else the contract's.
func (c *Contract) ServerName() string {
	if n := os.Getenv("BENCH_MCP_NAME"); n != "" {
		return n
	}
	return c.Server.Name
}

// Note is what a tool's handler tells the front about its call, which the
// front can't see on the wire: the subscription, a short hash of the
// session (never the session itself) and what Wakeshift recognized.
type Note struct {
	Tool         string                `json:"tool"`
	Lane         string                `json:"lane"`
	Subscription string                `json:"subscription,omitempty"`
	Session      string                `json:"session,omitempty"`
	Agent        *wakeshift.Recognized `json:"agent,omitempty"`
}

// App is a running toy app's shared state.
type App struct {
	Contract *Contract
	Kit      *wakeshift.Client
	ID       string // the app's
	Event    *wakeshift.Event[Args, Payload]
	Run      string
	control  string
	mu       sync.Mutex
	notes    map[string]Note // by the front's call ID (CallHeader)
}

// CallHeader carries the front's ID for one HTTP exchange, which a handler
// files its Note under.
const CallHeader = "X-Wakebench-Call"

// Open reads the environment and the contract, opens the app and defines
// bench.event. It exits the process when standard input closes.
func Open(ctx context.Context) *App {
	go func() {
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()
	b, err := os.ReadFile(os.Getenv("BENCH_CONTRACT"))
	if err != nil {
		log.Fatalf("BENCH_CONTRACT: %v", err)
	}
	var c Contract
	if err := json.Unmarshal(b, &c); err != nil {
		log.Fatalf("contract.json: %v", err)
	}
	key, err := secretFile("BENCH_KEY_FILE")
	if err != nil {
		log.Fatal(err)
	}
	control, err := secretFile("BENCH_CONTROL_FILE")
	if err != nil {
		log.Fatal(err)
	}
	kit, err := wakeshift.New(wakeshift.WithKey(key), wakeshift.Required())
	if err != nil {
		log.Fatal(err)
	}
	id, err := kit.AppID(ctx)
	if err != nil {
		log.Fatal(err)
	}
	// The front checked the key's app before starting this; again here,
	// before anything is sent with it.
	if want := os.Getenv("BENCH_REQUIRE_APP"); want != "" && id != want {
		log.Fatalf("refused: the key is for %s, not %s", id, want)
	}
	ev, err := wakeshift.DefineEvent(kit, c.Event.Name, wakeshift.EventDef[Args, Payload]{
		Description: c.Event.Description,
		Topic:       c.Event.Topic,
		Text:        func(p Payload, _ Args) string { return c.Text(p) },
		// The bench's events are public: anyone may watch a lane.
		Public: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	run := os.Getenv("BENCH_RUN")
	if run == "" {
		run = "manual"
	}
	return &App{Contract: &c, Kit: kit, ID: id, Event: ev, Run: run, control: control, notes: map[string]Note{}}
}

func secretFile(name string) (string, error) {
	path := os.Getenv(name)
	if path == "" {
		return "", fmt.Errorf("set %s", name)
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s must be readable only by you (chmod 600)", name)
	}
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

// ArgsOf is a lane's arguments to bench.event.
func (a *App) ArgsOf(lane string) Args { return Args{Run: a.Run, Lane: lane} }

// Short is a session's short hash, as the TypeScript app records it.
func Short(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:12]
}

// Noted files a handler's Note under its exchange's call ID.
func (a *App) Noted(h http.Header, n Note) {
	id := h.Get(CallHeader)
	if id == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.notes[id] = n
}

// Watch subscribes the calling conversation: the watch tool's body,
// whatever the framework.
func (a *App) Watch(ctx context.Context, call *wakeshift.ToolCall, lane string, h http.Header) (string, error) {
	sub, err := a.Event.Subscribe(ctx, call, a.ArgsOf(lane), wakeshift.TTL(a.Contract.TTL()))
	if err != nil {
		return "", err
	}
	rec := sub.Recognized
	a.Noted(h, Note{Tool: "watch", Lane: lane, Subscription: sub.ID, Session: Short(string(sub.Session)), Agent: &rec})
	return a.Contract.Tools.Watch.Text, nil
}

// Unwatch ends the calling conversation's subscription.
func (a *App) Unwatch(ctx context.Context, call *wakeshift.ToolCall, lane string, h http.Header) (string, error) {
	ended, err := a.Event.Unsubscribe(ctx, call, a.ArgsOf(lane))
	if err != nil {
		return "", err
	}
	a.Noted(h, Note{Tool: "unwatch", Lane: lane})
	if ended {
		return a.Contract.Tools.Unwatch.Ended, nil
	}
	return a.Contract.Tools.Unwatch.NotWatching, nil
}

// ReportSeen takes the codes the agent says it received.
func (a *App) ReportSeen(lane string, ids []string, h http.Header) string {
	a.Noted(h, Note{Tool: "report_seen", Lane: lane})
	return a.Contract.SeenText(len(ids))
}

// Wait is whether the port serves the SDK's wait tool (BENCH_WAIT=1; off,
// the SDK's default, otherwise).
func Wait() bool { return os.Getenv("BENCH_WAIT") == "1" }

var laneRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// Lane is the bearer token the front puts on every request: the lane, whose
// user every caller of its server is.
func Lane(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
	if r.Token == nil || !laneRE.MatchString(r.Token.UserID) {
		return wakeshift.Identity{}, errors.New("no lane")
	}
	return wakeshift.Identity{User: r.Token.UserID}, nil
}

// LaneOf is the lane a handler's request is for, from its headers.
func LaneOf(h http.Header) string {
	tok, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if !ok || !laneRE.MatchString(tok) {
		return ""
	}
	return tok
}

// Serve listens on PORT: mcp (the framework's handler, wrapped by the SDK)
// at /mcp behind the lane's bearer token, and the bench's side API.
func (a *App) Serve(mcp http.Handler) {
	authed := auth.RequireBearerToken(func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if !laneRE.MatchString(token) {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(24 * time.Hour)}, nil
	}, nil)(mcp)
	mux := http.NewServeMux()
	mux.Handle("/mcp", authed)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"ok":true}`) })
	mux.HandleFunc("POST /bench/emit", a.guard(a.emit))
	mux.HandleFunc("POST /bench/status", a.guard(a.status))
	mux.HandleFunc("GET /bench/note", a.guard(a.note))
	addr := "127.0.0.1:" + os.Getenv("PORT")
	log.Printf("listening on %s (app %s)", addr, a.ID)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (a *App) guard(h http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + a.control)
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeJSON(w, 401, map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "control token"}})
			return
		}
		h(w, r)
	}
}

func (a *App) emit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Lane string  `json:"lane"`
		ID   string  `json:"id"`
		P    Payload `json:"payload"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil || !laneRE.MatchString(in.Lane) {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "bad_request", "message": "lane"}})
		return
	}
	out, err := a.Event.Emit(r.Context(), a.ArgsOf(in.Lane), in.P, wakeshift.WithID(in.ID))
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": map[string]string{"code": "emit", "message": err.Error()}})
		return
	}
	writeJSON(w, 200, map[string]any{"matched": out.Matched})
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Lane          string   `json:"lane"`
		Subscriptions []string `json:"subscriptions"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in) != nil || !laneRE.MatchString(in.Lane) {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "bad_request", "message": "lane"}})
		return
	}
	st, err := a.Event.Status(r.Context(), a.ArgsOf(in.Lane))
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": map[string]string{"code": "status", "message": err.Error()}})
		return
	}
	each := []any{}
	if len(in.Subscriptions) > 0 {
		subs := make([]wakeshift.Recipient, len(in.Subscriptions))
		for i, id := range in.Subscriptions {
			subs[i] = wakeshift.Recipient{User: in.Lane, Subscription: id}
		}
		got, err := a.Event.StatusOf(r.Context(), subs...)
		if err != nil {
			for _, id := range in.Subscriptions {
				each = append(each, map[string]string{"id": id, "error": err.Error()})
			}
		} else {
			for _, s := range got {
				each = append(each, s)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"event": st, "subscriptions": each})
}

func (a *App) note(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("call")
	a.mu.Lock()
	n, ok := a.notes[id]
	delete(a.notes, id)
	a.mu.Unlock()
	if !ok {
		writeJSON(w, 200, map[string]any{"note": nil})
		return
	}
	writeJSON(w, 200, map[string]any{"note": n})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
