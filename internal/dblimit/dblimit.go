// Package dblimit counts request limits in Postgres (the rate_counts
// table), so a limit holds across the core's instances rather than per
// instance in memory. A limit allows Max requests per Window for each
// subject (a client address, a link, a user), the window starting at the
// subject's first counted request; Retry-After is what's left of it.
//
// Subjects are stored only as an HMAC of the limit's name and the subject
// under the core's key, so the table holds no address or ID. Rows past
// their window are deleted by Prune (the core's expiry pass).
package dblimit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is a pool, a connection or a transaction. Counting in a
// transaction rolls back with it.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Limit is Max per Window for each subject. Name keeps limits apart: two
// limits with one name share their counts.
type Limit struct {
	Name   string
	Max    int
	Window time.Duration
}

// Counter counts limits under a key.
type Counter struct{ key []byte }

// New counts under key (the core's key for the purpose "rate-counts").
func New(key []byte) (*Counter, error) {
	if len(key) < 16 {
		return nil, errors.New("dblimit: the key is too short")
	}
	return &Counter{key: append([]byte(nil), key...)}, nil
}

func (c *Counter) id(name, subject string) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(name))
	m.Write([]byte{0})
	m.Write([]byte(subject))
	return m.Sum(nil)
}

// Add counts one request by subject under l, at now, and returns the count
// in the current window and when the window ends.
func (c *Counter) Add(ctx context.Context, q Querier, l Limit, subject string, now time.Time) (int, time.Time, error) {
	var n int
	var ends time.Time
	// Every SET expression reads the row as it was, so one CASE on its
	// expiry decides all three: a window that's over starts again at now.
	err := q.QueryRow(ctx, `INSERT INTO rate_counts (key, started_at, expires_at, n) VALUES ($1, $2, $3, 1)
		ON CONFLICT (key) DO UPDATE SET
			n = CASE WHEN rate_counts.expires_at <= $2 THEN 1 ELSE rate_counts.n + 1 END,
			started_at = CASE WHEN rate_counts.expires_at <= $2 THEN $2 ELSE rate_counts.started_at END,
			expires_at = CASE WHEN rate_counts.expires_at <= $2 THEN $3 ELSE rate_counts.expires_at END
		RETURNING n, expires_at`, c.id(l.Name, subject), now, now.Add(l.Window)).Scan(&n, &ends)
	return n, ends, err
}

// Take counts one request by subject under l and reports whether it's
// within the limit; past it, retry is how long until the window ends (at
// least a second). A refused request counts too, without extending the
// window.
func (c *Counter) Take(ctx context.Context, q Querier, l Limit, subject string, now time.Time) (ok bool, retry time.Duration, err error) {
	n, ends, err := c.Add(ctx, q, l, subject, now)
	if err != nil {
		return false, 0, err
	}
	if n <= l.Max {
		return true, 0, nil
	}
	return false, retryAfter(ends, now), nil
}

// Over reports, without counting, whether subject has used up l at now
// (Max or more in the current window), and how long until the window ends.
func (c *Counter) Over(ctx context.Context, q Querier, l Limit, subject string, now time.Time) (over bool, retry time.Duration, err error) {
	var n int
	var ends time.Time
	err = q.QueryRow(ctx, `SELECT n, expires_at FROM rate_counts WHERE key = $1 AND expires_at > $2`, c.id(l.Name, subject), now).Scan(&n, &ends)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if n < l.Max {
		return false, 0, nil
	}
	return true, retryAfter(ends, now), nil
}

func retryAfter(ends, now time.Time) time.Duration {
	secs := math.Ceil(ends.Sub(now).Seconds())
	return time.Duration(max(secs, 1)) * time.Second
}

// Prune deletes counts whose window ended before now, a batch at a time,
// and returns how many it deleted.
func Prune(ctx context.Context, q Querier, now time.Time) (int64, error) {
	var total int64
	for {
		tag, err := q.Exec(ctx, `DELETE FROM rate_counts WHERE key IN (SELECT key FROM rate_counts WHERE expires_at <= $1 LIMIT 5000)`, now)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 5000 {
			return total, nil
		}
	}
}
