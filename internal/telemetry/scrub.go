package telemetry

import (
	"context"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// allowed are the span and event attributes that may leave the process,
// each holding no credential or content once scrubbed. Everything else is
// dropped: url.path and url.query (stream links, publish URLs and invite
// codes are credentials in URLs; http.route has the pattern instead),
// client and peer addresses (personal data), user agents and headers.
var allowed = map[attribute.Key]bool{
	"http.request.method":       true,
	"http.response.status_code": true,
	"http.route":                true,
	"http.request.body.size":    true,
	"http.response.body.size":   true,
	"server.address":            true,
	"server.port":               true,
	"url.scheme":                true,
	"network.protocol.name":     true,
	"network.protocol.version":  true,
	"error.type":                true,
	// url.full keeps only its origin (scrubText).
	"url.full": true,
	// Exception events (span.RecordError), the message scrubbed. Not
	// exception.stacktrace: it isn't scrubbed, and routes and messages say
	// enough.
	"exception.type":    true,
	"exception.message": true,
	"exception.escaped": true,
}

var (
	// urlRE finds URLs in text; scrubText keeps their scheme and host.
	urlRE = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)
	// quotedRE finds quoted values (Go's %q, Postgres's errors quoting the
	// input they refused).
	quotedRE = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	emailRE  = regexp.MustCompile(`[^\s@"'<>()]+@[^\s@"'<>()]+\.[a-zA-Z]{2,}`)
	// tokenRE finds long runs that could be a token or key; UUIDs (IDs, not
	// secrets) are put back.
	tokenRE = regexp.MustCompile(`[A-Za-z0-9_\-.~+/=]{24,}`)
	// ipRE finds what could be an IPv4 or IPv6 address (net.ParseIP
	// decides): a client's address is personal data, and the standard
	// library's own log lines (an http.Server's) can name one.
	ipRE   = regexp.MustCompile(`\b[0-9]{1,3}(?:\.[0-9]{1,3}){3}\b|[0-9a-fA-F]{0,4}(?::[0-9a-fA-F]{0,4}){2,7}(?:%[0-9A-Za-z]+)?`)
	uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// placeholderRE is a URL set aside by scrubText; onlyPlaceholderRE a
	// quoted value that's just one.
	placeholderRE     = regexp.MustCompile("\x00[0-9]+\x00")
	onlyPlaceholderRE = regexp.MustCompile("^\x00[0-9]+\x00$")
)

// scrubText removes what text may carry that mustn't leave: a URL becomes
// its origin, a quoted value "…" (unless it's just a URL), an email [email],
// an IP address [ip], and a long token-like run [redacted].
func scrubText(s string) string {
	// URLs become their origins and are set aside, so the steps below
	// neither redact a long hostname nor quote away a quoted URL's host.
	var origins []string
	s = strings.ReplaceAll(s, "\x00", "")
	s = urlRE.ReplaceAllStringFunc(s, func(m string) string {
		origin := "[url]"
		if u, err := url.Parse(m); err == nil && u.Host != "" {
			host := u.Hostname()
			if net.ParseIP(host) != nil {
				host = "[ip]" // an address is personal data, in a URL too
			}
			origin = u.Scheme + "://" + host + portOf(u)
		}
		origins = append(origins, origin)
		return "\x00" + strconv.Itoa(len(origins)-1) + "\x00"
	})
	s = quotedRE.ReplaceAllStringFunc(s, func(m string) string {
		if onlyPlaceholderRE.MatchString(m[1 : len(m)-1]) {
			return m
		}
		return `"…"`
	})
	s = emailRE.ReplaceAllString(s, "[email]")
	s = ipRE.ReplaceAllStringFunc(s, func(m string) string {
		ip, _, _ := strings.Cut(m, "%")
		if net.ParseIP(ip) == nil {
			return m
		}
		return "[ip]"
	})
	s = tokenRE.ReplaceAllStringFunc(s, func(m string) string {
		if uuidRE.MatchString(m) {
			return m
		}
		return "[redacted]"
	})
	return placeholderRE.ReplaceAllStringFunc(s, func(m string) string {
		i, _ := strconv.Atoi(m[1 : len(m)-1])
		return origins[i]
	})
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return ":" + p
	}
	return ""
}

// scrubAttrs keeps the allowed attributes, their strings scrubbed.
func scrubAttrs(in []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(in))
	for _, kv := range in {
		if !allowed[kv.Key] {
			continue
		}
		if kv.Value.Type() == attribute.STRING && kv.Key != "http.route" {
			kv = kv.Key.String(scrubText(kv.Value.AsString()))
		}
		out = append(out, kv)
	}
	return out
}

// Scrub wraps an exporter so the spans it's given are scrubbed first: names
// and status descriptions as text, attributes (the span's, its events' and
// its links') through the allowlist.
func Scrub(next sdktrace.SpanExporter) sdktrace.SpanExporter {
	return scrubber{next}
}

type scrubber struct{ next sdktrace.SpanExporter }

func (s scrubber) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, sp := range spans {
		out[i] = scrub(sp)
	}
	return s.next.ExportSpans(ctx, out)
}

func (s scrubber) Shutdown(ctx context.Context) error { return s.next.Shutdown(ctx) }

func scrub(sp sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	events := make([]sdktrace.Event, len(sp.Events()))
	for i, e := range sp.Events() {
		e.Name = scrubText(e.Name)
		e.Attributes = scrubAttrs(e.Attributes)
		events[i] = e
	}
	links := make([]sdktrace.Link, len(sp.Links()))
	for i, l := range sp.Links() {
		l.Attributes = scrubAttrs(l.Attributes)
		// A link to a caller's trace keeps its IDs, not its tracestate:
		// that's text the caller chose.
		l.SpanContext = l.SpanContext.WithTraceState(trace.TraceState{})
		links[i] = l
	}
	status := sp.Status()
	status.Description = scrubText(status.Description)
	return scrubbedSpan{ReadOnlySpan: sp, name: scrubName(sp), attrs: scrubAttrs(sp.Attributes()), events: events, links: links, status: status}
}

// scrubName keeps a span's name if it's its method and route ("POST
// /v1/publish/{token}", as otelhttp names them) or plain words
// ("pipeline.run"). A name that could hold a path or query is cut to its
// first word: scrubText can't catch every credential in one (an invite
// code is 12 characters).
func scrubName(sp sdktrace.ReadOnlySpan) string {
	name := sp.Name()
	for _, kv := range sp.Attributes() {
		if kv.Key == "http.route" && strings.HasSuffix(name, " "+kv.Value.AsString()) && !strings.Contains(strings.TrimSuffix(name, " "+kv.Value.AsString()), " ") {
			return name
		}
	}
	if !strings.ContainsAny(name, "/?=@&#:") {
		return name
	}
	first, _, _ := strings.Cut(name, " ")
	if strings.ContainsAny(first, "/?=@&#:") {
		return "span"
	}
	return first
}

// scrubbedSpan is a span with its name, attributes, events, links and
// status replaced.
type scrubbedSpan struct {
	sdktrace.ReadOnlySpan
	name   string
	attrs  []attribute.KeyValue
	events []sdktrace.Event
	links  []sdktrace.Link
	status sdktrace.Status
}

func (s scrubbedSpan) Name() string                     { return s.name }
func (s scrubbedSpan) Attributes() []attribute.KeyValue { return s.attrs }
func (s scrubbedSpan) Events() []sdktrace.Event         { return s.events }
func (s scrubbedSpan) Links() []sdktrace.Link           { return s.links }
func (s scrubbedSpan) Status() sdktrace.Status          { return s.status }
