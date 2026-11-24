// Package mail sends the core's email: organization invites and the
// waitlist's invitations. A Sender is Resend's HTTPS API in production and a
// Fake in tests. Nothing here logs a message's text, which carries
// links with tokens, nor its recipient.
package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Message is one email.
type Message struct {
	To, Subject, Text, HTML string
}

// Sender sends mail.
type Sender interface {
	// Ready says whether it can send now (its key is set up).
	Ready(ctx context.Context) bool
	// Send sends m. Its error never includes m's text or recipient.
	Send(ctx context.Context, m Message) error
}

// ErrNotReady: there's no key to send with yet.
var ErrNotReady = errors.New("mail: email isn't set up yet")

// None is a Sender that's never ready: the console shows invite links to
// copy instead.
type None struct{}

func (None) Ready(context.Context) bool          { return false }
func (None) Send(context.Context, Message) error { return ErrNotReady }

// Resend sends through Resend's API (POST /emails with a bearer key).
type Resend struct {
	From string // e.g. "Wakeshift <invites@wakeshift.ai>"
	// Key, or KeySecret: a Secret Manager secret (projects/…/secrets/…)
	// read at runtime with the instance's service account and again every
	// 10 minutes, so the service runs (and shows links instead) before the
	// key is added, and picks up a rotated key.
	Key, KeySecret string
	// API is Resend's base URL (default https://api.resend.com).
	API    string
	Client *http.Client
	// Metadata is the metadata server's base URL (tests point it at a
	// fake; default http://metadata.google.internal).
	Metadata string

	mu      sync.Mutex
	key     string
	checked time.Time
}

func (r *Resend) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (r *Resend) apiKey(ctx context.Context) (string, error) {
	if r.Key != "" {
		return r.Key, nil
	}
	if r.KeySecret == "" {
		return "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.checked.IsZero() && time.Since(r.checked) < recheck {
		return r.key, nil
	}
	// Read again every recheck, so a new version (a rotated key) or a
	// disabled one applies without a restart. A failed read keeps the key
	// already read.
	r.checked = time.Now()
	key, err := r.readSecret(ctx)
	if err != nil {
		return r.key, err
	}
	r.key = key
	return key, nil
}

// recheck is how often the key's secret is read again.
const recheck = 10 * time.Minute

// Ready says whether there's a key.
func (r *Resend) Ready(ctx context.Context) bool {
	k, _ := r.apiKey(ctx)
	return k != ""
}

// Send sends m.
func (r *Resend) Send(ctx context.Context, m Message) error {
	key, err := r.apiKey(ctx)
	if err != nil {
		return err
	}
	if key == "" {
		return ErrNotReady
	}
	body, _ := json.Marshal(map[string]any{"from": r.From, "to": []string{m.To}, "subject": m.Subject, "text": m.Text, "html": m.HTML})
	api := strings.TrimSuffix(r.API, "/")
	if api == "" {
		api = "https://api.resend.com"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", api+"/emails", bytes.NewReader(body))
	if err != nil {
		return errors.New("mail: bad Resend URL")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client().Do(req)
	if err != nil {
		return errors.New("mail: request to Resend failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("mail: Resend answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// readSecret reads the key's secret's latest version with the instance's
// service account: "" if it has none yet.
func (r *Resend) readSecret(ctx context.Context) (string, error) {
	md := strings.TrimSuffix(r.Metadata, "/")
	if md == "" {
		md = "http://metadata.google.internal"
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", md+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := r.client().Do(req)
	if err != nil {
		return "", errors.New("mail: metadata token: request failed")
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&tok)
	resp.Body.Close()
	if err != nil || tok.AccessToken == "" {
		return "", errors.New("mail: metadata token: none given")
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", "https://secretmanager.googleapis.com/v1/"+r.KeySecret+"/versions/latest:access", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err = r.client().Do(req)
	if err != nil {
		return "", errors.New("mail: reading the key: request failed")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest, http.StatusPreconditionFailed:
		return "", nil // no version yet, or a disabled one
	default:
		return "", fmt.Errorf("mail: reading the key: HTTP %d", resp.StatusCode)
	}
	var v struct {
		Payload struct{ Data string } `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&v); err != nil {
		return "", errors.New("mail: reading the key: bad answer")
	}
	b, err := base64.StdEncoding.DecodeString(v.Payload.Data)
	if err != nil {
		return "", errors.New("mail: reading the key: bad encoding")
	}
	return strings.TrimSpace(string(b)), nil
}

// Fake keeps what it's asked to send, for tests.
type Fake struct {
	NotReady bool
	Fail     error

	mu   sync.Mutex
	sent []Message
}

func (f *Fake) Ready(context.Context) bool { return !f.NotReady }

func (f *Fake) Send(_ context.Context, m Message) error {
	if f.NotReady {
		return ErrNotReady
	}
	if f.Fail != nil {
		return f.Fail
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

// Sent is what's been sent so far.
func (f *Fake) Sent() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.sent...)
}
