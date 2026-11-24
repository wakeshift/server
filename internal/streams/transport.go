package streams

// A stream is read three ways, all with the same claiming, leases,
// rotation, takeover and revocation:
//
//   - WebSocket (GET /v1/stream with an upgrade): frames as text messages,
//     acks on the socket, keepalive in ping frames.
//   - Server-Sent Events (Accept: text/event-stream, or ?format=sse): each
//     frame is an event, "id: <delivery_id>", "event: <frame type>",
//     "data: <the frame's JSON>"; a comment line (": keepalive") every
//     PingEvery. For browsers and EventSource clients.
//   - Lines (?format=lines): each frame is one JSON line (an event as a
//     compact line, monitorLine, with its delivery_id only on a link made
//     with ack=true); the keepalive is
//     an empty line every PingEvery, which Claude Code's Monitor skips (it
//     trims each line and drops empty ones, so only events wake the
//     agent). For Monitor: curl -sN 'https://…/v1/stream?link=…&format=lines'.
//
// A one-way stream with ?max=N ends after N events, once they're confirmed
// (on a link with acks, acknowledged, or after a lease without), with the
// next link (hub.rotate, same max): for a command whose harness
// wakes its agent on output and stops listening after a while (Cursor's
// notify_on_output, 100 matches). Its holder is a background command's,
// which hooks' polls leave alone (poll.go). The one-shot wait is wait.go.
//
// SSE and lines are one-way. On a link made with ack=true, deliveries stay
// leased until acked through POST /v1/stream/ack with the same link (the
// long poll's ack endpoint), and come back on reconnect if not. Without
// acks, a delivery counts as delivered only once the connection has
// outlived it: a keepalive written at least PingEvery after it went through
// (streams.go explains why). An SSE reconnect that sends Last-Event-ID
// (EventSource does by itself) confirms the event it names and gets again
// what was written after that, and anything else unconfirmed.
//
// The stream ends with a last frame saying why (SSE event "closed", or a
// line): {"type":"hub.closed","reason":…,"note":…}, the note in words for
// the agent: whether there's anything more to do. Reasons: "replaced",
// "unsubscribed", "closed" (on subscriptions that ended before 0.4.0, when
// an app could close a topic), "ttl", "once",
// "removed" (the app ended the subscription, or removed its user),
// "forgotten" (the app forgot the user: their workspace is gone), "revoked",
// "expired" (only for a link that expired before it connected), "renewed"
// (a link that expired and was renewed: grace.go), "error".
// When the client should carry on (the connection's age limit, a server
// restart), a session's stream sends {"type":"hub.rotate","url":…,"link":…}
// instead, with the next URL in the same form, and ends without hub.closed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/wakeshift/server/internal/framing"
)

// endReason is how a stream is ending (for a WebSocket: its close code).
type endReason int

const (
	endError      endReason = iota // try again
	endReplaced                    // a newer connection took it
	endRevoked                     // the link no longer works, or there's nothing left to receive
	endRestarting                  // the server is draining
	endReconnect                   // the connection's age limit
	endRotated                     // hub.rotate was sent
	endTooManyAcks
	endUpdateRequired // the connector is older than Wakeshift serves now
)

// Ending is a stream's last frame, {"type":"hub.closed","reason","note"}:
// why it ended, and in words for the agent, what (if anything) to do next.
type Ending struct {
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

// Endings for ends that don't depend on the link.
var (
	endingReplaced   = Ending{"replaced", "A newer connection took this stream, so this one stopped. Nothing to do here: the newer one gets the messages."}
	endingRevoked    = Ending{"revoked", "This stream link no longer works: access to it was revoked. Nothing more to do here unless the user asks you to watch again."}
	endingForgotten  = Ending{"forgotten", "The app ended its subscriptions for this user, so this stream has ended. Nothing more to do."}
	endingExpired    = Ending{"expired", "This stream link expired before it connected, so this command can't wait for messages any more. Tell the user that notifications for this conversation have stopped, and that they can ask you to start watching when they want them back."}
	endingRenewed    = Ending{"renewed", "This link expired and was renewed, and only its newest url works now. If this command printed a hub.restart line with a url, start it again with that url instead; otherwise tell the user that notifications for this conversation have stopped, and that they can ask you to start watching when they want them back."}
	endingRestarting = Ending{"restarting", "The server is restarting: connect again with the same command."}
	endingReconnect  = Ending{"reconnect", "Connection age limit: connect again with the same command."}
	endingTooMany    = Ending{"error", "Too many acks."}
	endingError      = Ending{"error", "Something went wrong: connect again with the same command."}
)

// transport is how a stream's frames reach its client.
type transport interface {
	send(ctx context.Context, frame any, id string) error
	ping(ctx context.Context) error // keepalive
	end(reason endReason, why Ending)
	// url is a link's stream URL (wss://…) in this transport's form.
	url(stream string) string
}

type wsTransport struct{ c *websocket.Conn }

func (t wsTransport) send(ctx context.Context, frame any, _ string) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return t.c.Write(wctx, websocket.MessageText, b)
}

