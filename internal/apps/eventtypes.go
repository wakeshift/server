package apps

// Event types: what an app offers to be subscribed to, by name and
// arguments, the way MCP Events lists them. Topics stay the routing layer:
// an event type says which topic its arguments pick (Topic, a template) and
// which equality filter on the payload (Filter), so a subscription to
// chat.message {channel: "eng"} is a topic subscription to channels/eng.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// EventType is one event an app offers.
type EventType struct {
	// Name is what clients subscribe to: chat.message, game.turn.
	Name        string `json:"name"`
	Description string `json:"description"`
	// Arguments is the JSON Schema of a subscription's arguments: an
	// object. Payload is the JSON Schema of what each occurrence carries.
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	// Topic is the topic a subscription's arguments pick, with {argument}
	// placeholders for required arguments: "channels/{channel}".
	Topic string `json:"topic"`
	// Filter is the equality filter on the payload's top-level fields a
	// subscription's arguments make: {"branch": "{branch}"}. A placeholder
	// for an argument the subscription leaves out drops that field.
	Filter map[string]string `json:"filter,omitempty"`
	// Personal: each subscription belongs to its subscriber (the app's
	// user), and an occurrence is emitted to one subscriber, so it can carry
	// that subscriber's own private view. Nothing in the arguments names
	// the subscriber: Wakeshift takes it from the call, so it can't be
	// faked (PersonalTopic).
	Personal bool `json:"personal,omitempty"`
	// DeleteOnDelivery: an occurrence's content (its text and payload) is
	// deleted from Wakeshift once it's delivered, keeping only its metadata
	// (IDs, times, who it was from, where it went), for messages that
	// shouldn't be kept: deliveries.RedactDelivered. Undelivered, it
	// expires as any event does.
	DeleteOnDelivery bool `json:"delete_on_delivery,omitempty"`
	// SubscribePolicy is who may subscribe (the manifest's "subscribe"):
	// SubscribeTools, only through the app's own tools; SubscribeAny, also
	// through subscribe_events and native MCP Events (subscribe and poll).
	// Sticky: a manifest that leaves it out keeps the stored setting
	// (Registry's storeManifest fills it in), and an event never given one
	// is SubscribeAny, as every event was before the setting.
	SubscribePolicy string `json:"subscribe,omitempty"`
}

// Who may subscribe to an event (EventType.SubscribePolicy).
const (
	SubscribeTools = "tools"
	SubscribeAny   = "any"
)

// ReservedVoucherArg is the event argument a native client passes a
// subscribe voucher in: no event may declare an argument by that name.
const ReservedVoucherArg = "wakeshift_voucher"

// ToolsOnly says only the app's own tools may subscribe to e.
func (e EventType) ToolsOnly() bool { return e.SubscribePolicy == SubscribeTools }

// effectivePolicy is p, or SubscribeAny for an event never given one.
func effectivePolicy(p string) string {
	if p == "" {
		return SubscribeAny
	}
	return p
}

// ArgumentsKey is the key a subscription with these arguments is recorded
// under (Subscription.Key), without the event type: how one is found to
// end it after its event type left the manifest.
func ArgumentsKey(args json.RawMessage) (string, error) {
	args, _, err := argumentsObject(args)
	if err != nil {
		return "", err
	}
	canon, err := Canonical(args)
	if err != nil {
		return "", EventArgsError{err.Error()}
	}
	return argumentsKey(canon), nil
}

