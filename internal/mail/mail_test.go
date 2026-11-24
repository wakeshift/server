package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResendSends(t *testing.T) {
	var got map[string]any
	var auth string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		w.Write([]byte(`{"message": "echoes the secret link https://x/join#tok"}`))
	}))
	defer srv.Close()
	r := &Resend{From: "Wakeshift <invites@wakeshift.ai>", Key: "re_test", API: srv.URL}
	m := Message{To: "cy@example.com", Subject: "Join", Text: "https://x/join#tok", HTML: "<a>"}
	if !r.Ready(context.Background()) {
		t.Fatal("not ready with a key")
	}
	if err := r.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer re_test" || got["from"] != "Wakeshift <invites@wakeshift.ai>" || got["text"] != m.Text || got["to"].([]any)[0] != "cy@example.com" {
		t.Fatalf("sent %v with %q", got, auth)
	}
	// A refusal's error names the status, never the message, its
	// recipient or Resend's answer.
	status = http.StatusUnprocessableEntity
	err := r.Send(context.Background(), m)
	if err == nil || strings.Contains(err.Error(), "tok") || strings.Contains(err.Error(), "cy@") || !strings.Contains(err.Error(), "422") {
		t.Fatalf("error: %v", err)
	}
}

func TestResendWithoutAKey(t *testing.T) {
	if (&Resend{}).Ready(context.Background()) {
		t.Fatal("ready without a key")
	}
	if err := (&Resend{}).Send(context.Background(), Message{}); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if (None{}).Ready(context.Background()) {
		t.Fatal("None is ready")
	}
}

// The key read from Secret Manager at runtime: none until a version
// exists, then that.
func TestResendReadsItsSecret(t *testing.T) {
	version := false
	var secretPath string
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "no", 403)
			return
		}
		w.Write([]byte(`{"access_token": "at"}`))
	}))
	defer md.Close()
	sm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secretPath = r.URL.Path
		if !version || r.Header.Get("Authorization") != "Bearer at" {
			http.Error(w, "{}", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"payload": map[string]string{"data": base64.StdEncoding.EncodeToString([]byte("re_live\n"))}})
	}))
	defer sm.Close()
	// Secret Manager's host is fixed: send it to the fake.
	client := &http.Client{Transport: rewrite{to: sm.URL}}
	r := &Resend{KeySecret: "projects/p/secrets/mail-api-key", Metadata: md.URL, Client: client}
	if r.Ready(context.Background()) {
		t.Fatal("ready without a version")
	}
	version = true
	r.checked = time.Now().Add(-11 * time.Minute) // past the recheck
	if !r.Ready(context.Background()) || r.key != "re_live" {
		t.Fatalf("key %q", r.key)
	}
	if secretPath != "/v1/projects/p/secrets/mail-api-key/versions/latest:access" {
		t.Fatalf("read %s", secretPath)
	}
	// Disabled (or rotated away): gone at the next read.
	version = false
	r.checked = time.Now().Add(-11 * time.Minute)
	if r.Ready(context.Background()) {
		t.Fatal("still ready after the version went")
	}
}

// rewrite sends requests for secretmanager.googleapis.com to a test
// server.
type rewrite struct{ to string }

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "secretmanager.googleapis.com" {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(rw.to, "http://")
	}
	return http.DefaultTransport.RoundTrip(r)
}