// ping waits for the client's pong: proof it's still reading.
func (t wsTransport) ping(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return t.c.Ping(pctx)
}

// end closes with the kind's code; the close reason is the Ending's reason
// (a close reason is limited to 123 bytes).
func (t wsTransport) end(reason endReason, why Ending) {
	switch reason {
	case endReplaced:
		t.c.Close(CodeReplaced, "replaced by a newer connection")
	case endRevoked:
		t.c.Close(CodeRevoked, why.Reason)
	case endRestarting:
		t.c.Close(websocket.StatusServiceRestart, "server restarting; reconnect with the same link")
	case endReconnect:
		t.c.Close(websocket.StatusTryAgainLater, "connection age limit; reconnect with the same link")
	case endRotated:
		t.c.Close(websocket.StatusNormalClosure, "rotated; reconnect with the url from hub.rotate")
	case endTooManyAcks:
		t.c.Close(websocket.StatusPolicyViolation, "too many acks")
	case endUpdateRequired:
		t.c.Close(CodeUpdateRequired, why.Reason)
	default:
		t.c.Close(websocket.StatusTryAgainLater, "try again")
	}
}

func (wsTransport) url(stream string) string { return stream }

// Formats of a one-way stream (?format=).
const (
	FormatSSE   = "sse"
	FormatLines = "lines"
)

// httpTransport is SSE or lines on a plain HTTP response.
type httpTransport struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	format string
	max    int // the stream's ?max=, kept in the URLs it hands over
	// acks: the link takes acks by delivery ID (ack=true), so its lines
	// carry them.
	acks bool
}

func newHTTPTransport(w http.ResponseWriter, format string) *httpTransport {
	h := w.Header()
	if format == FormatSSE {
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
	} else {
		h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
	}
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // proxies: don't buffer
	w.WriteHeader(http.StatusOK)
	t := &httpTransport{w: w, rc: http.NewResponseController(w), format: format}
	// Something at once, not just the headers: a proxy in front (Cloud
	// Run's) may hold the headers until the first bytes, and a client
	// counts as connected only once it has them. For lines an empty line,
	// which Monitor and the plugins skip, as they do the keepalive.
	if format == FormatSSE {
		t.write("retry: 3000\n\n")
	} else {
		t.write("\n")
	}
	return t
}

func (t *httpTransport) write(s string) error {
	t.rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := t.w.Write([]byte(s)); err != nil {
		return err
	}
	return t.rc.Flush()
}

func (t *httpTransport) send(_ context.Context, frame any, id string) error {
	if t.format == FormatLines {
		if f, ok := frame.(Frame); ok {
			return t.write(string(monitorLine(f, t.acks)) + "\n")
		}
	}
	b, err := marshalRaw(frame)
	if err != nil {
		return err
	}
	if t.format != FormatSSE {
		return t.write(string(b) + "\n")
	}
	typ := "message"
	var head struct{ Type string }
	if json.Unmarshal(b, &head) == nil && head.Type != "" {
		typ = head.Type
	}
	ev := "event: " + typ + "\n"
	if id != "" {
		ev = "id: " + id + "\n" + ev
	}
	return t.write(ev + "data: " + string(b) + "\n\n")
}

