package events_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/events"
)

func slackMessage(id, text string) events.Event {
	return events.Event{
		Type: "slack.message", Source: "slack", SourceEventID: id,
		From: events.Party{Kind: "slack_user", ID: "U1"}, Subject: text, Trust: events.External,
		Attrs: map[string]string{"channel": "C1"},
	}
}

func appendOne(t *testing.T, d *db.DB, tenant string, e events.Event) (events.Event, bool) {
	t.Helper()
	var out events.Event
	var inserted bool
	err := d.InTenant(context.Background(), tenant, func(tx pgx.Tx) (err error) {
		out, inserted, err = events.Append(context.Background(), tx, e)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out, inserted
}

func readAfter(t *testing.T, d *db.DB, tenant string, after uint64, patterns ...string) []events.Event {
	t.Helper()
	var out []events.Event
	err := d.InTenant(context.Background(), tenant, func(tx pgx.Tx) (err error) {
		out, err = events.ReadAfter(context.Background(), tx, after, patterns, 1000)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAppendAndRead(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)

	first, inserted := appendOne(t, d, tenant, slackMessage("Ev1", "hello"))
	if !inserted || first.Seq != 1 || first.TraceID != first.ID || first.Attrs["channel"] != "C1" {
		t.Fatalf("first = %+v", first)
	}
	chat := events.Event{
		Type: "app.agentchat.channel.pr-482", Source: "app:agentchat", From: events.Party{Kind: "agent", ID: "codex"},
		Data: json.RawMessage(`{"text":"@claude-code look"}`), Mentions: []string{"claude-code"},
		TraceID: first.TraceID, ParentID: first.ID, Hop: 1, Trust: events.Internal,
	}
	second, _ := appendOne(t, d, tenant, chat)
	if second.Seq != 2 || second.ParentID != first.ID || second.TraceID != first.TraceID || second.Mentions[0] != "claude-code" {
		t.Fatalf("second = %+v", second)
	}

	if got := readAfter(t, d, tenant, 0); len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("read all = %v", got)
	}
	if got := readAfter(t, d, tenant, 1); len(got) != 1 || string(got[0].Data) != `{"text": "@claude-code look"}` {
		t.Fatalf("read after 1 = %+v", got)
	}
	if got := readAfter(t, d, tenant, 0, "app.agentchat.channel.*"); len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("wildcard = %v", got)
	}
	if got := readAfter(t, d, tenant, 0, "slack.message"); len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("exact = %v", got)
	}
	// LIKE metacharacters in a pattern are literal.
	if got := readAfter(t, d, tenant, 0, "slack_message"); len(got) != 0 {
		t.Fatalf("underscore matched %v", got)
	}
}

func TestRetriedSourceEventsAreStoredOnce(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)

	a, _ := appendOne(t, d, tenant, slackMessage("Ev1", "hello"))
	again, inserted := appendOne(t, d, tenant, slackMessage("Ev1", "hello"))
	if inserted || again.ID != a.ID {
		t.Fatalf("retry stored again: %+v", again)
	}
	edit := slackMessage("Ev1", "hello (edited)")
	edit.SourceRevision = "1739999999.000200"
	if e, inserted := appendOne(t, d, tenant, edit); !inserted || e.ID == a.ID {
		t.Fatal("an edit (new revision) must be kept")
	}
	if n := len(readAfter(t, d, tenant, 0)); n != 2 {
		t.Fatalf("%d events", n)
	}
}

func TestEventsAreTenantScoped(t *testing.T) {
	d := dbtest.New(t)
	a, b := dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)
	appendOne(t, d, a, slackMessage("Ev1", "for a"))
	// The same source event ID in another tenant is a different event, and
	// each tenant has its own sequence.
	if e, inserted := appendOne(t, d, b, slackMessage("Ev1", "for b")); !inserted || e.Seq != 1 {
		t.Fatalf("tenant b: %+v", e)
	}
	if got := readAfter(t, d, b, 0); len(got) != 1 || got[0].Subject != "for b" {
		t.Fatalf("tenant b reads %v", got)
	}
}

// A reader that advances its cursor past each event it sees never misses
// one, however writers' transactions interleave. This is the property the
// pipeline runner relies on.
func TestReaderNeverSkipsEvents(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	ctx := context.Background()
	const writers, perWriter = 8, 25

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
					// Some work before and after taking the sequence, so
					// transactions overlap in every order.
					time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
					if _, _, err := events.Append(ctx, tx, slackMessage(fmt.Sprintf("w%d-%d", w, i), "x")); err != nil {
						return err
					}
					time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
					return nil
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	seen := map[string]bool{}
	var cursor uint64
	for finished := false; !finished; {
		select {
		case <-done:
			finished = true // one more read after the last commit
		default:
		}
		for _, e := range readAfter(t, d, tenant, cursor) {
			if e.Seq != cursor+1 {
				t.Fatalf("gap: cursor %d, next %d", cursor, e.Seq)
			}
			if seen[e.SourceEventID] {
				t.Fatalf("%s read twice", e.SourceEventID)
			}
			seen[e.SourceEventID] = true
			cursor = e.Seq
		}
	}
	if len(seen) != writers*perWriter {
		t.Fatalf("reader saw %d of %d events", len(seen), writers*perWriter)
	}
}

func TestMatchType(t *testing.T) {
	for _, c := range []struct {
		pattern, typ string
		want         bool
	}{
		{"slack.message", "slack.message", true},
		{"slack.message", "slack.message.changed", false},
		{"app.agentchat.channel.*", "app.agentchat.channel.pr-482", true},
		{"app.agentchat.channel.*", "app.agentchat.other", false},
		{"*", "anything", true},
	} {
		if got := events.MatchType(c.pattern, c.typ); got != c.want {
			t.Errorf("MatchType(%q, %q) = %v", c.pattern, c.typ, got)
		}
	}
}
