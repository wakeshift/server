// Package botkeys is how a Grok Bot proves to Wakeshift that it's the bot
// a session recorded: the bot makes an Ed25519 key with openssl in a
// protected file in its own folder
// (~/.local/share/wakeshift/<bot id>/signing.pem), and signs challenges
// Wakeshift issues with it. Wakeshift keeps only the public key.
//
// What's signed is the text "wakeshift-proof", a newline, the bot's ID, a
// newline, then the challenge (Message): Wakeshift rebuilds it from the bot
// ID it recorded, so a signature made for one bot can't be replayed for
// another, and the fixed first line keeps it from meaning anything else.
//
// A challenge is wakeshift:v1:<purpose>:<id>:<nonce> (Format): purpose
// register (handed out in a subscribing result) or setup (POSTed through
// the webhook), id the workspace's and the challenge row's IDs, nonce 128
// bits derived from them with a key of Wakeshift's (Codec), so an open
// challenge can be shown again without storing it. Only the nonce's
// SHA-256 is stored. Wakeshift owns replay protection and expiry: each
// challenge works once, until its expiry, by the database's clock, and
// three bad signatures end it (Lock, Use, Fail).
package botkeys

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/harnesses"
)

// Purposes.
const (
	Register = "register"
	Setup    = "setup"
)

// Lifetimes: a register challenge is shown again while it's open; a
// setup challenge is the 10 minutes the run has to answer.
var (
	RegisterTTL = 30 * time.Minute
	SetupTTL    = 10 * time.Minute
)

// MaxFailures is how many signatures that don't verify end a challenge.
const MaxFailures = 3

// MaxChallenge is the longest challenge the routine accepts (and a far
// bound on ours).
const MaxChallenge = 200

