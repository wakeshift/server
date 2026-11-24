package coretest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Notes: the test events most of these tests subscribe conversations to
// and emit, through the app API's own routes (/event-subscriptions,
// /events/emit, /calls/addition), as an SDK's wrapped tool does. The SDK's
// wrapper itself is tested in sdkwrapped_test.go; these tests are about
// what the core does for a conversation, so they call the routes with the
// call they name.
//
// A note's topic is its arguments' values joined by "/": "ci/acme/web" is
// note.three {a: ci, b: acme, c: web}, routed to the topic ci/acme/web, so
// queries on topic_subscriptions and topic_events see the same topic. Its
// optional arguments (branch, os, round) filter the payload's field of the
// same name.

// Caller is who called one of the app's tools, as the app API takes it
// (the SDK's wire call).
type Caller struct {
	User      string         `json:"user,omitempty"`
	Client    string         `json:"client"`
	Meta      map[string]any `json:"meta,omitempty"`
	Session   string         `json:"session,omitempty"`
	UserAgent string         `json:"user_agent,omitempty"`
	// ClientVersion is the MCP client's clientInfo.version.
	ClientVersion string `json:"client_version,omitempty"`
	Conversation  string `json:"conversation,omitempty"`
	Harness       string `json:"harness,omitempty"`
	AgentToken    string `json:"agent_token,omitempty"`
	Tool          string `json:"tool,omitempty"`
	// Tasks: the client takes MCP tasks and the SDK serves the wait tool.
	Tasks bool `json:"tasks,omitempty"`
}

// WithHarness is the call with the program the agent says it runs in.
func (c Caller) WithHarness(harness string) Caller {
	c.Harness = strings.TrimSpace(harness)
	return c
}

// NoteCount is who is subscribed to a note's topic right now.
type NoteCount struct {
	Subscriptions int            `json:"subscriptions"`
	Connected     int            `json:"connected"`
	Via           map[string]int `json:"via"`
}

// NoteStatus counts the subscriptions to the note for topic and their
// connected conversations.
func NoteStatus(app *wakeshift.Client, topic string) (NoteCount, error) {
	name, args, to := noteOf(topic, nil)
	body := map[string]any{"name": name, "arguments": args}
	if to != "" {
		body["to"] = to
	}
	var out NoteCount
	err := RawApp(app, "POST", "/api/apps/v1/events/status", body, &out)
	return out, err
}

// Addition is what Wakeshift adds to a tool result for the calling
// conversation: its text (the trailer, a text beacon, included) and _meta.
type Addition struct {
	Text  string               `json:"text,omitempty"`
	Meta  map[string]any       `json:"meta,omitempty"`
	Agent wakeshift.Recognized `json:"agent"`
}

// NoteSub is a conversation's subscription to a note.
type NoteSub struct {
	ID        string
	Topic     string
	Created   bool
	ExpiresAt time.Time
	Session   string
	Addition  Addition
	// Wakes is the subscribe's wakes (the SDKs' call.wakes).
	Wakes bool
}

// SubOpts shape a subscription: once, replay, ttl, and a filter on the
// payload's branch, os or round.
type SubOpts struct {
	Once   bool
	Replay time.Duration
	TTL    time.Duration
	Filter map[string]any
}

// Note is an occurrence the app emits.
type Note struct {
	ID   string
	Text string
	Data any
}

// Published is what an emit did.
type Published struct {
	Matched   int
	Duplicate bool
}

var noteNames = []string{"note.one", "note.two", "note.three", "note.four"}

const noteFilters = `"branch":{"type":"string"},"os":{"type":"string"},"round":{"type":"integer"}`

