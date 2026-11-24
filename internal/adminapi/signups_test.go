package adminapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/adminapi"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/iap/iaptest"
	"github.com/wakeshift/server/internal/mail"
)

// withMail is setup, with a fake mail sender.
func withMail(t *testing.T) (*fixture, *mail.Fake) {
	t.Helper()
	f := setup(t)
	fake := &mail.Fake{}
	api := &adminapi.API{DB: f.d, Apps: f.reg, AppAPI: &appapi.API{DB: f.d, Apps: f.reg, Issuer: publicURL},
		Service: f.google.verifier(), ServiceAccount: serviceAccount, IAP: f.iap.Verifier(t, iaptest.Audience, admin),
		Mail: fake, ConsoleURL: publicURL}
	mux := http.NewServeMux()
	api.Register(mux)
	f.h = mux
	return f, fake
}

// waitlisted puts someone on the waitlist, as signing in past the cap does.
func (f *fixture) waitlisted(t *testing.T, email, name string) string {
	t.Helper()
	var id string
	if err := f.d.Pool.QueryRow(ctx, `INSERT INTO waitlist (provider, subject, email, name) VALUES ('google', $1, $1, $2) RETURNING id::text`, email, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) adminLog(t *testing.T, action string) (n int, reasons string) {
	t.Helper()
	if err := f.d.Pool.QueryRow(ctx, `SELECT count(*), coalesce(string_agg(reason, '|' ORDER BY id), '') FROM admin_log WHERE action = $1`, action).Scan(&n, &reasons); err != nil {
		t.Fatal(err)
	}
	return n, reasons
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return out
}

// The cap changes with a reason, logged with the old and new values; a
// refused change writes nothing.
func TestSignupCapChange(t *testing.T) {
	f, _ := withMail(t)
	bearer, assertion := f.good(t)
	const path = "/internal/admin/v1/signups/cap"
	for _, bad := range []map[string]any{{"cap": 80}, {"cap": -1, "reason": "x"}, {"cap": 20_000_000, "reason": "x"}, {"reason": "x"}} {
		if rec := f.do(t, "PUT", path, bearer, assertion, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if n, _ := f.adminLog(t, adminapi.ActionSignupCapSet); n != 0 {
		t.Fatalf("refused changes logged %d", n)
	}
	rec := f.do(t, "PUT", path, bearer, assertion, map[string]any{"cap": 80, "reason": "Launch week"})
	if rec.Code != 200 {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	if limit, _, _ := accounts.SignupCap(ctx, f.d.Pool); limit != 80 {
		t.Errorf("cap %d", limit)
	}
	var actor, kind, target, detail string
	f.d.Pool.QueryRow(ctx, `SELECT actor, target_kind, target_id, detail::text FROM admin_log WHERE action = $1`, adminapi.ActionSignupCapSet).Scan(&actor, &kind, &target, &detail)
	if n, reasons := f.adminLog(t, adminapi.ActionSignupCapSet); n != 1 || reasons != "Launch week" || actor != admin || kind != "signups" || target != "cap" ||
		!strings.Contains(detail, `"cap": 80`) || !strings.Contains(detail, `"old": 50`) {
		t.Fatalf("logged %d %q: %s %s %s %s", n, reasons, actor, kind, target, detail)
	}
}

// Inviting marks the entries invited, logs each with the reason, and emails
// each; their next sign-in makes the account, past the cap. Inviting again
// sends the email again; one with an account is skipped.
func TestWaitlistInvite(t *testing.T) {
	f, fake := withMail(t)
	bearer, assertion := f.good(t)
	const path = "/internal/admin/v1/waitlist/invite"
	ada, bob := f.waitlisted(t, "ada@example.com", "Ada Lovelace"), f.waitlisted(t, "bob@example.com", "")
	gone := "00000000-0000-0000-0000-000000000000"

	for name, bad := range map[string]map[string]any{
		"no reason":  {"ids": []string{ada}},
		"no ids":     {"ids": []string{}, "reason": "x"},
		"not an ID":  {"ids": []string{"ada@example.com"}, "reason": "x"},
		"too many":   {"ids": make([]string, adminapi.MaxBatch+1), "reason": "x"},
		"no ids key": {"reason": "x"},
	} {
		if rec := f.do(t, "POST", path, bearer, assertion, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if n, _ := f.adminLog(t, adminapi.ActionWaitlistInvited); n != 0 || len(fake.Sent()) != 0 {
		t.Fatalf("refused invites: %d logged, %d sent", n, len(fake.Sent()))
	}

	rec := f.do(t, "POST", path, bearer, assertion, map[string]any{"ids": []string{ada, bob, gone}, "reason": "Early tester"})
	if rec.Code != 200 {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	if out := decodeBody(t, rec.Body.Bytes()); out["invited"] != 2.0 || out["emailed"] != 2.0 || out["skipped"] != 1.0 || out["mail_ready"] != true {
		t.Fatalf("answer %v", out)
	}
	sent := fake.Sent()
	if len(sent) != 2 {
		t.Fatalf("%d emails", len(sent))
	}
	byTo := map[string]mail.Message{}
	for _, m := range sent {
		byTo[m.To] = m
	}
	m := byTo["ada@example.com"]
	if m.Subject != "Your Wakeshift account is ready" || !strings.HasPrefix(m.Text, "YOU'RE OFF THE WAITLIST\nYour Wakeshift account is ready\n===============================\n\nHi Ada Lovelace,\n\nThanks for waiting.") ||
		!strings.Contains(m.Text, "the Google account you joined the waitlist with") || !strings.Contains(m.Text, publicURL+"/\n") ||
		!strings.Contains(m.HTML, `<a href="`+publicURL+`/" target="_blank"`) {
		t.Errorf("ada's email: %+v", m)
	}
	if b := byTo["bob@example.com"]; !strings.HasPrefix(b.Text, "YOU'RE OFF THE WAITLIST\nYour Wakeshift account is ready\n===============================\n\nThanks for waiting.") {
		t.Errorf("bob's email: %q", b.Text)
	}
	var invited, emailed int
	f.d.Pool.QueryRow(ctx, `SELECT count(invited_at), count(emailed_at) FROM waitlist WHERE invited_by = $1`, admin).Scan(&invited, &emailed)
	if invited != 2 || emailed != 2 {
		t.Errorf("%d invited, %d emailed", invited, emailed)
	}
	if n, reasons := f.adminLog(t, adminapi.ActionWaitlistInvited); n != 2 || reasons != "Early tester|Early tester" {
		t.Fatalf("logged %d %q", n, reasons)
	}

	// Ada's next sign-in makes her account, at the cap.
	if err := pgx.BeginFunc(ctx, f.d.Pool, func(tx pgx.Tx) error {
		_, err := accounts.SetSignupCap(ctx, tx, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	store := &accounts.Store{Pool: f.d.Pool}
	if _, err := store.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "ada@example.com", Email: "ada@example.com"}); err != nil {
		t.Fatalf("ada after the invitation: %v", err)
	}
	var w *accounts.WaitlistedError
	if _, err := store.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "cy", Email: "cy@example.com"}); !errors.As(err, &w) {
		t.Fatalf("someone not invited: %v", err)
	}

	// Again: bob's email again; ada has her account, so she's skipped.
	rec = f.do(t, "POST", path, bearer, assertion, map[string]any{"ids": []string{ada, bob}, "reason": "Reminder"})
	if out := decodeBody(t, rec.Body.Bytes()); rec.Code != 200 || out["invited"] != 1.0 || out["emailed"] != 1.0 || out["skipped"] != 1.0 {
		t.Fatalf("again: %d %v", rec.Code, out)
	}
	if sent := fake.Sent(); len(sent) != 3 || sent[2].To != "bob@example.com" {
		t.Fatalf("emails after again: %d", len(sent))
	}
	var again bool
	f.d.Pool.QueryRow(ctx, `SELECT (detail->>'again')::boolean FROM admin_log WHERE action = $1 AND reason = 'Reminder'`, adminapi.ActionWaitlistInvited).Scan(&again)
	if !again {
		t.Error("the second invitation isn't logged as again")
	}

	// Without mail set up: invited, nothing sent, and the answer says so.
	fake.NotReady = true
	dee := f.waitlisted(t, "dee@example.com", "Dee")
	rec = f.do(t, "POST", path, bearer, assertion, map[string]any{"ids": []string{dee}, "reason": "x"})
	if out := decodeBody(t, rec.Body.Bytes()); rec.Code != 200 || out["invited"] != 1.0 || out["emailed"] != 0.0 || out["mail_ready"] != false {
		t.Fatalf("without mail: %d %v", rec.Code, out)
	}
}

// Removing deletes the entries and logs each; a refused removal deletes
// nothing.
func TestWaitlistRemove(t *testing.T) {
	f, _ := withMail(t)
	bearer, assertion := f.good(t)
	const path = "/internal/admin/v1/waitlist/remove"
	ada := f.waitlisted(t, "ada@example.com", "Ada")
	if rec := f.do(t, "POST", path, bearer, assertion, map[string]any{"ids": []string{ada}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("no reason: %d", rec.Code)
	}
	var n int
	if f.d.Pool.QueryRow(ctx, `SELECT count(*) FROM waitlist`).Scan(&n); n != 1 {
		t.Fatal("a refused removal deleted the entry")
	}
	rec := f.do(t, "POST", path, bearer, assertion, map[string]any{"ids": []string{ada}, "reason": "Asked to be removed"})
	if out := decodeBody(t, rec.Body.Bytes()); rec.Code != 200 || out["removed"] != 1.0 {
		t.Fatalf("remove: %d %v", rec.Code, out)
	}
	if f.d.Pool.QueryRow(ctx, `SELECT count(*) FROM waitlist`).Scan(&n); n != 0 {
		t.Fatal("the entry stayed")
	}
	var target, detail string
	f.d.Pool.QueryRow(ctx, `SELECT target_id, detail::text FROM admin_log WHERE action = $1`, adminapi.ActionWaitlistRemoved).Scan(&target, &detail)
	if logged, reasons := f.adminLog(t, adminapi.ActionWaitlistRemoved); logged != 1 || reasons != "Asked to be removed" || target != ada || strings.Contains(detail, "@") {
		t.Fatalf("logged %d %q %s %s", logged, reasons, target, detail)
	}
}
