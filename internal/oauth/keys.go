package oauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// keyLock is the advisory lock key held while creating a signing key, so
// instances starting together create one, not several.
const keyLock = 0x6f617574 // "oaut"

// keyRotation is how long a key signs new tokens before a new one replaces
// it. Old keys stay in JWKS (verifying tokens they signed) until retired.
const keyRotation = 90 * 24 * time.Hour

// keyRetireAfter is how long a key that stopped signing still verifies:
// far longer than anything it signed lives (access tokens, an hour; agent
// tokens and beacons, minutes). Then it's retired: JWKS stops publishing
// it and nothing it signed verifies.
const keyRetireAfter = 24 * time.Hour

// keyWarmup is how long a new key is published before it signs: longer
// than the key caches here (5 minutes) and JWKS's max-age (300s).
const keyWarmup = 10 * time.Minute

type signingKey struct {
	kid     string
	private ed25519.PrivateKey
	public  ed25519.PublicKey
	created time.Time
}

// keyStore loads signing keys from the database, creating one when there's
// none recent enough.
type keyStore struct {
	pool *pgxpool.Pool
	seal cipher.AEAD // encrypts private keys at rest

	mu         sync.Mutex
	loaded     time.Time
	lastTry    time.Time    // reloadFor's last reload, for its rate limit
	all        []signingKey // not retired, newest first
	refreshing bool         // a background reload is running
}

func newKeyStore(pool *pgxpool.Pool, secret []byte) (*keyStore, error) {
	block, err := aes.NewCipher(secret[:32])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &keyStore{pool: pool, seal: aead}, nil
}

// signer returns the key that signs new tokens. Only the first call reads
// the database (the core loads the keys at start: LoadKeys); after that
// they're reloaded in the background when due, so signing never takes a
// pool connection. Callers sign inside their transactions, and a second
// connection taken while holding one can wait forever once every
// connection is held that way.
func (k *keyStore) signer(ctx context.Context) (signingKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.all) == 0 {
		if err := k.loadLocked(ctx); err != nil {
			return signingKey{}, err
		}
	} else if time.Since(k.loaded) >= 5*time.Minute || time.Since(k.all[0].created) >= keyRotation {
		k.refreshLocked()
	}
	return signerOf(k.all), nil
}

// signerOf is the key of keys (newest first) that signs: a new key signs
// only once it's had time to reach the other instances' (and apps') caches;
// until then the previous one does.
func signerOf(keys []signingKey) signingKey {
	if len(keys) > 1 && time.Since(keys[0].created) < keyWarmup {
		return keys[1]
	}
	return keys[0]
}

// refreshLocked reloads the keys in the background, unless that's running.
func (k *keyStore) refreshLocked() {
	if k.refreshing {
		return
	}
	k.refreshing = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		keys, err := k.load(ctx)
		k.mu.Lock()
		defer k.mu.Unlock()
		k.refreshing = false
		if err != nil {
			slog.Warn("oauth: reloading signing keys", "err", err)
			return
		}
		k.all, k.loaded = keys, time.Now()
	}()
}

// reloadFor reloads the keys when a token names a key this instance hasn't
// seen (another instance just made it), at most every few seconds.
// The database is read without holding mu, so signing doesn't wait for it.
func (k *keyStore) reloadFor(ctx context.Context, kid string) {
	k.mu.Lock()
	for _, key := range k.all {
		if key.kid == kid {
			k.mu.Unlock()
			return
		}
	}
	if time.Since(k.lastTry) < 5*time.Second {
		k.mu.Unlock()
		return
	}
	k.lastTry = time.Now()
	k.mu.Unlock()
	keys, err := k.load(ctx)
	if err != nil {
		slog.Warn("oauth: reloading signing keys", "err", err)
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.all, k.loaded = keys, time.Now()
}

// public returns every unretired key, for JWKS.
func (k *keyStore) public(ctx context.Context) ([]signingKey, error) {
	if _, err := k.signer(ctx); err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]signingKey(nil), k.all...), nil
}