func argumentsKey(canon []byte) string {
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// PersonalTopic is where a personal event's occurrences for one subscriber
// go: topic under a private u/… prefix derived from the subscriber (the
// app's user ID). Only Wakeshift makes it: from the caller's own identity
// on subscribe, and from the emit's to.
func PersonalTopic(topic, principal string) string {
	sum := sha256.Sum256([]byte(principal))
	return "u/p" + hex.EncodeToString(sum[:15]) + "/" + topic
}

// personalPrefix is how much PersonalTopic adds ("u/p" + 30 hex + "/"), so
// a personal event's topic template leaves room for it within a topic's
// 200 characters.
const personalPrefix = 34

const (
	maxEventTypes      = 50
	maxEventSchema     = 16 << 10
	maxEventArguments  = 4 << 10
	maxEventFilter     = 10
	reservedEventSpace = "wakeshift."
)

var (
	eventNameRE   = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*){0,4}$`)
	placeholderRE = regexp.MustCompile(`\{([a-z][a-z0-9_]{0,63})\}`)
	// What a topic template may hold besides placeholders: topic characters.
	topicTemplateRE = regexp.MustCompile(`^[a-z0-9._/{}-]+$`)
	// An argument value that a topic segment can carry.
	topicValueRE = regexp.MustCompile(`^[a-z0-9._-]{1,100}$`)
)

// ErrUnknownEventType: the app offers no event by that name.
var ErrUnknownEventType = errors.New("apps: no such event type")

// UnknownEventError is ErrUnknownEventType for the event called Name.
type UnknownEventError struct{ Name string }

func (e UnknownEventError) Error() string { return "apps: no such event type: " + e.Name }

func (e UnknownEventError) Is(target error) bool { return target == ErrUnknownEventType }

// EventArgsError is what's wrong with a subscription's arguments or an
// occurrence's payload: the caller's mistake, shown to it.
type EventArgsError struct{ Msg string }

func (e EventArgsError) Error() string { return e.Msg }

// EventType is the app's event type called name.
func (m Manifest) EventType(name string) (EventType, error) {
	for _, e := range m.Events {
		if e.Name == name {
			return e, nil
		}
	}
	return EventType{}, UnknownEventError{Name: name}
}

// checkEvents validates a manifest's event types.
func checkEvents(list []EventType) error {
	if len(list) > maxEventTypes {
		return fmt.Errorf("at most %d event types", maxEventTypes)
	}
	seen := map[string]bool{}
	for _, e := range list {
		if err := e.check(); err != nil {
			return fmt.Errorf("event %q: %w", e.Name, err)
		}
		if seen[e.Name] {
			return fmt.Errorf("event %q is declared twice", e.Name)
		}
		seen[e.Name] = true
	}
	return nil
}

// AmbiguousTopicError is a manifest whose topic templates could render one
// topic for two different subscriptions. Routing is by topic alone, so each
// would get the other's occurrences (and a stricter event's authorize would
// be bypassed): refused when the manifest is published (code
// ambiguous_topic).
//
// The rule (topicShapes, topicsOverlap): argument values can hold any topic
// character but /, including . _ and -, so a segment with two placeholders
// ({org}-{job}, {a}{b}) can be split more than one way, while a segment with
// one placeholder and fixed text around it (job-{id}) can't. And two events'
// templates can render the same topic exactly when they have as many
// segments and every pair of segments can render the same text.
type AmbiguousTopicError struct {
	Event, Topic string
	// Other is the other event whose template overlaps Topic; empty when
	// Topic itself is ambiguous (two placeholders in one segment).
	Other, OtherTopic string
}

func (e AmbiguousTopicError) Error() string {
	if e.Other == "" {
		return fmt.Sprintf("event %q: topic %q is ambiguous: a topic segment holds at most one {argument} placeholder "+
			"(argument values may contain . _ and -, so two in one segment can make one topic from different arguments); "+
			"put each placeholder in a segment of its own", e.Event, e.Topic)
	}
	return fmt.Sprintf("events %q and %q: topics %q and %q can make the same topic, so each event's subscribers would get the other's occurrences; "+
		"give each event a literal segment of its own (the default topic, ev/<name>/…, has one)", e.Event, e.Other, e.Topic, e.OtherTopic)
}

// key identifies the problem, whichever order the events are in.
func (e AmbiguousTopicError) key() string {
	a, b := e.Event+" "+e.Topic, e.Other+" "+e.OtherTopic
	if e.Other != "" && b < a {
		a, b = b, a
	}
	return a + "\n" + b
}

// AmbiguousTopics lists a manifest's ambiguous topic templates: each
// template with two placeholders in one segment, and each pair of events
// whose templates can render the same topic. Event types are assumed valid
// (checkEvents).
func AmbiguousTopics(list []EventType) []AmbiguousTopicError {
	var out []AmbiguousTopicError
	shapes := make([][]topicShape, len(list))
	for i, e := range list {
		shapes[i] = topicShapes(e.Topic)
		if slices.ContainsFunc(shapes[i], func(s topicShape) bool { return s.holes > 1 }) {
			out = append(out, AmbiguousTopicError{Event: e.Name, Topic: e.Topic})
		}
	}
	for i, e := range list {
		for j := i + 1; j < len(list); j++ {
			o := list[j]
			// A personal event's topics are under u/p…/, which no other
			// event's topic can be (check), so only two personal events, or
			// two shared ones, can meet.
			if e.Personal == o.Personal && topicsOverlap(shapes[i], shapes[j]) {
				out = append(out, AmbiguousTopicError{Event: e.Name, Topic: e.Topic, Other: o.Name, OtherTopic: o.Topic})
			}
		}
	}
	return out
}

// topicShape is a template segment: fixed text before its first
// placeholder (pre) and after its last (suf), and how many placeholders
// it has (holes). A literal segment is all pre.
type topicShape struct {
	pre, suf string
	holes    int
}

func topicShapes(topic string) []topicShape {
	var out []topicShape
	for _, seg := range strings.Split(topic, "/") {
		m := placeholderRE.FindAllStringIndex(seg, -1)
		if len(m) == 0 {
			out = append(out, topicShape{pre: seg})
			continue
		}
		out = append(out, topicShape{pre: seg[:m[0][0]], suf: seg[m[len(m)-1][1]:], holes: len(m)})
	}
	return out
}

// topicsOverlap says whether two templates can render the same topic. A
// value can't hold /, so segment i of one is segment i of the other: they
// must have as many segments, and each pair must be able to match. A
// value can hold every character a template's text can, so a placeholder
// segment can be any text that starts with its pre and ends with its suf,
// with room for its placeholders (ignoring the 100-character cap, which
// only errs towards refusing).
func topicsOverlap(a, b []topicShape) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !segmentsOverlap(a[i], b[i]) {
			return false
		}
	}
	return true
}

func segmentsOverlap(a, b topicShape) bool {
	if a.holes == 0 && b.holes == 0 {
		return a.pre == b.pre
	}
	if b.holes == 0 {
		a, b = b, a
	}
	if a.holes == 0 {
		lit := a.pre
		return len(lit) >= len(b.pre)+len(b.suf)+b.holes && strings.HasPrefix(lit, b.pre) && strings.HasSuffix(lit, b.suf)
	}
	// Both have placeholders: some text starts with both pres (one is a
	// prefix of the other) and ends with both sufs, such as the longer pre,
	// then a 0, then the longer suf.
	return (strings.HasPrefix(a.pre, b.pre) || strings.HasPrefix(b.pre, a.pre)) &&
		(strings.HasSuffix(a.suf, b.suf) || strings.HasSuffix(b.suf, a.suf))
}

func (e EventType) check() error {
	switch {
	case !eventNameRE.MatchString(e.Name) || len(e.Name) > 64:
		return errors.New("a name is lowercase dot-separated words (chat.message), at most 64 characters")
	case strings.HasPrefix(e.Name, reservedEventSpace):
		return errors.New("names starting with wakeshift. are Wakeshift's")
	case strings.TrimSpace(e.Description) == "" || len(e.Description) > 500:
		return errors.New("a description is required, at most 500 characters")
	case strings.TrimSpace(e.Topic) == "":
		return errors.New("topic is required")
	case len(e.Filter) > maxEventFilter:
		return fmt.Errorf("at most %d filter fields", maxEventFilter)
	case e.SubscribePolicy != "" && e.SubscribePolicy != SubscribeTools && e.SubscribePolicy != SubscribeAny:
		return errors.New(`subscribe is "tools" (only the app's own tools subscribe) or "any"`)
	}
	args, err := resolveSchema("arguments", e.Arguments)
	if err != nil {
		return err
	}
	payload, err := resolveSchema("payload", e.Payload)
	if err != nil {
		return err
	}
	// A filter matches the payload's top-level fields: each must be one the
	// payload schema names, or a typo would match nothing, silently.
	for field := range e.Filter {
		if _, ok := payload.Schema().Properties[field]; !ok {
			return fmt.Errorf("filter field %q isn't one of the payload schema's properties", field)
		}
	}
	s := args.Schema()
	if s.Type != "object" {
		return errors.New(`the arguments schema's type is "object"`)
	}
	if _, ok := s.Properties[ReservedVoucherArg]; ok {
		return fmt.Errorf("%q is a name Wakeshift reserves (a native client passes its subscribe voucher in it): give the argument another name", ReservedVoucherArg)
	}
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	scalar := func(name string) error {
		p, ok := s.Properties[name]
		if !ok {
			return fmt.Errorf("{%s} isn't one of the arguments", name)
		}
		if p.Type != "string" && p.Type != "integer" && p.Type != "boolean" {
			return fmt.Errorf("argument %q is used in a template, so its type is string, integer or boolean", name)
		}
		return nil
	}
	if !topicTemplateRE.MatchString(e.Topic) || len(e.Topic) > 200 {
		return errors.New("topic is a topic (lowercase a-z 0-9 . _ - and /) with {argument} placeholders")
	}
	if e.Personal && len(e.Topic) > 200-personalPrefix {
		return fmt.Errorf("a personal event's topic is at most %d characters: each subscriber's topic adds %d", 200-personalPrefix, personalPrefix)
	}
	rest := placeholderRE.ReplaceAllString(e.Topic, "x")
	if strings.ContainsAny(rest, "{}") || strings.HasPrefix(rest, "/") || strings.HasSuffix(rest, "/") || strings.Contains(rest, "//") {
		return errors.New("topic has a malformed placeholder or an empty segment")
	}
	if rest == "u" || strings.HasPrefix(e.Topic, "u/") {
		return errors.New("an event's topic can't be private (u/…): it's shared by everyone with the same arguments")
	}
	for _, m := range placeholderRE.FindAllStringSubmatch(e.Topic, -1) {
		if err := scalar(m[1]); err != nil {
			return err
		}
		if !required[m[1]] {
			return fmt.Errorf("topic uses {%s}, so the arguments schema requires it", m[1])
		}
	}
	for field, tmpl := range e.Filter {
		if field == "" || len(field) > 64 {
			return errors.New("filter fields are 1-64 characters")
		}
		m := placeholderRE.FindStringSubmatch(tmpl)
		if m == nil || m[0] != tmpl {
			return fmt.Errorf("filter %q is one {argument} placeholder", field)
		}
		if err := scalar(m[1]); err != nil {
			return err
		}
	}
	return nil
}

