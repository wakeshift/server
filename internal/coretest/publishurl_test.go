package coretest

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// syncBuffer is a bytes.Buffer safe for the server's goroutines to log into.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A publish URL only takes a POST. A GET or HEAD (a link unfurled in chat,
// a browser's prefetch) or a trailing slash is a 405 naming POST, not the
// web app, and the request log never shows the token.
func TestPublishURLRefusesOtherMethods(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := Start(t)
	for _, r := range [][2]string{
		{"GET", "/v1/publish/wsp_s3cretA"},
		{"HEAD", "/v1/publish/wsp_s3cretB"},
		{"GET", "/v1/publish/wsp_s3cretC/"},
		{"PUT", "/v1/publish/wsp_s3cretD"},
	} {
		req, _ := http.NewRequest(r[0], c.URL+r[1], nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
			t.Errorf("%s %s: %d, Allow %q", r[0], r[1], resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
	if out := logs.String(); strings.Contains(out, "s3cret") {
		t.Fatalf("the request log has a publish token:\n%s", out)
	}
}
