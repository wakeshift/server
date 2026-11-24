package coretest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// serveAttached serves srv, with Wakeshift attached as at, statelessly
// behind bearer tokens that verify checks, and returns its URL.
func serveAttached(t *testing.T, srv *mcp.Server, at *wakeshift.Attachment, verify auth.TokenVerifier) string {
	t.Helper()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	hs := httptest.NewServer(auth.RequireBearerToken(verify, nil)(at.Handler(h)))
	t.Cleanup(hs.Close)
	return hs.URL
}

// tokenIsUser takes any bearer token as the user it names.
func tokenIsUser(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
}

// identifyUser identifies a request as its token's user.
func identifyUser(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
	if r.Token == nil {
		return wakeshift.Identity{}, nil
	}
	return wakeshift.Identity{User: r.Token.UserID}, nil
}

// endSub ends one of user's subscriptions by its ID, as an app's own tool
// does (POST /event-subscriptions/end), reporting whether one ended.
func endSub(app *wakeshift.Client, user, id string) (bool, error) {
	var out struct {
		Ended bool `json:"ended"`
	}
	err := coretest.RawApp(app, "POST", "/api/apps/v1/event-subscriptions/end", map[string]any{"user": user, "id": id}, &out)
	return out.Ended, err
}

// postRPC sends one JSON-RPC request to an MCP endpoint with headers, and
// returns the response's JSON (a streamed answer's data line) and its
// HTTP status.
func postRPC(t *testing.T, url string, headers map[string]string, id int, method string, params any) ([]byte, int) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	// A request answered with a stream that stays open (an events/stream
	// where push is served) fails here, rather than hanging the package.
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, "POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil && rctx.Err() != nil {
		t.Fatalf("%s: no complete answer in 30 seconds (a stream left open?): %s", method, raw)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			return []byte(data), resp.StatusCode
		}
	}
	return raw, resp.StatusCode
}

// bearer is the headers of a Streamable HTTP request with token, from a
// client with User-Agent ua.
func bearer(token, ua string) map[string]string {
	return map[string]string{"Accept": "application/json, text/event-stream", "Authorization": "Bearer " + token, "User-Agent": ua}
}