// resolveSchema parses and resolves a JSON Schema, with no remote
// references (a schema can't make Wakeshift fetch anything).
func resolveSchema(what string, raw json.RawMessage) (*jsonschema.Resolved, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("the %s schema is required", what)
	}
	if len(raw) > maxEventSchema {
		return nil, fmt.Errorf("the %s schema is at most %d bytes", what, maxEventSchema)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("the %s schema isn't a JSON Schema: %v", what, err)
	}
	var tree any
	json.Unmarshal(raw, &tree)
	if err := checkTypes(tree); err != nil {
		return nil, fmt.Errorf("the %s schema: %v", what, err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("the %s schema: %v", what, err)
	}
	return rs, nil
}

var schemaTypes = map[string]bool{"null": true, "boolean": true, "object": true, "array": true, "number": true, "string": true, "integer": true}

// checkTypes refuses a "type" (anywhere in a schema) that names no JSON
// Schema type, which the resolver lets through.
func checkTypes(v any) error {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if k == "type" {
				names, ok := e.([]any)
				if !ok {
					names = []any{e}
				}
				for _, n := range names {
					if s, ok := n.(string); !ok || !schemaTypes[s] {
						return fmt.Errorf("%v isn't a JSON Schema type", n)
					}
				}
				continue
			}
			if k == "properties" || k == "$defs" || k == "definitions" || k == "patternProperties" {
				if m, ok := e.(map[string]any); ok {
					for _, sub := range m {
						if err := checkTypes(sub); err != nil {
							return err
						}
					}
				}
				continue
			}
			if err := checkTypes(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range t {
			if err := checkTypes(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// Subscription is what a subscription's arguments make of an event type.
type Subscription struct {
	Event     string
	Arguments []byte // canonical JSON
	Key       string // hex SHA-256 of Arguments
	Topic     string
	Filter    map[string]json.RawMessage // payload field -> value, for the topic subscription's filter
}

// Subscribe checks a subscription's arguments against the event type, and
// works out its canonical arguments, topic and filter.
func (e EventType) Subscribe(args json.RawMessage) (Subscription, error) {
	args, obj, err := argumentsObject(args)
	if err != nil {
		return Subscription{}, err
	}
	rs, err := resolveSchema("arguments", e.Arguments)
	if err != nil {
		return Subscription{}, err // validated when the manifest was published
	}
	if err := rs.Validate(jsonNumbers(any(obj))); err != nil {
		return Subscription{}, EventArgsError{"arguments don't match the event's schema: " + err.Error()}
	}
	canon, err := Canonical(args)
	if err != nil {
		return Subscription{}, EventArgsError{err.Error()}
	}
	sub := Subscription{Event: e.Name, Arguments: canon, Key: argumentsKey(canon), Filter: map[string]json.RawMessage{}}
	if sub.Topic, err = e.topicFor(obj); err != nil {
		return Subscription{}, err
	}
	for field, tmpl := range e.Filter {
		val, ok := obj[tmpl[1:len(tmpl)-1]]
		if !ok {
			continue
		}
		b, err := Canonical(mustJSON(val))
		if err != nil {
			return Subscription{}, EventArgsError{err.Error()}
		}
		sub.Filter[field] = b
	}
	return sub, nil
}

// Target is the topic an occurrence for these arguments goes to (emit).
// It needs only the topic's arguments, so it doesn't check them against
// the whole arguments schema (a subscription's other arguments filter the
// payload, which CheckPayload checks): the topic is rendered exactly as a
// subscription's is.
func (e EventType) Target(args json.RawMessage) (string, error) {
	_, obj, err := argumentsObject(args)
	if err != nil {
		return "", err
	}
	return e.topicFor(obj)
}

// argumentsObject decodes arguments, which are a JSON object (none: {}).
func argumentsObject(args json.RawMessage) (json.RawMessage, map[string]any, error) {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = json.RawMessage("{}")
	}
	if len(args) > maxEventArguments {
		return nil, nil, EventArgsError{fmt.Sprintf("arguments are at most %d bytes", maxEventArguments)}
	}
	v, err := decodeJSON(args)
	obj, ok := v.(map[string]any)
	if err != nil || !ok {
		return nil, nil, EventArgsError{"arguments are a JSON object"}
	}
	return args, obj, nil
}

// topicFor renders the topic template from arguments.
func (e EventType) topicFor(obj map[string]any) (string, error) {
	var bad error
	topic := placeholderRE.ReplaceAllStringFunc(e.Topic, func(p string) string {
		val, ok := obj[p[1:len(p)-1]]
		s, valid := topicSegment(val)
		if (!ok || !valid) && bad == nil {
			bad = EventArgsError{fmt.Sprintf("argument %q is required and must be lowercase a-z 0-9 . _ - (at most 100 characters): it names a topic", p[1:len(p)-1])}
		}
		return s
	})
	return topic, bad
}

// topicSegment is an argument's value in a topic. A number is in its
// canonical form, as in the arguments' key: 1, 1.0 and 1e0 are one
// subscription, so they're one topic (a subscription made with 1.0 hears
// an emit of 1).
func topicSegment(v any) (string, bool) {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case json.Number:
		n, err := canonicalNumber(t.String())
		if err != nil {
			return "", false
		}
		s = n
	case bool:
		s = strconv.FormatBool(t)
	default:
		return "", false
	}
	return s, topicValueRE.MatchString(s)
}

// CheckPayload checks an occurrence's payload against the event type.
func (e EventType) CheckPayload(payload json.RawMessage) error {
	rs, err := resolveSchema("payload", e.Payload)
	if err != nil {
		return err
	}
	v, err := decodeJSON(payload)
	if err != nil {
		return EventArgsError{"the payload is JSON"}
	}
	if err := rs.Validate(jsonNumbers(v)); err != nil {
		return EventArgsError{"the payload doesn't match the event's schema: " + err.Error()}
	}
	return nil
}

// Canonical is JSON's one canonical form: objects with their keys sorted,
// no insignificant space, numbers in their shortest form (1, 1.0 and 1e0
// are all 1). Two subscriptions with equal arguments have equal canonical
// arguments, whatever the client sent.
func Canonical(raw json.RawMessage) ([]byte, error) {
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, errors.New("not JSON")
	}
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func decodeJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data")
	}
	return v, nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case json.Number:
		s, err := canonicalNumber(t.String())
		if err != nil {
			return err
		}
		b.WriteString(s)
	case string:
		s, _ := json.Marshal(t)
		b.Write(s)
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			s, _ := json.Marshal(k)
			b.Write(s)
			b.WriteByte(':')
			if err := writeCanonical(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unexpected %T", v)
	}
	return nil
}

