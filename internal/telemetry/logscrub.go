package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// allowedLog are the log attributes that may leave the process, each
// holding no credential, content or personal data once scrubbed
// (each is reviewed before it's added). Everything else is dropped, including
// keys code adds later: request paths (stream links, publish URLs and
// invite codes are credentials in URLs; "route" has the
// pattern instead), user, agent, tenant and other row IDs, emails, client
// text (User-Agent, a client's claimed harness), and any group.
var allowedLog = map[attribute.Key]bool{
	// Requests (internal/server): the method, the route's pattern, the
	// status and how long it took.
	"method": true,
	"route":  true,
	"status": true,
	"ms":     true,
	// Errors: otelslog turns an error attribute into these (the message
	// scrubbed); "err" in case one is logged as a string.
	"err":               true,
	"exception.message": true,
	"exception.type":    true,
	// Which app, harness, connector, sign-in provider or version: names
	// and public IDs, never a person's.
	"app":       true,
	"harness":   true,
	"connector": true,
	"provider":  true,
	"version":   true,
	// Outcomes, as fixed codes. Not "code": it's the name of invite and
	// sign-in codes' wildcards, too short for scrubText to catch.
	"outcome":    true,
	"reason":     true,
	"why":        true,
	"issued":     true,
	"for":        true,
	"recognized": true,
	"via":        true,
	"replay":     true,
	"full":       true,
	// Counts and durations.
	"count":         true,
	"tenants":       true,
	"webhooks":      true,
	"workspaces":    true,
	"apps_finished": true,
	"logs":          true,
	"invites":       true,
	"log_entries":   true,
	"attempt":       true,
	"attempts":      true,
	"max":           true,
	"took":          true,
	"wait":          true,
	"age_ms":        true,
	"send_ms":       true,
	"past_exp_s":    true,
}

// ScrubLogs wraps an exporter so the log records it's given are scrubbed
// first: the body (the message) as text, attributes through allowedLog
// with their strings scrubbed, the event name dropped. Severity,
// timestamps, trace and span IDs, the resource and the scope are kept.
func ScrubLogs(next sdklog.Exporter) sdklog.Exporter {
	return logScrubber{next}
}

type logScrubber struct{ next sdklog.Exporter }

func (s logScrubber) Export(ctx context.Context, records []sdklog.Record) error {
	out := make([]sdklog.Record, len(records))
	for i := range records {
		out[i] = scrubRecord(records[i])
	}
	return s.next.Export(ctx, out)
}

func (s logScrubber) Shutdown(ctx context.Context) error   { return s.next.Shutdown(ctx) }
func (s logScrubber) ForceFlush(ctx context.Context) error { return s.next.ForceFlush(ctx) }

func scrubRecord(r sdklog.Record) sdklog.Record {
	out := r.Clone()
	body := attribute.Value{}
	if b := r.Body(); b.Type() == attribute.STRING {
		body = attribute.StringValue(scrubText(b.AsString()))
	}
	out.SetBody(body)
	out.SetEventName("")
	var attrs []attribute.KeyValue
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv, ok := scrubLogAttr(kv); ok {
			attrs = append(attrs, kv)
		}
		return true
	})
	out.SetAttributes(attrs...)
	return out
}

// scrubLogAttr keeps an allowed attribute with a scalar value, its string
// scrubbed (but a route, a pattern the mux matched, as it is: scrubText
// would take a long one for a token).
func scrubLogAttr(kv attribute.KeyValue) (attribute.KeyValue, bool) {
	if !allowedLog[kv.Key] {
		return kv, false
	}
	switch kv.Value.Type() {
	case attribute.BOOL, attribute.INT64, attribute.FLOAT64:
		return kv, true
	case attribute.STRING:
		if kv.Key == "route" {
			return kv, true
		}
		return kv.Key.String(scrubText(kv.Value.AsString())), true
	}
	return kv, false
}