func (k *keyStore) loadLocked(ctx context.Context) error {
	keys, err := k.load(ctx)
	if err == nil {
		k.all, k.loaded = keys, time.Now()
	}
	return err
}

// load reads the unretired keys, newest first, creating one if none is
// recent enough.
func (k *keyStore) load(ctx context.Context) (keys []signingKey, err error) {
	err = pgx.BeginFunc(ctx, k.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, keyLock); err != nil {
			return err
		}
		if keys, err = k.read(ctx, tx); err != nil {
			return err
		}
		if len(keys) == 0 || time.Since(keys[0].created) >= keyRotation {
			key, err := k.create(ctx, tx)
			if err != nil {
				return err
			}
			keys = append([]signingKey{key}, keys...)
		}
		keys, err = retire(ctx, tx, keys, time.Now())
		return err
	})
	return keys, err
}

// retire retires the keys (newest first) that stopped signing more than
// keyRetireAfter ago: those older than the newest key that has been signing
// (past its warmup) for that long. It returns the keys left.
func retire(ctx context.Context, tx pgx.Tx, keys []signingKey, now time.Time) ([]signingKey, error) {
	for i, key := range keys {
		if now.Sub(key.created) < keyWarmup+keyRetireAfter {
			continue
		}
		if i+1 == len(keys) {
			return keys, nil
		}
		old := make([]string, 0, len(keys)-i-1)
		for _, o := range keys[i+1:] {
			old = append(old, o.kid)
		}
		if _, err := tx.Exec(ctx, `UPDATE oauth_signing_keys SET retired_at = now() WHERE kid = ANY($1) AND retired_at IS NULL`, old); err != nil {
			return nil, err
		}
		slog.Info("oauth: retired signing keys", "kids", old)
		return keys[:i+1], nil
	}
	return keys, nil
}

// read returns unretired keys, newest first.
func (k *keyStore) read(ctx context.Context, tx pgx.Tx) ([]signingKey, error) {
	rows, err := tx.Query(ctx, `SELECT kid, public_key, private_key, created_at FROM oauth_signing_keys
		WHERE retired_at IS NULL ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []signingKey
	for rows.Next() {
		var kid string
		var pub, sealed []byte
		var created time.Time
		if err := rows.Scan(&kid, &pub, &sealed, &created); err != nil {
			return nil, err
		}
		priv, err := k.open(kid, sealed)
		if err != nil {
			// Unusable (WAKESHIFT_SECRET_KEY changed): skip it; a new key is
			// made if none is left. Tokens it signed stop verifying.
			slog.Warn("oauth: skipping a signing key", "kid", kid, "err", err)
			continue
		}
		out = append(out, signingKey{kid: kid, private: priv, public: pub, created: created})
	}
	return out, rows.Err()
}

func (k *keyStore) create(ctx context.Context, tx pgx.Tx) (signingKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return signingKey{}, err
	}
	sum := sha256.Sum256(pub)
	kid := hex.EncodeToString(sum[:8])
	sealed := k.close(kid, priv)
	var created time.Time
	err = tx.QueryRow(ctx, `INSERT INTO oauth_signing_keys (kid, public_key, private_key) VALUES ($1, $2, $3) RETURNING created_at`,
		kid, []byte(pub), sealed).Scan(&created)
	return signingKey{kid: kid, private: priv, public: pub, created: created}, err
}

// close and open seal a private key, bound to its key ID.
func (k *keyStore) close(kid string, priv ed25519.PrivateKey) []byte {
	nonce := make([]byte, k.seal.NonceSize())
	rand.Read(nonce)
	return k.seal.Seal(nonce, nonce, priv, []byte(kid))
}

func (k *keyStore) open(kid string, sealed []byte) (ed25519.PrivateKey, error) {
	n := k.seal.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed key too short")
	}
	priv, err := k.seal.Open(nil, sealed[:n], sealed[n:], []byte(kid))
	if err != nil {
		return nil, errors.New("can't decrypt (was WAKESHIFT_SECRET_KEY changed?)")
	}
	return priv, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