// Pattern is what the agent checks a challenge against before writing it
// to a file: only letters, digits, ":", "-" and "_".
var Pattern = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,200}$`)

// challengeRE is a challenge as Wakeshift makes them.
var challengeRE = regexp.MustCompile(`^wakeshift:v1:(register|setup):([A-Za-z0-9_-]{43}):([A-Za-z0-9_-]{22})$`)

// Challenge is a parsed challenge.
type Challenge struct {
	Purpose string
	Tenant  string // uuid
	ID      string // uuid
	Nonce   []byte // 16 bytes
}

// String is the challenge as handed out.
func (c Challenge) String() string {
	return Format(c.Purpose, c.Tenant, c.ID, c.Nonce)
}

// Format is wakeshift:v1:<purpose>:<id>:<nonce>.
func Format(purpose, tenant, id string, nonce []byte) string {
	t, _ := uuidBytes(tenant)
	i, _ := uuidBytes(id)
	return "wakeshift:v1:" + purpose + ":" + base64.RawURLEncoding.EncodeToString(append(t, i...)) + ":" + base64.RawURLEncoding.EncodeToString(nonce)
}

// ErrBadChallenge is a string that isn't a challenge Wakeshift makes.
var ErrBadChallenge = errors.New("botkeys: not a challenge")

// Parse reads a challenge (surrounding white space, such as a file's
// trailing newline, ignored).
func Parse(s string) (Challenge, error) {
	m := challengeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Challenge{}, ErrBadChallenge
	}
	ids, err := base64.RawURLEncoding.DecodeString(m[2])
	if err != nil || len(ids) != 32 {
		return Challenge{}, ErrBadChallenge
	}
	nonce, err := base64.RawURLEncoding.DecodeString(m[3])
	if err != nil || len(nonce) != 16 {
		return Challenge{}, ErrBadChallenge
	}
	return Challenge{Purpose: m[1], Tenant: uuidString(ids[:16]), ID: uuidString(ids[16:]), Nonce: nonce}, nil
}

// Message is the text a proof signs: "wakeshift-proof\n<bot>\n<challenge>".
func Message(bot, challenge string) []byte {
	return []byte("wakeshift-proof\n" + bot + "\n" + challenge)
}

// BotIDOK reports whether a bot ID can go into the signed text and the
// commands: harnesses.GrokBotIDOK, the one rule the core and the
// connector share.
func BotIDOK(bot string) bool { return harnesses.GrokBotIDOK(bot) }

// Codec derives challenges' nonces with a key of its own.
type Codec struct{ key []byte }

// NewCodec makes a Codec.
func NewCodec(key []byte) *Codec { return &Codec{key: key} }

// Nonce is the challenge's nonce: derived from what it names, in a signing
// domain of its own ("bot-proof:"), so it can be shown again from its row
// and no other capability's MAC is one.
func (c *Codec) Nonce(purpose, tenant, id string) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte("bot-proof:" + purpose + ":" + tenant + ":" + id))
	return m.Sum(nil)[:16]
}

// Challenge is the challenge for a row.
func (c *Codec) Challenge(purpose, tenant, id string) string {
	return Format(purpose, tenant, id, c.Nonce(purpose, tenant, id))
}

// NonceHash is what's stored of a nonce.
func NonceHash(nonce []byte) []byte {
	s := sha256.Sum256(nonce)
	return s[:]
}

// Fingerprint is a public key's SHA-256.
func Fingerprint(pub ed25519.PublicKey) []byte {
	s := sha256.Sum256(pub)
	return s[:]
}

// Short is a fingerprint as audit entries name it: its first 16 hex
// characters.
func Short(fp []byte) string {
	h := hex.EncodeToString(fp)
	if len(h) > 16 {
		h = h[:16]
	}
	return h
}

// ErrBadKey is a public key that isn't an Ed25519 key in PEM
// (SubjectPublicKeyInfo, as `openssl pkey -pubout` writes it).
var ErrBadKey = errors.New("botkeys: not an Ed25519 public key")

// ParsePublicKey reads `openssl pkey -pubout`'s output.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, _ := pem.Decode([]byte(strings.TrimSpace(s)))
	if b == nil || b.Type != "PUBLIC KEY" {
		return nil, ErrBadKey
	}
	k, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		return nil, ErrBadKey
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return nil, ErrBadKey
	}
	return pub, nil
}

// ParseSignature reads a signature in base64 (white space ignored).
func ParseSignature(s string) ([]byte, bool) {
	s = strings.Join(strings.Fields(s), "")
	sig, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, false
	}
	return sig, true
}

// Verify checks sig, by pub, over Message(bot, challenge).
func Verify(pub ed25519.PublicKey, bot, challenge string, sig []byte) bool {
	return len(pub) == ed25519.PublicKeySize && len(sig) == ed25519.SignatureSize && ed25519.Verify(pub, Message(bot, challenge), sig)
}

// FetchMessage is what the connector signs to fetch the bot's setup
// challenge: "wakeshift-fetch\n<bot>\n<unix seconds>". Only the key the
// setup was minted for can fetch it, so knowing a bot's ID (as every app
// it uses does) isn't enough to read, and so use up, its setup check.
func FetchMessage(bot, ts string) []byte {
	return []byte("wakeshift-fetch\n" + bot + "\n" + ts)
}

// FetchSkew is how far a fetch's time may be from Wakeshift's.
const FetchSkew = 5 * time.Minute

// VerifyFetch checks a fetch's signature by pub over FetchMessage, and
// that its time (unix seconds) is within FetchSkew of now.
func VerifyFetch(pub ed25519.PublicKey, bot, ts string, sig []byte, now time.Time) bool {
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(ts) > 12 {
		return false
	}
	at := time.Unix(n, 0)
	if at.Before(now.Add(-FetchSkew)) || at.After(now.Add(FetchSkew)) {
		return false
	}
	return len(pub) == ed25519.PublicKeySize && len(sig) == ed25519.SignatureSize && ed25519.Verify(pub, FetchMessage(bot, ts), sig)
}

// privateMarkers are what a private key looks like in a proof body: PEM's
// label, and a PKCS#8 Ed25519 key's base64 prefix.
var privateMarkers = [][]byte{[]byte("PRIVATE KEY"), []byte("MC4CAQAwBQYDK2VwBCIEI")}

// HoldsPrivateKey reports whether a request body holds a private key.
func HoldsPrivateKey(body []byte) bool {
	for _, m := range privateMarkers {
		if bytes.Contains(body, m) {
			return true
		}
	}
	return false
}

func uuidBytes(s string) ([]byte, bool) {
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	return b, err == nil && len(b) == 16
}

func uuidString(b []byte) string {
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Row is a stored challenge.
type Row struct {
	ID, Session, Bot, App, Grant, Purpose, SetupID string
	NonceHash                                      []byte
	ExpiresAt, CreatedAt                           time.Time
	Failures                                       int
	Used, Expired                                  bool
}

// Issue stores a new challenge in the transaction's workspace (tenant)
// for a session and returns it. setup names the setup row (purpose
// Setup only).
func (c *Codec) Issue(ctx context.Context, tx pgx.Tx, tenant, purpose, session, bot, app, grant, setup string) (string, Row, error) {
	ttl := RegisterTTL
	if purpose == Setup {
		ttl = SetupTTL
	}
	var r Row
	var id string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return "", r, err
	}
	nonce := c.Nonce(purpose, tenant, id)
	err := tx.QueryRow(ctx, `INSERT INTO bot_challenges (tenant_id, id, session_id, bot_id, app_id, grant_id, purpose, setup_id, nonce_hash, expires_at)
		VALUES (current_tenant(), $1, $2, $3, $4, nullif($5, '')::uuid, $6, nullif($7, '')::uuid, $8, now() + $9::interval)
		RETURNING expires_at`, id, session, bot, app, grant, purpose, setup, NonceHash(nonce), ttl.String()).Scan(&r.ExpiresAt)
	if err != nil {
		return "", r, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bot_setup_targets (tenant_id) VALUES (current_tenant()) ON CONFLICT DO NOTHING`); err != nil {
		return "", r, err
	}
	// The connector fetches a challenge by the bot's ID (OpenFor): nobody
	// copies it from a webhook's body or a tool's result into a command.
	if _, err := tx.Exec(ctx, `INSERT INTO bot_open_challenges (tenant_id, challenge_id, bot_hash, purpose, expires_at) VALUES (current_tenant(), $1, $2, $3, $4)`,
		id, BotHash(bot), purpose, r.ExpiresAt); err != nil {
		return "", r, err
	}
	r.ID, r.Session, r.Bot, r.App, r.Grant, r.Purpose, r.SetupID = id, session, bot, app, grant, purpose, setup
	return Format(purpose, tenant, id, nonce), r, nil
}

