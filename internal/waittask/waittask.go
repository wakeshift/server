// Package waittask makes and opens the IDs of MCP wait tasks: Claude
// Code's no-install wake path (docs/harnesses.md, "Claude Code: MCP tasks
// as a wake path"). An app's MCP server answers a call of Wakeshift's wait
// tool with a task, and Claude Code polls tasks/get for it, through the
// app's server, for as long as the conversation lives: across the app's
// deploys, the core's, and the user closing and resuming Claude Code. So a
// task is nothing but its ID, which any core instance can open, and which
// names the conversation's session, never its link or any secret:
//
//	wkt1_<base64url(nonce || AES-256-GCM(tenant || session || issued))>
//
// sealed with a key derived from WAKESHIFT_SECRET_KEY and bound, as the
// AEAD's associated data, to the app that asked for it, so an ID can't be
// forged, altered, or used by another app. The core still checks, on every
// use, that the caller is the conversation the task was made for (its
// workspace) and that the conversation still exists, is the app's, and
// has the agent's consent (internal/appapi, tasks.go). An ID is 96
// characters.
//
// Holder is the name of the task's batch: the stream lease holder its
// waits take the conversation's messages under, derived from the ID, so
// every instance finds what a task handed out from the ID alone.
package waittask

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Prefix starts every task ID (its version).
const Prefix = "wkt1_"

// MaxAge is how long a task ID works after it's made: a consent's longest
// term. A conversation that waited this long calls the wait tool again.
const MaxAge = 30 * 24 * time.Hour

// HolderPrefix starts a task's lease holder: a one-shot wait's
// (internal/streams waitPrefix), so the stream treats it as one.
const HolderPrefix = "poll_wait_"

// ErrInvalid: the ID isn't one this core made for this app (forged,
// altered, another app's, or malformed).
var ErrInvalid = errors.New("waittask: not a wait task of this app")

// ErrExpired: the ID is genuine but older than MaxAge.
var ErrExpired = errors.New("waittask: the wait task has expired")

// Task is what an ID names.
type Task struct {
	Tenant  string // the conversation's workspace (a UUID)
	Session string // its session (a UUID)
	Issued  time.Time
}

// Codec makes and opens task IDs with one key.
type Codec struct {
	aead   cipher.AEAD
	holder []byte
}

// New makes a Codec from a key of at least 32 bytes (config.Key("wait-tasks")).
func New(key []byte) (*Codec, error) {
	if len(key) < 32 {
		return nil, errors.New("waittask: the key must be at least 32 bytes")
	}
	block, err := aes.NewCipher(derive(key, "seal"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Codec{aead: aead, holder: derive(key, "holder")}, nil
}

func derive(key []byte, purpose string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("wakeshift wait task\x00" + purpose))
	return m.Sum(nil)
}

// aad binds a sealed ID to its app.
func aad(app string) []byte { return []byte("wakeshift wait task v1\x00" + app) }

const plainLen = 16 + 16 + 8

// Mint is the ID of a new task for app.
func (c *Codec) Mint(app string, t Task) (string, error) {
	tenant, ok := parseUUID(t.Tenant)
	if !ok {
		return "", errors.New("waittask: tenant isn't a UUID")
	}
	session, ok := parseUUID(t.Session)
	if !ok {
		return "", errors.New("waittask: session isn't a UUID")
	}
	plain := make([]byte, 0, plainLen)
	plain = append(plain, tenant[:]...)
	plain = append(plain, session[:]...)
	plain = binary.BigEndian.AppendUint64(plain, uint64(t.Issued.Unix()))
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, plain, aad(app))), nil
}

// Open is the task an ID names, if this core made it for app and it's
// under MaxAge at now.
func (c *Codec) Open(app, id string, now time.Time) (Task, error) {
	rest, ok := strings.CutPrefix(id, Prefix)
	if !ok || len(id) > 128 {
		return Task{}, ErrInvalid
	}
	sealed, err := base64.RawURLEncoding.DecodeString(rest)
	n := c.aead.NonceSize()
	if err != nil || len(sealed) != n+plainLen+c.aead.Overhead() {
		return Task{}, ErrInvalid
	}
	plain, err := c.aead.Open(nil, sealed[:n], sealed[n:], aad(app))
	if err != nil || len(plain) != plainLen {
		return Task{}, ErrInvalid
	}
	t := Task{
		Tenant:  formatUUID(plain[:16]),
		Session: formatUUID(plain[16:32]),
		Issued:  time.Unix(int64(binary.BigEndian.Uint64(plain[32:])), 0),
	}
	if now.Sub(t.Issued) > MaxAge {
		return t, ErrExpired
	}
	if t.Issued.After(now.Add(time.Minute)) {
		return t, ErrInvalid // made by a core whose clock is far ahead: never trusted to outlive MaxAge
	}
	return t, nil
}

// Holder is the stream lease holder of the task an ID names: its batch's
// name. Only meaningful for an ID Open accepted.
func (c *Codec) Holder(id string) string {
	m := hmac.New(sha256.New, c.holder)
	m.Write([]byte(id))
	return HolderPrefix + hex.EncodeToString(m.Sum(nil)[:8])
}

// parseUUID reads a UUID in its canonical form (any case).
func parseUUID(s string) ([16]byte, bool) {
	var u [16]byte
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, false
	}
	b, err := hex.DecodeString(s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:])
	if err != nil {
		return u, false
	}
	copy(u[:], b)
	return u, true
}

// formatUUID writes 16 bytes as a canonical, lowercase UUID.
func formatUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