// DefineNotes defines the note events on app (PublishManifest adds them).
func DefineNotes(t testing.TB, app *wakeshift.Client) {
	t.Helper()
	segs := []string{"a", "b", "c", "d"}
	for i, name := range noteNames {
		var props, req, topic []string
		for _, s := range segs[:i+1] {
			props = append(props, fmt.Sprintf(`%q:{"type":"string"}`, s))
			req = append(req, fmt.Sprintf("%q", s))
			topic = append(topic, "{"+s+"}")
		}
		args := `{"type":"object","properties":{` + strings.Join(props, ",") + `,` + noteFilters + `},"required":[` + strings.Join(req, ",") + `],"additionalProperties":false}`
		if _, err := wakeshift.DefineEvent(app, name, wakeshift.EventDef[map[string]any, map[string]any]{
			// Public: these tests subscribe through every path (raw calls
			// that name no tool, subscribe_events, natively), as before
			// the tools-only default.
			Description: "A test note.", Text: func(map[string]any, map[string]any) string { return "a note" }, Public: true,
			ArgsSchema: json.RawMessage(args), PayloadSchema: json.RawMessage(`{"type":"object","properties":{` + noteFilters + `}}`),
			Topic: strings.Join(topic, "/"), Filter: map[string]string{"branch": "{branch}", "os": "{os}", "round": "{round}"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	DefinePersonalNote(t, app)
}

// DefinePersonalNote defines only the personal note, note.mine, on app:
// for an app with event types of its own, which the other notes'
// all-placeholder topics ({a}/{b}) could overlap (apps.AmbiguousTopics).
func DefinePersonalNote(t testing.TB, app *wakeshift.Client) {
	t.Helper()
	// A personal note: each subscriber's own (u/<user>/<rest>).
	if _, err := wakeshift.DefineEvent(app, "note.mine", wakeshift.EventDef[map[string]any, map[string]any]{
		Description: "A test note for one subscriber.", Text: func(map[string]any, map[string]any) string { return "a note" }, Personal: true,
		ArgsSchema:    json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`),
		PayloadSchema: json.RawMessage(`{"type":"object"}`), Topic: "{a}",
	}); err != nil {
		t.Fatal(err)
	}
}

// NoteApp registers an app with the note events and nothing else in its
// manifest.
func (c *Core) NoteApp(id string) *wakeshift.Client {
	c.t.Helper()
	app := c.BareApp(id)
	DefineNotes(c.t, app)
	if err := app.PublishManifest(context.Background(), wakeshift.Manifest{Description: "Notes."}); err != nil {
		c.t.Fatal(err)
	}
	return app
}

// BareApp registers an app with no manifest at all.
func (c *Core) BareApp(id string) *wakeshift.Client {
	c.t.Helper()
	return c.App(id, "https://"+id+".test/mcp")
}

// noteOf is the note event and arguments for topic, and for a personal
// one (u/<user>/<rest>), the user it's for: note.mine {a: <rest>, with
// "." for "/"}, whose topic is apps.PersonalTopic(<rest>, <user>).
func noteOf(topic string, filter map[string]any) (name string, args map[string]any, to string) {
	if rest, ok := strings.CutPrefix(topic, "u/"); ok {
		user, rest, _ := strings.Cut(rest, "/")
		return "note.mine", map[string]any{"a": strings.ReplaceAll(rest, "/", ".")}, user
	}
	segs := strings.Split(topic, "/")
	args = map[string]any{}
	for k, v := range filter {
		args[k] = v
	}
	for i, s := range segs {
		args[string(rune('a'+i))] = s
	}
	if len(segs) > len(noteNames) {
		return "note.too-long", args, ""
	}
	return noteNames[len(segs)-1], args, ""
}

// RawApp calls the app API as app, with a fresh assertion, returning an
// *wakeshift.APIError for an error answer.
func RawApp(app *wakeshift.Client, method, path string, body, out any) error {
	return RawAppCtx(context.Background(), app, method, path, body, out)
}

// RawAppCtx is rawApp with ctx.
func RawAppCtx(ctx context.Context, app *wakeshift.Client, method, path string, body, out any) error {
	cr := CredsOf(app)
	jti := make([]byte, 16)
	rand.Read(jti)
	now := time.Now()
	// An app whose key doesn't name it (a current key) is named by its
	// key's kid, as the SDK names it until it knows the app.
	kid, iss := "", cr.ID
	if iss == "" {
		kid = appkey.KeyID(cr.Key.Public().(ed25519.PublicKey))
		iss = kid
	}
	tok, err := jws.Sign(cr.Key, kid, "app+jwt", map[string]any{"iss": iss, "sub": iss, "aud": strings.TrimSuffix(cr.Core, "/") + "/api/apps",
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti)})
	if err != nil {
		return err
	}
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(cr.Core, "/")+path, r)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		json.Unmarshal(b, &e)
		return &wakeshift.APIError{Status: resp.StatusCode, Code: wakeshift.Code(e.Error.Code), Message: e.Error.Message}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func secs(d time.Duration) any {
	if d == 0 {
		return nil
	}
	return d.Seconds()
}

// Subscribe subscribes the calling conversation to the note for topic, as
// a wrapped tool does: the subscribe defers its block, and the call asks
// for it once (/calls/addition with the pending record).
func Subscribe(app *wakeshift.Client, call Caller, topic string, o SubOpts) (NoteSub, error) {
	name, args, _ := noteOf(topic, o.Filter)
	body := map[string]any{"call": call, "name": name, "arguments": args, "once": o.Once, "defer_addition": true}
	if o.Replay > 0 {
		body["replay"] = secs(o.Replay)
	}
	if o.TTL > 0 {
		body["ttl"] = secs(o.TTL)
	}
	var out struct {
		EventSubscription struct {
			ID        string    `json:"id"`
			Created   bool      `json:"created"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"event_subscription"`
		Session string `json:"session"`
		Pending string `json:"pending"`
		Wakes   bool   `json:"wakes"`
	}
	if err := RawApp(app, "POST", "/api/apps/v1/event-subscriptions", body, &out); err != nil {
		return NoteSub{}, err
	}
	sub := NoteSub{ID: out.EventSubscription.ID, Topic: topic, Created: out.EventSubscription.Created, ExpiresAt: out.EventSubscription.ExpiresAt, Session: out.Session, Wakes: out.Wakes}
	// The block's call names the conversation the subscribe resolved, as
	// the SDK's does.
	call.Session = out.Session
	var err error
	sub.Addition, err = additionFor(app, call, out.Pending)
	return sub, err
}

// additionFor is the block of a call that subscribed (pending names its
// record) or not (pending is "").
func additionFor(app *wakeshift.Client, call Caller, pending string) (Addition, error) {
	var out struct {
		Text    string               `json:"text"`
		Trailer string               `json:"trailer"`
		Meta    map[string]any       `json:"meta"`
		Agent   wakeshift.Recognized `json:"agent"`
	}
	body := map[string]any{"call": call}
	if pending != "" {
		body["pending"] = pending
	}
	err := RawApp(app, "POST", "/api/apps/v1/calls/addition", body, &out)
	text := out.Text
	if out.Trailer != "" {
		text = strings.TrimSpace(text + "\n\n" + out.Trailer)
	}
	if err == nil && LeaksLink(text) {
		err = fmt.Errorf("coretest: a stream link outside its link step: %s", text)
	}
	return Addition{Text: text, Meta: out.Meta, Agent: out.Agent}, err
}

// LinkStepRE is the step a result has the agent write its conversation's
// link with (harnesses.LinkStep): the file's name and the link.
var LinkStepRE = regexp.MustCompile(`If you haven't written this conversation's link to the file ~/\.wakeshift/links/([0-9a-f]{16}) yet, first write it with your file-writing tool \(by its full path in your home directory, not in a folder named ~\), its whole content this one line: (\S+) \. Never write it with a shell command or put the link in one: it's this conversation's private link, and a command would show it in approvals and logs\.`)

// linkRE is a stream link: its url with a token (a command names the url
// up to the token, which isn't one).
var linkRE = regexp.MustCompile(`/v1/stream(?:/wait)?\?link=[A-Za-z0-9._~%-]`)

// ClaimRE is a claiming command's code (group 1) and the claim's URL
// (group 2): harnesses.Claim.
var ClaimRE = regexp.MustCompile(`-d 'token=(wso_[A-Za-z0-9_-]+)' --data-urlencode "conversation=\$[A-Z_]+" '([^']+)'`)

// ClaimLink claims the link a command's code hands over at url, as the
// command does (conversation: the ID it reports, "" for none): the link,
// or "" and the status of a refusal.
func ClaimLink(url, code, conversation string) (string, int) {
	resp, err := http.PostForm(url, neturl.Values{"token": {code}, "conversation": {conversation}})
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	return strings.TrimSpace(string(b)), resp.StatusCode
}

// LeaksLink reports whether text holds a stream link anywhere but in a
// link step: no command Wakeshift gives may hold one.
func LeaksLink(text string) bool {
	return linkRE.MatchString(LinkStepRE.ReplaceAllString(text, ""))
}

// Unsubscribe ends the calling conversation's subscription to the note for
// topic: how many ended (0 or 1).
func Unsubscribe(app *wakeshift.Client, call Caller, topic string) (int, error) {
	name, args, _ := noteOf(topic, nil)
	var out struct{ Ended bool }
	err := RawApp(app, "POST", "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": call, "name": name, "arguments": args}, &out)
	if out.Ended {
		return 1, err
	}
	return 0, err
}

// EmitNote emits n to the note for each topic: how many conversations it
// woke, and whether it was a repeat of an earlier emit's ID.
func EmitNote(app *wakeshift.Client, topics []string, n Note) (Published, error) {
	return EmitNoteCtx(context.Background(), app, topics, n)
}

// EmitNoteCtx is emitNote with ctx.
func EmitNoteCtx(ctx context.Context, app *wakeshift.Client, topics []string, n Note) (Published, error) {
	var p Published
	for _, topic := range topics {
		name, args, to := noteOf(topic, nil)
		body := map[string]any{"name": name, "arguments": args, "text": n.Text, "payload": map[string]any{}}
		if to != "" {
			body["to"] = to
		}
		if n.Data != nil {
			body["payload"] = n.Data
		}
		if n.ID != "" {
			body["id"] = n.ID
		}
		var out Published
		if err := RawAppCtx(ctx, app, "POST", "/api/apps/v1/events/emit", body, &out); err != nil {
			return p, err
		}
		p.Matched += out.Matched
		p.Duplicate = p.Duplicate || out.Duplicate
	}
	return p, nil
}

// SubState is one subscription right now (event.statusOf).
type SubState struct {
	Live      bool   `json:"live"`
	Connected bool   `json:"connected"`
	Via       string `json:"via"`
}

// SubStatus reads one of user's subscriptions to a note by its ID.
func SubStatus(app *wakeshift.Client, sub NoteSub, user string) (SubState, error) {
	name, _, _ := noteOf(sub.Topic, nil)
	var out struct {
		Subscriptions []SubState `json:"subscriptions"`
	}
	err := RawApp(app, "POST", "/api/apps/v1/events/status", map[string]any{"name": name,
		"subscriptions": []map[string]string{{"user": user, "id": sub.ID}}}, &out)
	if err != nil || len(out.Subscriptions) != 1 {
		return SubState{}, err
	}
	return out.Subscriptions[0], nil
}

// NotePublishURL makes a publish URL for the note for topic.
func NotePublishURL(app *wakeshift.Client, topic string) (wakeshift.PublishURL, error) {
	name, args, to := noteOf(topic, nil)
	body := map[string]any{"name": name, "arguments": args}
	if to != "" {
		body["to"] = to
	}
	var out wakeshift.PublishURL
	err := RawApp(app, "POST", "/api/apps/v1/events/publish-urls", body, &out)
	return out, err
}

// EndNote ends one of user's subscriptions by its ID, as the app does
// outside a tool call (the conversation is told).
func EndNote(app *wakeshift.Client, user, id string) (bool, error) {
	var out struct{ Ended bool }
	err := RawApp(app, "POST", "/api/apps/v1/event-subscriptions/end", map[string]any{"user": user, "id": id}, &out)
	return out.Ended, err
}

// Annotate is what Wakeshift adds to any other tool result of the app's
// for the calling conversation.
func Annotate(app *wakeshift.Client, call Caller) (Addition, error) {
	return additionFor(app, call, "")
}

// NoteRow finds an event subscription by its ID, in whichever of the
// core's workspaces holds it: the workspace, its subscriptions row, and
// whether it's live (not ended, and its subscription active and
// unexpired). It fails the test if no workspace has it.
func (c *Core) NoteRow(t testing.TB, id string) (tenant, sub string, live bool) {
	t.Helper()
	ctx := context.Background()
	rows, _ := c.DB.Pool.Query(ctx, `SELECT id::text FROM tenants`)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, tn := range tenants {
		found := false
		err := c.DB.InTenant(ctx, tn, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `SELECT es.subscription_id::text, es.ended_at IS NULL AND s.status = 'active' AND coalesce(ts.expires_at > now() AND ts.ended_at IS NULL, true)
				FROM event_subscriptions es JOIN subscriptions s ON s.tenant_id = es.tenant_id AND s.id = es.subscription_id
				LEFT JOIN topic_subscriptions ts ON ts.tenant_id = es.tenant_id AND ts.subscription_id = es.subscription_id
				WHERE es.id = $1::uuid`, id).Scan(&sub, &live)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			found = err == nil
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if found {
			return tn, sub, live
		}
	}
	t.Fatalf("no workspace has the event subscription %s", id)
	return "", "", false
}

// NoteLive says whether the event subscription id is live.
func (c *Core) NoteLive(t testing.TB, id string) bool {
	t.Helper()
	_, _, live := c.NoteRow(t, id)
	return live
}
