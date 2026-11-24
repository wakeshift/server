package apps

// Abuse limits on making apps and keys: on top of the caps on how many
// exist at once (MaxOwned, MaxKeys), how fast they're made, so a script
// holding a console session can't churn apps or keys.
//
// Each is a count of rows in the same transaction as the change, under a
// lock that serializes the changes it counts (the account's advisory lock,
// or the app's row), so concurrent requests can't race past one, and the
// limits hold across every instance with nothing kept in memory. Apps and
// keys are never deleted (an app's row stays, with its revoked keys), so
// the counts include ones since deleted or revoked: churning is what's
// limited.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	// MaxAppsPerHour and MaxAppsPerDay: apps one account may create.
	MaxAppsPerHour = 5
	MaxAppsPerDay  = 20
	// MaxKeysPerDay: keys made for one app a day (created, rotated or
	// registered), whoever makes them.
	MaxKeysPerDay = 20
	// MaxRegistrationsPerHour: public keys one account may register an
	// hour from @wakeshift/cli's links, new apps and new keys together.
	MaxRegistrationsPerHour = 10
)

// RateLimitError is a change refused by an abuse limit; Message says
// which, for the person.
type RateLimitError struct{ Message string }

func (e *RateLimitError) Error() string { return "apps: rate limited: " + e.Message }

// lockAccount serializes user's app and key creation, so the per-account
// counts can't be raced past. Taken before any organization's or app's
// lock.
func lockAccount(ctx context.Context, tx pgx.Tx, user string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('apps.account:' || $1))`, user)
	return err
}

// checkAppRate refuses another app from user past MaxAppsPerHour or
// MaxAppsPerDay. The caller holds lockAccount.
func checkAppRate(ctx context.Context, tx pgx.Tx, user string) error {
	var hour, day int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE created_at > now() - interval '1 hour'), count(*)
		FROM apps WHERE created_by = $1 AND created_at > now() - interval '1 day'`, user).Scan(&hour, &day); err != nil {
		return err
	}
	switch {
	case hour >= MaxAppsPerHour:
		return &RateLimitError{fmt.Sprintf("You can create %d apps an hour. Try again later.", MaxAppsPerHour)}
	case day >= MaxAppsPerDay:
		return &RateLimitError{fmt.Sprintf("You can create %d apps a day. Try again tomorrow.", MaxAppsPerDay)}
	}
	return nil
}

// checkKeyRate refuses another key for app id past MaxKeysPerDay. The
// caller holds the app's row (forNewKey), or has just made the app.
func checkKeyRate(ctx context.Context, tx pgx.Tx, id string) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM app_keys WHERE app_id = $1 AND created_at > now() - interval '1 day'`, id).Scan(&n); err != nil {
		return err
	}
	if n >= MaxKeysPerDay {
		return &RateLimitError{fmt.Sprintf("An app can get %d new keys a day. Try again tomorrow.", MaxKeysPerDay)}
	}
	return nil
}

// checkRegistrationRate refuses another public-key registration by user
// past MaxRegistrationsPerHour: their keys made in the last hour whose
// app.key_created entry says it came through a registration (via). The
// caller holds lockAccount.
func checkRegistrationRate(ctx context.Context, tx pgx.Tx, user string) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM app_keys k
		JOIN app_log l ON l.app_id = k.app_id AND l.key_id = k.key_id AND l.action = $2 AND l.detail ? 'via'
		WHERE k.created_by = $1 AND k.created_at > now() - interval '1 hour'`, userActor(user), LogKeyCreated).Scan(&n); err != nil {
		return err
	}
	if n >= MaxRegistrationsPerHour {
		return &RateLimitError{fmt.Sprintf("You can register %d keys from the command line an hour. Try again later.", MaxRegistrationsPerHour)}
	}
	return nil
}
