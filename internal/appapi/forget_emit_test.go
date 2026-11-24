package appapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/dbtest"
)

// Forgetting a user deletes the app's recent emits addressed to them. A
// multi-recipient emit that names them as a direct recipient (a
// subscription, or native) records them only in its recipients, not its
// topics: its record, with their user ID and the payload meant for them,
// must go too.
func TestForgetUserDeletesDirectEmitsToThem(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('quizzer', 'Quizzer')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('bob', 'app', 'quizzer', 'bob')`); err != nil {
		t.Fatal(err)
	}
	a := &API{DB: d}
	app := apps.App{ID: "quizzer"}
	et := apps.EventType{Name: "dm", Topic: "dm", Personal: true}
	list := []recipient{{User: "bob", Subscription: "00000000-0000-4000-8000-000000000001"}, {User: "bob", Native: true}}
	ev := topicEvent{Text: "for bob only", Data: json.RawMessage(`{"secret":"bob's"}`), event: "dm"}
	if _, err := a.emitToMany(ctx, app, et, "dm", list, ev); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ForgetUser(ctx, "quizzer", "bob", "app:quizzer"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_events WHERE app_id = 'quizzer' AND (recipients::text LIKE '%"bob"%' OR text LIKE '%bob%')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d emit record(s) naming the forgotten user (their ID and payload) remain in topic_events", left)
	}
}

// Forgetting one recipient of a multi-recipient emit must not take the
// other recipients' copies still to be made: deleting the whole record
// cascades to its topic_fanout rows, so alice's copy is never made.
func TestForgetUserKeepsOtherRecipientsCopies(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('quizzer', 'Quizzer')`); err != nil {
		t.Fatal(err)
	}
	var alice string
	if err := d.Pool.QueryRow(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('alice', 'app', 'quizzer', 'alice') RETURNING id::text`).Scan(&alice); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('bob', 'app', 'quizzer', 'bob')`); err != nil {
		t.Fatal(err)
	}
	// A multi-recipient emit to alice and bob, as emitToMany records it,
	// whose copy into alice's workspace is still to be made (interrupted;
	// FinishFanouts makes it).
	ta, tb := apps.PersonalTopic("dm", "alice"), apps.PersonalTopic("dm", "bob")
	rec, _ := json.Marshal(emitRecord{Event: "dm", To: []recipient{{User: "alice", Topic: ta}, {User: "bob", Topic: tb}}})
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text, matched, recipients, results)
		VALUES ('quizzer', 'e1', gen_random_uuid(), $1, 'round over', 0, $2, '{"found": []}')`, []string{ta, tb}, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions) VALUES ('quizzer', 'e1', $1, '{}')`, alice); err != nil {
		t.Fatal(err)
	}
	a := &API{DB: d}
	if _, err := a.ForgetUser(ctx, "quizzer", "bob", "app:quizzer"); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_fanout WHERE app_id = 'quizzer' AND source_id = 'e1' AND tenant_id = $1`, alice).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatal("forgetting bob deleted alice's copy of the emit still to be made")
	}
}

// A multi-recipient emit to bob, by his subscription and natively, and to
// alice: forgetting bob keeps alice's part of the record and her copy
// still to be made, and nothing of bob's.
func TestForgetUserStripsThemFromSharedEmits(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('quizzer', 'Quizzer')`); err != nil {
		t.Fatal(err)
	}
	var alice string
	if err := d.Pool.QueryRow(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('alice', 'app', 'quizzer', 'alice') RETURNING id::text`).Scan(&alice); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('bob', 'app', 'quizzer', 'bob')`); err != nil {
		t.Fatal(err)
	}
	ta, tb := apps.PersonalTopic("dm", "alice"), apps.PersonalTopic("dm", "bob")
	sub := "00000000-0000-4000-8000-000000000001"
	rec, _ := json.Marshal(emitRecord{Event: "dm", To: []recipient{
		{User: "alice", Topic: ta}, {User: "bob", Subscription: sub}, {User: "bob", Native: true, Topic: tb}}})
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text, matched, recipients, results, fingerprint)
		VALUES ('quizzer', 'e1', gen_random_uuid(), $1, 'round over', 2, $2, $3, '\x01')`,
		[]string{ta}, rec, `{"found": ["s:`+sub+`/bob", "n:bob", "u:alice"]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions) VALUES ('quizzer', 'e1', $1, '{}')`, alice); err != nil {
		t.Fatal(err)
	}
	a := &API{DB: d}
	if _, err := a.ForgetUser(ctx, "quizzer", "bob", "app:quizzer"); err != nil {
		t.Fatal(err)
	}
	var recipients, results string
	var topics []string
	var fingerprintNull bool
	if err := d.Pool.QueryRow(ctx, `SELECT recipients::text, results::text, topics, fingerprint IS NULL FROM topic_events WHERE app_id = 'quizzer' AND source_id = 'e1'`).
		Scan(&recipients, &results, &topics, &fingerprintNull); err != nil {
		t.Fatalf("the shared emit's record: %v", err)
	}
	for _, s := range []string{recipients, results} {
		if strings.Contains(s, "bob") || strings.Contains(s, tb) {
			t.Errorf("bob is still in the shared emit: %s", s)
		}
	}
	if !strings.Contains(recipients, `"alice"`) || !strings.Contains(results, "u:alice") || len(topics) != 1 || topics[0] != ta {
		t.Errorf("alice's part went: recipients %s, results %s, topics %v", recipients, results, topics)
	}
	if !fingerprintNull {
		t.Error("the fingerprint, which covered bob, is kept")
	}
	var pending int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_fanout WHERE app_id = 'quizzer' AND source_id = 'e1' AND tenant_id = $1`, alice).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatal("forgetting bob deleted alice's copy still to be made")
	}
}
