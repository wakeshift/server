package waittask

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

var (
	key1  = []byte("0123456789abcdef0123456789abcdef")
	key2  = []byte("fedcba9876543210fedcba9876543210")
	task  = Task{Tenant: "5f0c6a3e-2b1d-4c8e-9a7f-1d2e3f4a5b6c", Session: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", Issued: time.Unix(1_800_000_000, 0)}
	later = task.Issued.Add(time.Hour)
)

func codec(t *testing.T, key []byte) *Codec {
	t.Helper()
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	c := codec(t, key1)
	id, err := c.Mint("chat", task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, Prefix) || len(id) > 128 || len(id) != 96 {
		t.Fatalf("id %q (%d)", id, len(id))
	}
	if strings.Contains(id, task.Session[:8]) || strings.Contains(id, task.Tenant[:8]) {
		t.Fatalf("the id shows what it names: %s", id)
	}
	got, err := c.Open("chat", id, later)
	if err != nil || got != task {
		t.Fatalf("open: %+v %v", got, err)
	}
	// Another instance with the same key (a restart, a deploy) opens it too.
	if got, err := codec(t, key1).Open("chat", id, later); err != nil || got != task {
		t.Fatalf("another instance: %+v %v", got, err)
	}
	// Two tasks for the same session are different IDs, with different batches.
	again, _ := c.Mint("chat", task)
	if again == id || c.Holder(again) == c.Holder(id) {
		t.Fatal("two tasks share an ID or a holder")
	}
	if c.Holder(id) != codec(t, key1).Holder(id) || !strings.HasPrefix(c.Holder(id), HolderPrefix) || len(c.Holder(id)) != len(HolderPrefix)+16 {
		t.Fatalf("holder %q", c.Holder(id))
	}
}

func TestRefused(t *testing.T) {
	c := codec(t, key1)
	id, _ := c.Mint("chat", task)
	if _, err := c.Open("other-app", id, later); err != ErrInvalid {
		t.Errorf("another app's: %v", err)
	}
	if _, err := codec(t, key2).Open("chat", id, later); err != ErrInvalid {
		t.Errorf("another key's: %v", err)
	}
	if _, err := c.Open("chat", id, task.Issued.Add(MaxAge+time.Second)); err != ErrExpired {
		t.Errorf("expired: %v", err)
	}
	if _, err := c.Open("chat", id, task.Issued.Add(-2*time.Minute)); err != ErrInvalid {
		t.Errorf("from the future: %v", err)
	}
	// Every byte changed, one at a time, is refused.
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, Prefix))
	for i := range raw {
		b := append([]byte(nil), raw...)
		b[i] ^= 0x01
		if _, err := c.Open("chat", Prefix+base64.RawURLEncoding.EncodeToString(b), later); err != ErrInvalid {
			t.Fatalf("byte %d flipped: %v", i, err)
		}
	}
	for _, bad := range []string{"", Prefix, "wkt2_" + strings.TrimPrefix(id, Prefix), id + "A", id[:len(id)-1], strings.TrimPrefix(id, Prefix), Prefix + strings.Repeat("A", 200), id + "=", strings.ToUpper(id)} {
		if _, err := c.Open("chat", bad, later); err != ErrInvalid {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := c.Mint("chat", Task{Tenant: "nope", Session: task.Session}); err == nil {
		t.Error("minted for a tenant that isn't a UUID")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Error("a short key")
	}
}
