package appapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/apps"
)

// unknown_event names the event and the likely cause (no manifest sent yet),
// with the same status and code as before.
func TestUnknownEventSaysWhy(t *testing.T) {
	_, err := apps.Manifest{}.EventType("chat.message")
	w := httptest.NewRecorder()
	(&API{}).respondEventSub(w, nil, err)
	var body struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusNotFound || body.Error.Code != "unknown_event" {
		t.Fatalf("%d %q, want 404 unknown_event", w.Code, body.Error.Code)
	}
	for _, want := range []string{`no event named "chat.message"`, "manifest", "publishManifest", "at startup"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Errorf("message %q doesn't say %q", body.Error.Message, want)
		}
	}

	long := strings.Repeat("a", 200)
	if m := unknownEventMessage(apps.UnknownEventError{Name: long}); strings.Contains(m, long) {
		t.Errorf("a long name is repeated whole: %q", m)
	}
	if m := unknownEventMessage(apps.ErrUnknownEventType); !strings.Contains(m, "no event by that name") {
		t.Errorf("without a name: %q", m)
	}
}