// BotHash is what the index of open setup challenges keeps of a bot's ID.
func BotHash(bot string) []byte {
	h := sha256.Sum256([]byte("wakeshift-bot:" + bot))
	return h[:]
}

// Indexed is an open challenge the index holds: its workspace and ID.
type Indexed struct{ Tenant, ID string }

// OpenFor are the challenges of purpose the index holds for a bot, in any
// workspace, newest first, at most limit (id, when set, picks that one):
// read across workspaces by the bot's ID. The index holds only open
// challenges: a row goes when its challenge is used (a trigger on used_at,
// which every way a challenge ends sets: a proof, a missing report, its
// failures) or deleted, and expired ones are filtered here, so no stale
// row can push an open one past limit. The caller still checks each in its
// workspace (OpenIn) before acting on it.
func OpenFor(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, bot, purpose, id string, limit int) ([]Indexed, error) {
	rows, err := q.Query(ctx, `SELECT tenant_id::text, challenge_id::text FROM bot_open_challenges WHERE bot_hash = $1 AND purpose = $2 AND expires_at > now()
		AND ($3::text = '' OR challenge_id = NULLIF($3::text, '')::uuid) ORDER BY expires_at DESC LIMIT $4`, BotHash(bot), purpose, id, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Indexed])
}

// OpenIn reports, in the challenge's workspace, whether challenge id of
// purpose is still open (unused, unexpired) for bot; a setup challenge
// only if its setup was minted for the key whose fingerprint is fp.
func OpenIn(ctx context.Context, tx pgx.Tx, id, bot, purpose string, fp []byte) (bool, error) {
	var open bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bot_challenges c WHERE c.id = $1 AND c.bot_id = $2 AND c.purpose = $3 AND c.used_at IS NULL AND c.expires_at > now()
		AND ($3 <> 'setup' OR EXISTS (SELECT 1 FROM agent_webhook_setups s WHERE s.id = c.setup_id AND s.key_fingerprint = $4)))`,
		id, bot, purpose, fp).Scan(&open)
	return open, err
}

// FindOpen is the one open challenge of purpose for bot (id, when set,
// names a register challenge): the index's candidates (OpenFor), each
// checked in its own workspace (OpenIn, through inTenant; a setup
// challenge only for the key fingerprint fp its setup was minted for). ok
// is false
// when none is open, or more than one: a bot has at most one open setup
// challenge (a save's lock per bot), and an ID names one.
func FindOpen(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, inTenant func(ctx context.Context, tenant string, fn func(pgx.Tx) error) error, bot, purpose, id string, fp []byte) (Indexed, bool, error) {
	found, err := OpenFor(ctx, q, bot, purpose, id, 20)
	if err != nil {
		return Indexed{}, false, err
	}
	var open []Indexed
	for _, f := range found {
		var ok bool
		if err := inTenant(ctx, f.Tenant, func(tx pgx.Tx) error {
			var err error
			ok, err = OpenIn(ctx, tx, f.ID, bot, purpose, fp)
			return err
		}); err != nil {
			return Indexed{}, false, err
		}
		if ok {
			open = append(open, f)
		}
	}
	if len(open) != 1 {
		return Indexed{}, false, nil
	}
	return open[0], true, nil
}

// OpenRegister is the session's open register challenge, shown again (it
// works once, until it expires), else a new one.
func (c *Codec) OpenRegister(ctx context.Context, tx pgx.Tx, tenant, session, bot, app, grant string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM bot_challenges WHERE session_id = $1 AND purpose = 'register' AND used_at IS NULL
		AND expires_at > now() + interval '2 minutes' AND bot_id = $2 AND app_id = $3 ORDER BY created_at DESC LIMIT 1`, session, bot, app).Scan(&id)
	switch {
	case err == nil:
		return c.Challenge(Register, tenant, id), nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}
	ch, _, err := c.Issue(ctx, tx, tenant, Register, session, bot, app, grant, "")
	return ch, err
}

