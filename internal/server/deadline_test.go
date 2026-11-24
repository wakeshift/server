package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// shortBodyDeadline lowers BodyReadTimeout for a test.
func shortBodyDeadline(t *testing.T, d time.Duration) {
	old := BodyReadTimeout
	BodyReadTimeout = d
	t.Cleanup(func() { BodyReadTimeout = old })
}

// dial opens a raw connection to srv.
func dial(t *testing.T, srv *httptest.Server) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A client that trickles its body in is cut off once BodyReadTimeout has
// passed: the handler's read fails, however long the client keeps sending.
func TestSlowBodyIsCutOff(t *testing.T) {
	shortBodyDeadline(t, 300*time.Millisecond)
	readErr := make(chan error, 1)
	s := &Server{DB: fakeDB{}, Web: fstest.MapFS{}, Routes: map[string]http.Handler{
		"POST /v1/slow": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := io.ReadAll(r.Body)
			readErr <- err
		}),
	}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	c := dial(t, srv)
	fmt.Fprintf(c, "POST /v1/slow HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 60000\r\n\r\n{")
	start := time.Now()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				if _, err := c.Write([]byte(" ")); err != nil {
					return
				}
			}
		}
	}()
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("the slow body was read to its end")
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("cut off only after %v", took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a body trickled in for 10s wasn't cut off")
	}
}

// A request that sent its body in time and is then held open (a long poll,
// a wait, an MCP push stream) isn't cut off by the body's deadline, and
// its context isn't canceled; nor is a request without a body, nor the
// next request on the same connection.
func TestBodyDeadlineSparesHeldRequests(t *testing.T) {
	shortBodyDeadline(t, 200*time.Millisecond)
	hold := 4 * BodyReadTimeout
	held := func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "read: "+err.Error(), 500)
			return
		}
		w.WriteHeader(200)
		http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
			return // the client sees a short body
		case <-time.After(hold):
		}
		w.Write([]byte("held"))
	}
	s := &Server{DB: fakeDB{}, Web: fstest.MapFS{}, Routes: map[string]http.Handler{
		"POST /v1/held": http.HandlerFunc(held),
		"GET /v1/held":  http.HandlerFunc(held),
	}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	c := dial(t, srv)
	br := bufio.NewReader(c)
	for i, req := range []string{
		"POST /v1/held HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}",
		"POST /v1/held HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n\r\n",
		"GET /v1/held HTTP/1.1\r\nHost: x\r\n\r\n",
		"POST /v1/held HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}", // the same connection, again
	} {
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(c, req); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || string(b) != "held" {
			t.Fatalf("request %d: %d %q %v", i, resp.StatusCode, b, err)
		}
	}
}

// RFC 9116: the deployment's support address and the repository's
// advisories, with an Expires about a year out; without a support address,
// the advisories alone. Other .well-known paths stay JSON 404s.
func TestSecurityTxt(t *testing.T) {
	for _, email := range []string{"security@example.com", ""} {
		h := (&Server{DB: fakeDB{}, Web: fstest.MapFS{}, SupportEmail: email}).Handler()
		r := get(t, h, "/.well-known/security.txt")
		b := body(r)
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
			t.Fatalf("%q: %d %s", email, r.StatusCode, r.Header.Get("Content-Type"))
		}
		if !strings.Contains(b, "Contact: "+AdvisoriesURL+"\n") || !strings.Contains(b, "Preferred-Languages: en\n") {
			t.Fatalf("%q: %s", email, b)
		}
		if strings.Contains(b, "Contact: mailto:security@example.com\n") != (email != "") || strings.Count(b, "mailto:") != strings.Count(email, "@") {
			t.Fatalf("%q: %s", email, b)
		}
		_, rest, _ := strings.Cut(b, "Expires: ")
		exp, err := time.Parse(time.RFC3339, strings.SplitN(rest, "\n", 2)[0])
		if err != nil || exp.Before(time.Now().AddDate(0, 11, 0)) || exp.After(time.Now().AddDate(1, 0, 1)) {
			t.Fatalf("%q: Expires %v %v", email, exp, err)
		}
		if r := get(t, h, "/.well-known/nope"); r.StatusCode != 404 || !strings.Contains(body(r), `"not_found"`) {
			t.Fatalf("other .well-known: %d", r.StatusCode)
		}
	}
}
