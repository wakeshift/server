package appapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/dbtest"
)

// A deleted app gets no new publish URL, however the request got past its
// owner check: the check is made again in the transaction that makes it.
func TestNoPublishURLForADeletedApp(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	var owner string
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('ada@example.com') RETURNING id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `WITH o AS (INSERT INTO organizations (name, created_by) VALUES ('Acme', $1) RETURNING id)
		INSERT INTO apps (id, name, org_id, created_by) SELECT a.id, a.name, o.id, $1 FROM o, (VALUES ('quizzer', 'Quizzer'), ('gone', 'Gone')) a(id, name)`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE apps SET deleted_at = now() WHERE id = 'gone'`); err != nil {
		t.Fatal(err)
	}
	api := &appapi.API{DB: d, Issuer: "https://wakeshift.test"}
	m := apps.Manifest{Events: []apps.EventType{{Name: "ci.done", Description: "A build finished.", Topic: "ci/x",
		Arguments: json.RawMessage(`{"type":"object"}`), Payload: json.RawMessage(`{"type":"object"}`)}}}
	app := func(id string) apps.App { return apps.App{ID: id, Manifest: m} }
	if _, err := api.CreateEventPublishURL(ctx, app("gone"), "ci.done", nil, "", "", "user:"+owner); !errors.Is(err, apps.ErrDeleted) {
		t.Fatalf("a deleted app: %v", err)
	}
	if _, err := api.CreateEventPublishURL(ctx, app("nope"), "ci.done", nil, "", "", "user:"+owner); !errors.Is(err, apps.ErrNotFound) {
		t.Fatalf("no such app: %v", err)
	}
	if u, err := api.CreateEventPublishURL(ctx, app("quizzer"), "ci.done", nil, "", "", "user:"+owner); err != nil || u.ID == "" {
		t.Fatalf("an app: %+v %v", u, err)
	}
	var n int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_publish_urls`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d publish URLs", n)
	}
}
