package coretest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wakeshift/server/internal/mcpevents"
)

// Webhook is an MCP Events client's callback (ChatGPT's) on this machine:
// it answers the verification and records the events. It doesn't check
// signatures.
type Webhook struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events []map[string]any
}

// NewWebhook serves a Webhook for the test's lifetime. The core delivers
// to it only with AllowLoopbackWebhooks.
func NewWebhook(t *testing.T) *Webhook {
	h := &Webhook{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["type"] == "verification" {
			json.NewEncoder(w).Encode(map[string]any{"challenge": body["challenge"]})
			return
		}
		h.mu.Lock()
		h.events = append(h.events, body)
		h.mu.Unlock()
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// Delivery is the delivery object a client subscribing to h sends.
func (h *Webhook) Delivery() map[string]any {
	return map[string]any{"mode": "webhook", "url": h.srv.URL + "/callback", "secret": "whsec_" + strings.Repeat("A", 43) + "="}
}

// Events are the events received, in order.
func (h *Webhook) Events() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.events...)
}

// Data are the events' payloads, in order.
func (h *Webhook) Data() []map[string]any {
	var out []map[string]any
	for _, e := range h.Events() {
		d, _ := e["data"].(map[string]any)
		out = append(out, d)
	}
	return out
}

// AllowLoopbackWebhooks lets the core deliver MCP Events to callbacks on
// this machine (a test's Webhook), until the test ends. It sets a package
// variable, so a test that calls it mustn't run in parallel.
func AllowLoopbackWebhooks(t *testing.T) {
	mcpevents.SetAllowLoopback(true)
	t.Cleanup(func() { mcpevents.SetAllowLoopback(false) })
}