// canonicalNumber is a JSON number's exact value in one form: an integer
// in digits (1, 1.0 and 1e0 are 1; -0 is 0), anything else as the shortest
// plain decimal that is exactly it (0.10 is 0.1). No two different values
// share a form, however large or precise.
func canonicalNumber(s string) (string, error) {
	mant, exp, _ := strings.Cut(strings.ToLower(s), "e")
	if len(mant) > 100 {
		return "", fmt.Errorf("number %s is too long", s)
	}
	if exp != "" {
		e, err := strconv.Atoi(exp)
		if err != nil || e < -400 || e > 400 {
			return "", fmt.Errorf("number %s is out of range", s)
		}
	}
	// Beyond float64's range, where validators (and most clients) can't
	// read it, it's refused, not kept.
	if f, err := strconv.ParseFloat(s, 64); err != nil && !errors.Is(err, strconv.ErrRange) || f > 1.7976931348623157e308 || f < -1.7976931348623157e308 {
		return "", fmt.Errorf("number %s is out of range", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", fmt.Errorf("number %s isn't a number", s)
	}
	if r.IsInt() {
		return r.Num().String(), nil
	}
	// A finite decimal's denominator is 2^a·5^b: max(a, b) digits after
	// the point are exact.
	d := new(big.Int).Set(r.Denom())
	digits := 0
	for _, p := range []int64{2, 5} {
		n, q, m := 0, new(big.Int), new(big.Int)
		for {
			q.QuoRem(d, big.NewInt(p), m)
			if m.Sign() != 0 {
				break
			}
			d.Set(q)
			n++
		}
		digits = max(digits, n)
	}
	out := strings.TrimRight(r.FloatString(digits), "0")
	return strings.TrimSuffix(out, "."), nil
}

// jsonNumbers turns json.Numbers into float64s, which the schema
// validator expects (integers keep integral values).
func jsonNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		f, _ := strconv.ParseFloat(t.String(), 64)
		return f
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = jsonNumbers(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = jsonNumbers(e)
		}
		return out
	}
	return v
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
