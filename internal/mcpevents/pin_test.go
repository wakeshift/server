package mcpevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A Pinned request connects only to the addresses pinned, whatever its
// host resolves to: here a name that resolves to nothing.
func TestPinnedDialsOnlyThePinned(t *testing.T) {
	SetAllowLoopback(true)
	t.Cleanup(func() { SetAllowLoopback(false) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	url := "http://pinned.invalid" + port + "/hook"
	c := Client()
	do := func(ctx context.Context) error {
		req, _ := http.NewRequestWithContext(ctx, "POST", url, nil)
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := do(context.Background()); err == nil {
		t.Fatal("pinned.invalid resolved without a pin")
	}
	if err := do(Pinned(context.Background(), []netip.Addr{netip.MustParseAddr("127.0.0.1")})); err != nil {
		t.Fatalf("pinned to the server: %v", err)
	}
	// Pinned to an address that isn't public: refused before connecting.
	SetAllowLoopback(false)
	c = Client()
	if err := do(Pinned(context.Background(), []netip.Addr{netip.MustParseAddr("127.0.0.1")})); !errors.Is(err, errPrivate) {
		t.Fatalf("pinned to loopback without AllowLoopback: %v", err)
	}
}

// A pinned request tries every address pinned, as a dial to a name tries
// every address it resolves to: past ones that fail (here, refused at
// connect time for not being public) and across address families, so a
// callback whose one reachable address comes last, or in the other
// family, is reached.
func TestPinnedTriesEveryAddress(t *testing.T) {
	SetAllowLoopback(true)
	t.Cleanup(func() { SetAllowLoopback(false) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	// A client of its own each time, as a verification has: no connection to
	// reuse.
	do := func(addrs ...string) error {
		var pin []netip.Addr
		for _, a := range addrs {
			pin = append(pin, netip.MustParseAddr(a))
		}
		req, _ := http.NewRequestWithContext(Pinned(context.Background(), pin), "POST", "http://pinned.invalid"+port+"/hook", nil)
		resp, err := Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	for _, addrs := range [][]string{
		{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "192.168.0.1", "127.0.0.1"}, // the reachable one seventh
		{"::1", "fd00::1", "10.0.0.1", "127.0.0.1"},                                              // IPv6 first, none reachable
		{"10.0.0.1", "::1", "127.0.0.1"},
	} {
		if err := do(addrs...); err != nil {
			t.Errorf("pinned to %v: %v", addrs, err)
		}
	}
	if _, err := dialPinned(context.Background(), &net.Dialer{}, "tcp", "443", nil); !errors.Is(err, errPrivate) {
		t.Fatalf("pinned to nothing: %v", err)
	}
	// None reachable: the refusal, from the first family's first address.
	if err := do("10.0.0.1", "fd00::1"); !errors.Is(err, errPrivate) {
		t.Fatalf("pinned to private addresses alone: %v", err)
	}
}

// A name whose answer mixes public addresses and ones that aren't is
// treated as before pinning, when the dialer resolved the name itself and
// its Control refused each address that isn't public and went on to the
// next: the verification connects to the public addresses only, never to
// the others; with no public one it's refused. Pinned, it's the same, and
// the private addresses aren't even dialed (or counted).
func TestMixedAnswerConnectsOnlyToPublic(t *testing.T) {
	SetAllowLoopback(true) // 127.0.0.1 plays the public address
	t.Cleanup(func() { SetAllowLoopback(false) })
	was := lookupNetIP
	t.Cleanup(func() { lookupNetIP = was })
	lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("169.254.169.254"),
			netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("fd00::1")}, nil
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		var ctl struct{ Challenge string }
		json.Unmarshal(b, &ctl)
		json.NewEncoder(w).Encode(map[string]string{"challenge": ctl.Challenge})
	}))
	t.Cleanup(srv.Close)
	cb := "http://mixed.invalid" + srv.URL[strings.LastIndex(srv.URL, ":"):] + "/hook"
	addrs, err := Resolve(context.Background(), cb)
	if err != nil || fmt.Sprint(addrs) != "[127.0.0.1]" {
		t.Fatalf("Resolve: %v %v, want the public address alone", addrs, err)
	}
	// Mixed.invalid resolves nowhere for real: the request reached the
	// listener only through the pin, at the public address.
	if err := Verify(Pinned(context.Background(), addrs), Client(), cb, "sub_1", []byte("secret"), time.Now()); err != nil || hits.Load() != 1 {
		t.Fatalf("verifying the mixed answer's public address: %v (%d requests)", err, hits.Load())
	}
	// No public address in the answer: refused, nothing sent.
	SetAllowLoopback(false)
	if _, err := Resolve(context.Background(), cb); !errors.Is(err, errPrivate) {
		t.Fatalf("an answer with no public address: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("%d requests", hits.Load())
	}
}

// Resolve keeps every public address a name has, however many, in the
// resolver's order, dropping those that aren't public and repeats.
func TestResolveKeepsEveryPublicAddress(t *testing.T) {
	was := lookupNetIP
	t.Cleanup(func() { lookupNetIP = was })
	var answer []netip.Addr
	for _, a := range []string{"10.0.0.1", "2606:4700::1", "8.8.8.1", "8.8.8.2", "8.8.8.3", "8.8.8.4", "8.8.8.5", "::ffff:8.8.8.6", "8.8.8.2", "fd00::1"} {
		answer = append(answer, netip.MustParseAddr(a))
	}
	lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) { return answer, nil }
	got, err := Resolve(context.Background(), "https://many.example/hook")
	want := "[2606:4700::1 8.8.8.1 8.8.8.2 8.8.8.3 8.8.8.4 8.8.8.5 8.8.8.6]"
	if err != nil || fmt.Sprint(got) != want {
		t.Fatalf("Resolve: %v %v, want %s", got, err, want)
	}
}

// Resolve keeps the public addresses alone; a name with none is errPrivate,
// which Unreachable tells the app only as "couldn't be reached", unless
// the callback named the address itself.
func TestResolveAndUnreachable(t *testing.T) {
	if _, err := Resolve(context.Background(), "https://localhost/hook"); !errors.Is(err, errPrivate) {
		t.Fatalf("localhost: %v", err)
	}
	got, err := Resolve(context.Background(), "https://[::ffff:8.8.8.8]/hook")
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("8.8.8.8") {
		t.Fatalf("a public literal: %v %v", got, err)
	}
	for cb, want := range map[string]string{
		"https://localhost/hook":       "the callback couldn't be reached",
		"https://internal.example/x":   "the callback couldn't be reached",
		"https://10.1.2.3/hook":        "the callback's address isn't public",
		"https://[fd00::1]:8443/hook?": "the callback's address isn't public",
	} {
		e := Unreachable(cb, errPrivate)
		if e.Detail != want || e.Reason != ReasonRefused {
			t.Errorf("%s: %q %q, want %q", cb, e.Reason, e.Detail, want)
		}
	}
}
