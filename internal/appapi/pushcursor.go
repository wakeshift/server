package appapi

// Push cursors: c2.<subscription>.<seq>.<mac>, where <subscription> is
// the push subscription's event_subscriptions ID (its 16 bytes,
// base64url), <seq> a position in its session's queue (event_seq,
// decimal) and <mac> the first 16 bytes of an HMAC-SHA256,
// under a key of the core's own (API.PushCursorKey), over the workspace,
// the subscription and the position. So a cursor names exactly one
// subscription and position, can't be made or changed by anyone else, and
// is good only in the workspace it was made in. Poll keeps c1. cursors.
//
// A cursor isn't a secret by itself (it travels on every event and
// heartbeat), but it's a resume factor: never logged.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

const pushCursorPrefix = "c2."

// pushCursor is a c2. cursor as read, before its MAC is checked.
type pushCursor struct {
	sub string // the event subscription's ID (a UUID)
	seq int64
	mac []byte
}

// errBadPushCursor is a cursor Wakeshift didn't make, or made for another
// subscription or workspace: InvalidParams to the client.
var errBadPushCursor = badRequest{errors.New("cursor isn't one Wakeshift gave out for this subscription")}

// parsePushCursor reads a c2. cursor's parts. Only what pushCursorOf makes
// parses: no padding, no leading zeros, no sign.
func parsePushCursor(s string) (pushCursor, error) {
	rest, ok := strings.CutPrefix(s, pushCursorPrefix)
	if !ok || len(s) > 100 {
		return pushCursor{}, errBadPushCursor
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return pushCursor{}, errBadPushCursor
	}
	id, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(id) != 16 {
		return pushCursor{}, errBadPushCursor
	}
	seq, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || seq < 0 || strconv.FormatInt(seq, 10) != parts[1] {
		return pushCursor{}, errBadPushCursor
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(mac) != 16 {
		return pushCursor{}, errBadPushCursor
	}
	h := hex.EncodeToString(id)
	c := pushCursor{sub: h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], seq: seq, mac: mac}
	// The subscription's part must be the canonical encoding of its ID.
	if pushCursorSub(c.sub) != parts[0] {
		return pushCursor{}, errBadPushCursor
	}
	return c, nil
}

// pushCursorSub is a subscription ID's part of a cursor ("" for one that
// isn't a UUID).
func pushCursorSub(id string) string {
	b, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(b) != 16 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// pushMAC is the MAC of (workspace, subscription, position).
func (a *API) pushMAC(tenant, sub string, seq int64) []byte {
	m := hmac.New(sha256.New, a.PushCursorKey)
	m.Write([]byte("mcp-events-push-cursor\x00" + tenant + "\x00" + strings.ToLower(sub) + "\x00" + strconv.FormatInt(seq, 10)))
	return m.Sum(nil)[:16]
}

// pushCursorOf is the cursor for position seq of subscription sub, in
// workspace tenant.
func (a *API) pushCursorOf(tenant, sub string, seq int64) string {
	seq = max(seq, 0)
	return pushCursorPrefix + pushCursorSub(sub) + "." + strconv.FormatInt(seq, 10) + "." +
		base64.RawURLEncoding.EncodeToString(a.pushMAC(tenant, sub, seq))
}

// validPushCursor reports whether c was made for its subscription and
// position in workspace tenant.
func (a *API) validPushCursor(tenant string, c pushCursor) bool {
	return len(a.PushCursorKey) > 0 && hmac.Equal(c.mac, a.pushMAC(tenant, c.sub, c.seq))
}
