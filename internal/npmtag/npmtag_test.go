package npmtag

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func registry(t *testing.T, tags *string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The scoped name escaped, as npm's registry takes it.
		if r.URL.EscapedPath() != "/-/package/@wakeshift%2Fopencode/dist-tags" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Write([]byte(*tags))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolve(t *testing.T) {
	tags, mu := `{"latest":"1.5.0","staging":"1.6.0-staging.2"}`, &sync.Mutex{}
	srv := registry(t, &tags, mu)
	ctx := context.Background()
	for tag, want := range map[string]string{"latest": "1.5.0", "staging": "1.6.0-staging.2"} {
		got, err := Resolve(ctx, srv.Client(), srv.URL, "@wakeshift/opencode", tag)
		if err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", tag, got, err, want)
		}
	}
	if _, err := Resolve(ctx, srv.Client(), srv.URL, "@wakeshift/opencode", "next"); err == nil {
		t.Error("a missing dist-tag resolved")
	}
	if _, err := Resolve(ctx, srv.Client(), srv.URL, "@wakeshift/other", "latest"); err == nil {
		t.Error("a 404 resolved")
	}
	if _, err := Resolve(ctx, srv.Client(), srv.URL, "@wakeshift/opencode", "Latest/../x"); err == nil {
		t.Error("a dist-tag that isn't one was looked up")
	}
}

// Watch offers each new valid version once, keeps the last one through a
// failed lookup or an invalid version, and stops with its context.
func TestWatch(t *testing.T) {
	tags, mu := `{"latest":"1.5.0"}`, &sync.Mutex{}
	srv := registry(t, &tags, mu)
	ctx, cancel := context.WithCancel(context.Background())
	var got []string
	var gmu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, srv.Client(), srv.URL, "@wakeshift/opencode", "latest", 10*time.Millisecond,
			func(v string) bool { return v != "bad" },
			func(v string) { gmu.Lock(); got = append(got, v); gmu.Unlock() })
	}()
	step := func(body string) {
		mu.Lock()
		tags = body
		mu.Unlock()
		time.Sleep(60 * time.Millisecond)
	}
	step(`{"latest":"1.5.0"}`)
	step(`not json`)
	step(`{"latest":"bad"}`)
	step(`{"latest":"1.6.0"}`)
	cancel()
	<-done
	gmu.Lock()
	defer gmu.Unlock()
	if len(got) != 2 || got[0] != "1.5.0" || got[1] != "1.6.0" {
		t.Errorf("offered %v, want [1.5.0 1.6.0]", got)
	}
}