// ping writes a keepalive: an SSE comment, or for lines an empty line
// (Monitor skips it). Writing is what finds out a client is gone: a proxy
// in front notices only when it writes to it, and only then closes its
// connection to us.
func (t *httpTransport) ping(context.Context) error {
	if t.format == FormatSSE {
		return t.write(": keepalive\n\n")
	}
	return t.write("\n")
}

func (t *httpTransport) end(reason endReason, why Ending) {
	if reason == endRotated {
		return // hub.rotate said it
	}
	t.send(context.Background(), closedFrame(why), "")
}

// marshalRaw is json.Marshal without HTML escaping: a URL in a line keeps
// its & (an agent copies it from the line as it is).
func marshalRaw(v any) ([]byte, error) {
	var b strings.Builder
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(b.String(), "\n")), nil
}

// rotateFrame is {"type":"hub.rotate"}, type and url first (a shell loop
// reads the url from the start of the line).
type rotateFrame struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Link      string `json:"link"`
	ExpiresAt string `json:"expires_at"`
	Note      string `json:"note"`
}

// closedFrame is a stream's last frame, type first.
func closedFrame(why Ending) any {
	return struct {
		Type string `json:"type"`
		Ending
	}{"hub.closed", why}
}

func (t *httpTransport) url(stream string) string {
	u := HTTPStreamURL(stream, t.format)
	if t.max > 0 {
		u += "&max=" + strconv.Itoa(t.max)
	}
	return u
}

// MaxEvents is the most a one-way stream's ?max= may be.
const MaxEvents = 1000

// streamMax is a one-way stream's ?max=: end after this many events (0:
// no limit). For a command that wakes its agent on output (Cursor's Shell
// with notify_on_output, which stops after 100 matches): it exits first,
// and the exit wakes the agent to start it again with the url the
// stream's hub.rotate line gives.
func streamMax(r *http.Request, format string) (int, error) {
	v := r.URL.Query().Get("max")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > MaxEvents {
		return 0, fmt.Errorf("max is 1 to %d", MaxEvents)
	}
	if format == "" {
		return 0, errors.New("max is for format=lines or format=sse")
	}
	return n, nil
}

// HTTPStreamURL is a stream URL (wss://…/v1/stream?link=…) for a one-way
// read: https, with ?format= (FormatSSE or FormatLines).
func HTTPStreamURL(stream, format string) string {
	u, err := url.Parse(stream)
	if err != nil {
		return stream
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	q := u.Query()
	q.Set("format", format)
	u.RawQuery = q.Encode()
	return u.String()
}

// streamFormat is the one-way format a request asks for, "" for a
// WebSocket.
func streamFormat(r *http.Request) (string, error) {
	switch f := r.URL.Query().Get("format"); f {
	case FormatSSE, FormatLines:
		return f, nil
	case "", "ws", "websocket":
		if f == "" && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			return FormatSSE, nil
		}
		return "", nil
	default:
		return "", fmt.Errorf("format is %s or %s (or leave it out for a WebSocket)", FormatSSE, FormatLines)
	}
}

// MonitorLineMax is how long a lines-mode event line may be. Claude Code's
// Monitor shows about the first 500 characters of a line, so a line puts
// what the agent needs first and fits in that.
const MonitorLineMax = 480

// MinPreview is how much of the data's JSON, in bytes, a shortened line
// keeps as its data_preview while the body or subject can be cut instead.
const MinPreview = 40

// outsideMark is how a Monitor line says its content isn't the user's
// (the hooks' and tool results' <wakeshift_message trust="external"> tag,
// in a few bytes): every line but Wakeshift's own notices carries it, since
// the watch result that explained it may be hours back.
const outsideMark = "outside data, not from the user"

// monitorLine is an event as lines mode writes it: compact, the topic,
// who it's from and that it's outside data first, then what it says, then
// the event's ID (for replies; redelivered if it may have been seen
// before). The delivery ID only on a link that acks by it (acks): Monitor
// and the wait don't, and a UUID costs a ninth of the line. When the whole
// event doesn't fit, the data becomes a preview and truncated is set.
// MonitorLine is the line a one-shot wait prints for a delivery (its
// format=lines), without its delivery ID: an MCP wait task's result lists
// its messages the same way.
func MonitorLine(f Frame) []byte { return monitorLine(f, false) }

