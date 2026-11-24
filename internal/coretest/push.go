package coretest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// PushStream is the upstream of one MCP Events push stream
// (POST /api/apps/v1/events/stream), as an SDK's relay reads it: the
// status, the error before open, and the frames, one JSON object a line.
// A Go SDK harness test can drive the core's side with it, or stand in
// for the core in a relay's test with the frames it records.
type PushStream struct {
	Status int
	// Header is the response's headers (Retry-After on a 503).
	Header http.Header
	// Error is the JSON error body of a stream that didn't open.
	Error  json.RawMessage
	frames chan map[string]any
	cancel context.CancelFunc
}

// OpenPush opens a push stream for app with body ({call, name, arguments,
// cursor, max_age_ms, heartbeat_ms, voucher}), signing a fresh app
// assertion. Close it (or let the test's cleanup) to drop the upstream, as
// a client's abort does.
func (c *Core) OpenPush(t testing.TB, app *wakeshift.Client, body any) *PushStream {
	t.Helper()
	cr := CredsOf(app)
	jti := make([]byte, 16)
	rand.Read(jti)
	now := time.Now()
	tok, err := jws.Sign(cr.Key, "", "app+jwt", map[string]any{"iss": cr.ID, "sub": cr.ID, "aud": c.URL + "/api/apps",
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", c.URL+"/api/apps/v1/events/stream", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &PushStream{Status: resp.StatusCode, Header: resp.Header, frames: make(chan map[string]any, 1000), cancel: cancel}
	t.Cleanup(s.Close)
	if resp.StatusCode != http.StatusOK {
		json.NewDecoder(resp.Body).Decode(&s.Error)
		resp.Body.Close()
		close(s.frames)
		return s
	}
	go func() {
		defer close(s.frames)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var f map[string]any
			if json.Unmarshal(sc.Bytes(), &f) == nil {
				s.frames <- f
			}
		}
	}()
	return s
}

// Next is the next frame within d, or nil if none came (or the stream
// ended: Ended).
func (s *PushStream) Next(d time.Duration) map[string]any {
	select {
	case f := <-s.frames:
		return f
	case <-time.After(d):
		return nil
	}
}

// NextOf is the next frame of type typ within d, skipping heartbeats
// (unless typ is "heartbeat"); nil if none came.
func (s *PushStream) NextOf(typ string, d time.Duration) map[string]any {
	end := time.Now().Add(d)
	for {
		f := s.Next(time.Until(end))
		if f == nil || f["type"] == typ {
			return f
		}
		if f["type"] != "heartbeat" {
			return f
		}
	}
}

// Close drops the upstream.
func (s *PushStream) Close() { s.cancel() }