// ErrUnknown is a challenge that isn't stored, or whose nonce doesn't
// match.
var ErrUnknown = errors.New("botkeys: unknown challenge")

// Lock reads a challenge in the transaction's workspace and locks it, if
// its nonce matches; Used and Expired say whether it still works (by the
// database's clock).
func Lock(ctx context.Context, tx pgx.Tx, ch Challenge) (Row, error) {
	var r Row
	err := tx.QueryRow(ctx, `SELECT id::text, session_id::text, bot_id, app_id, coalesce(grant_id::text, ''), purpose, coalesce(setup_id::text, ''), nonce_hash,
			expires_at, failures, used_at IS NOT NULL, expires_at <= now(), created_at
		FROM bot_challenges WHERE id = $1 FOR UPDATE`, ch.ID).
		Scan(&r.ID, &r.Session, &r.Bot, &r.App, &r.Grant, &r.Purpose, &r.SetupID, &r.NonceHash, &r.ExpiresAt, &r.Failures, &r.Used, &r.Expired, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrUnknown
	}
	if err != nil {
		return r, err
	}
	if r.Purpose != ch.Purpose || subtle.ConstantTimeCompare(r.NonceHash, NonceHash(ch.Nonce)) != 1 {
		return Row{}, ErrUnknown
	}
	return r, nil
}

// Use marks a locked challenge used.
func Use(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE bot_challenges SET used_at = now() WHERE id = $1 AND used_at IS NULL`, id)
	return err
}

// Fail counts a signature that didn't verify on a locked challenge, and
// ends it at MaxFailures. ended reports that it did.
func Fail(ctx context.Context, tx pgx.Tx, id string) (ended bool, err error) {
	err = tx.QueryRow(ctx, `UPDATE bot_challenges SET failures = failures + 1,
			used_at = CASE WHEN failures + 1 >= $2 THEN now() ELSE used_at END
		WHERE id = $1 RETURNING used_at IS NOT NULL`, id, MaxFailures).Scan(&ended)
	return ended, err
}

// Key is a session's registered bot key.
type Key struct {
	Session, Bot string
	Public       ed25519.PublicKey
	Fingerprint  []byte
	Missing      bool
}

// GetKey is the session's bot key in the transaction's workspace, if it
// has one.
func GetKey(ctx context.Context, tx pgx.Tx, session string) (Key, bool, error) {
	var k Key
	var pub []byte
	err := tx.QueryRow(ctx, `SELECT session_id::text, bot_id, public_key, fingerprint, missing_at IS NOT NULL FROM bot_keys WHERE session_id = $1`, session).
		Scan(&k.Session, &k.Bot, &pub, &k.Fingerprint, &k.Missing)
	if errors.Is(err, pgx.ErrNoRows) {
		return k, false, nil
	}
	k.Public = pub
	return k, err == nil, err
}

// PutKey registers (or replaces) the session's bot key (the session is the
// app's), clearing missing. It reports the fingerprint it replaced, if any.
func PutKey(ctx context.Context, tx pgx.Tx, session, bot, app string, pub ed25519.PublicKey) (replaced []byte, err error) {
	old, had, err := GetKey(ctx, tx, session)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO bot_keys (tenant_id, session_id, bot_id, app_id, public_key, fingerprint) VALUES (current_tenant(), $1, $2, $5, $3, $4)
		ON CONFLICT (tenant_id, session_id) DO UPDATE SET bot_id = excluded.bot_id, public_key = excluded.public_key, fingerprint = excluded.fingerprint,
			replaced_at = CASE WHEN bot_keys.fingerprint = excluded.fingerprint THEN bot_keys.replaced_at ELSE now() END, missing_at = NULL`,
		session, bot, []byte(pub), Fingerprint(pub), app)
	if err != nil || !had || bytes.Equal(old.Fingerprint, Fingerprint(pub)) {
		return nil, err
	}
	return old.Fingerprint, nil
}

// MarkMissing records that the session's bot reported its key missing.
func MarkMissing(ctx context.Context, tx pgx.Tx, session string) error {
	_, err := tx.Exec(ctx, `UPDATE bot_keys SET missing_at = coalesce(missing_at, now()) WHERE session_id = $1`, session)
	return err
}

// ClearMissing records that the session's bot proved its key again.
func ClearMissing(ctx context.Context, tx pgx.Tx, session string) error {
	_, err := tx.Exec(ctx, `UPDATE bot_keys SET missing_at = NULL WHERE session_id = $1`, session)
	return err
}