// EventLine is MonitorLine with its own limit: the whole event while it
// fits in max bytes, else the same truncated form cut to fit max (an
// agent-hosted webhook's body: internal/agenthooks).
func EventLine(f Frame, max int) []byte { return eventLine(f, false, max) }

func monitorLine(f Frame, acks bool) []byte { return eventLine(f, acks, MonitorLineMax) }

func eventLine(f Frame, acks bool, max int) []byte {
	e := f.Event
	handle := e.From.ID
	if e.FromHandle != "" {
		handle = e.FromHandle
	}
	type line struct {
		Type        string          `json:"type"`
		Topic       string          `json:"topic,omitempty"`
		From        string          `json:"from"`
		App         string          `json:"app,omitempty"`
		Trust       string          `json:"trust,omitempty"`
		Subject     string          `json:"subject,omitempty"`
		Body        string          `json:"body,omitempty"`
		Data        json.RawMessage `json:"data,omitempty"`
		Preview     string          `json:"data_preview,omitempty"`
		Mentions    []string        `json:"mentions,omitempty"`
		ReplyTo     string          `json:"parent_id,omitempty"`
		Truncated   bool            `json:"truncated,omitempty"`
		ID          string          `json:"id"`
		DeliveryID  string          `json:"delivery_id,omitempty"`
		Redelivered bool            `json:"redelivered,omitempty"`
	}
	l := line{Type: "event", Topic: f.Topic, From: framing.Sender(e.From.Kind, handle), App: f.App, Subject: e.Subject, Body: e.Body, Data: e.Data,
		Mentions: e.Mentions, ReplyTo: e.ParentID, ID: e.ID, Redelivered: f.Attempt > 1}
	if acks {
		l.DeliveryID = f.DeliveryID
	}
	if e.From.Kind != "hub" {
		l.Trust = outsideMark
	}
	b, _ := json.Marshal(l)
	if len(b) <= max {
		return b
	}
	// Too long: the data becomes a preview, cut first only as far as
	// MinPreview, so a line with long text still shows the start of the
	// payload; then the body and the subject are cut, and only then the
	// rest of the preview, until the line fits (still valid JSON).
	// If the rest is still too long (a long sender, topic or IDs, many
	// mentions), mentions go from the end, then the parent ID, then the
	// sender, topic, app and IDs are cut in that order: a line never
	// exceeds max.
	l.Truncated, l.Data, l.Preview = true, nil, string(e.Data)
	fits := func() bool {
		b, _ = json.Marshal(l)
		return len(b) <= max
	}
	cut := func(field *string) bool {
		for !fits() && *field != "" {
			*field = shorten(*field, len(b)-max)
		}
		return len(b) <= max
	}
	// The preview down to MinPreview bytes (and its "…") at most.
	for !fits() && len(strings.TrimSuffix(l.Preview, "…")) > MinPreview {
		keep := len(strings.TrimSuffix(l.Preview, "…")) - (len(b) - max) - len("…")
		if keep < MinPreview {
			keep = MinPreview
		}
		l.Preview = clipUTF8(l.Preview, keep) + "…"
	}
	if len(b) <= max {
		return b
	}
	for _, field := range []*string{&l.Body, &l.Subject, &l.Preview} {
		if cut(field) {
			return b
		}
	}
	for len(l.Mentions) > 0 && !fits() {
		l.Mentions = l.Mentions[:len(l.Mentions)-1]
	}
	if fits() {
		return b
	}
	l.ReplyTo = ""
	for _, field := range []*string{&l.From, &l.Topic, &l.App, &l.ID, &l.DeliveryID} {
		if cut(field) {
			return b
		}
	}
	return b // only the fixed fields are left, well within the max
}

// shorten cuts s by at least over bytes (more, for the "…" it adds), or to
// nothing once little is left.
func shorten(s string, over int) string {
	n := len(strings.TrimSuffix(s, "…")) - over - len("…")
	if n < 8 {
		return ""
	}
	return clipUTF8(s, n) + "…"
}

// clipUTF8 is s cut to at most n bytes, at a character boundary.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
