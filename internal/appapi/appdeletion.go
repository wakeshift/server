package appapi

// Deleting an app deletes what Wakeshift keeps for its users
// (054_app_deletions.sql). apps.Registry.Delete records the deletion in
// app_deletions in the same transaction as the app's; FinishAppDeletions,
// run by the expiry pass every few minutes, does it and retries what fails
// on the next pass, well inside apps.DataDeletion:
//
//   - each workspace the app kept for its users (conversations' included) is
//     erased as forgetting the user would, with its daily usage counts
//     (usage_daily), and once they're all gone so are any counts left
//     under the app's ID;
//   - in every other workspace (people who connected the app to their own
//     agents), the app's grants are revoked, then they and what the app
//     made there are deleted: its subscriptions, its events and their
//     deliveries, webhooks, connector tickets, stream links
//     and sign-ins. The workspace's agents and conversations are its
//     owner's and stay, as does its audit log, which records the deletion
//     (audit.AppDataDeleted);
//   - the app's recent publishes, topic memberships, publish URLs
//     and subscription IDs.
//
// The app's own log stays readable by its owner for apps.LogRetention
// after the deletion, then it's deleted too.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/streams"
)

// AppDeletionPass is what one FinishAppDeletions pass did.
type AppDeletionPass struct {
	Finished   int // apps whose users' data is all gone now
	Workspaces int // workspaces erased or cleaned
	Logs       int // deleted apps' logs deleted
}

// FinishAppDeletions carries on deleting deleted apps' users' data, erasing
// at most limit of the apps' own workspaces and visiting at most sweep
// other workspaces in one pass (the rest wait for the next, which carries
// on where this one stopped), and deletes the logs of apps deleted over apps.LogRetention
// ago. A failure is recorded against its app (attempts, last_error) and
// retried on the next pass; the other apps carry on.
func FinishAppDeletions(ctx context.Context, d *db.DB, limit, sweep int) (AppDeletionPass, error) {
	var pass AppDeletionPass
	// An app deleted by a revision from before app_deletions has no row:
	// give it one.
	if _, err := d.Pool.Exec(ctx, `INSERT INTO app_deletions (app_id, requested_at)
		SELECT id, deleted_at FROM apps WHERE deleted_at IS NOT NULL ON CONFLICT DO NOTHING`); err != nil {
		return pass, err
	}
	// Round-robin: the deletion worked on least recently goes first, so one
	// big app's using up a pass's budget doesn't hold the others back.
	rows, _ := d.Pool.Query(ctx, `SELECT app_id FROM app_deletions WHERE data_deleted_at IS NULL
		ORDER BY last_attempt_at NULLS FIRST, requested_at`)
	pending, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return pass, err
	}
	var failed error
	for _, app := range pending {
		if ctx.Err() != nil {
			return pass, ctx.Err()
		}
		done, n, err := finishAppDeletion(ctx, d, app, &limit, &sweep)
		pass.Workspaces += n
		if err != nil {
			failed = cmp.Or(failed, fmt.Errorf("app %s: %w", app, err))
			if _, rerr := d.Pool.Exec(ctx, `UPDATE app_deletions SET attempts = attempts + 1, last_attempt_at = now(), last_error = left($2, 500)
				WHERE app_id = $1`, app, err.Error()); rerr != nil && ctx.Err() == nil {
				failed = cmp.Or(failed, rerr)
			}
			continue
		}
		if done {
			pass.Finished++
		} else if _, err := d.Pool.Exec(ctx, `UPDATE app_deletions SET last_attempt_at = now() WHERE app_id = $1`, app); err != nil && ctx.Err() == nil {
			failed = cmp.Or(failed, err)
		}
		if limit <= 0 || sweep <= 0 {
			break // the rest next pass
		}
	}
	n, err := deleteAppLogs(ctx, d)
	pass.Logs = n
	return pass, cmp.Or(failed, err)
}

// finishAppDeletion deletes what's left of app's users' data, erasing at
// most *budget of the app's workspaces (and taking them off it): done when
// it's all gone. n is how many workspaces it erased or cleaned.
func finishAppDeletion(ctx context.Context, d *db.DB, app string, budget, sweep *int) (done bool, n int, err error) {
	// What spans workspaces first: its publishes with their
	// copies still to make, so nothing of the app's is copied into a
	// workspace after the sweep below has been there.
	err = pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			`DELETE FROM topic_members WHERE app_id = $1`,
			`DELETE FROM topic_events WHERE app_id = $1`, // and their copies still to make
			`DELETE FROM topic_publish_urls WHERE app_id = $1`,
			`DELETE FROM app_emit_counters WHERE app_id = $1`,
			`DELETE FROM pending_addition_ids WHERE app_id = $1`,
			`DELETE FROM subscribe_vouchers WHERE app_id = $1`,
			`DELETE FROM subscribe_voucher_counters WHERE app_id = $1`,
			`DELETE FROM connector_call_claims WHERE app_id = $1`, // redeem by call: callredeem.go
			`DELETE FROM connector_call_index WHERE app_id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, app); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, 0, fmt.Errorf("deleting its publishes: %w", err)
	}

	// The workspaces the app kept for its users, conversations' included.
	rows, _ := d.Pool.Query(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 ORDER BY id LIMIT $2`, app, *budget+1)
	own, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, 0, err
	}
	for i, tenant := range own {
		if i == *budget {
			*budget = 0
			return false, n, nil // more than this pass erases
		}
		erased := false
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			held, _, err := holdAppWorkspace(ctx, tx, app)
			if err != nil || !held {
				return err // or forgotten meanwhile
			}
			if err := eraseAndClose(ctx, tx, tenant); err != nil {
				return err
			}
			erased = true
			_, err = tx.Exec(ctx, `UPDATE app_deletions SET workspaces_erased = workspaces_erased + 1 WHERE app_id = $1`, app)
			return err
		})
		if err != nil {
			return false, n, fmt.Errorf("erasing one of its workspaces: %w", err)
		}
		if erased {
			n++
		}
	}
	*budget -= len(own)

	// Everyone else's workspaces: what the app made there. Grants and copies
	// of the app's events are inside each, where only the workspace's own
	// transaction sees them, so this visits them all, in ID order from where
	// the last pass stopped (swept_to, moved on in each workspace's own
	// transaction), at most *sweep of them per pass. A workspace made after
	// the sweep passed its ID can't hold anything of the app's: a deleted
	// app can't act, and its keys and grants are revoked.
	for {
		var from *string
		if err := d.Pool.QueryRow(ctx, `SELECT swept_to::text FROM app_deletions WHERE app_id = $1`, app).Scan(&from); err != nil {
			return false, n, err
		}
		if *sweep <= 0 {
			return false, n, nil // the rest next pass
		}
		rows, _ := d.Pool.Query(ctx, `SELECT id::text FROM tenants WHERE app_id IS DISTINCT FROM $1 AND ($2::uuid IS NULL OR id > $2::uuid)
			ORDER BY id LIMIT $3`, app, from, min(*sweep, 500))
		tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return false, n, err
		}
		if len(tenants) == 0 {
			break
		}
		for _, tenant := range tenants {
			if ctx.Err() != nil {
				return false, n, ctx.Err()
			}
			var deleted int64
			err := d.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
				if deleted, err = purgeApp(ctx, tx, app, "app_deleted"); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE app_deletions SET swept_to = $2::uuid WHERE app_id = $1`, app, tenant)
				return err
			})
			if err != nil {
				return false, n, fmt.Errorf("deleting its data in a workspace: %w", err)
			}
			*sweep--
			if deleted > 0 {
				n++
			}
		}
	}

	// The record that it's done.
	err = pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var erased int
		var requested time.Time
		err := tx.QueryRow(ctx, `SELECT workspaces_erased, requested_at FROM app_deletions WHERE app_id = $1 AND data_deleted_at IS NULL FOR UPDATE`, app).Scan(&erased, &requested)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // finished meanwhile
		}
		if err != nil {
			return err
		}
		var left bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE app_id = $1)`, app).Scan(&left); err != nil || left {
			// One made meanwhile: the next pass erases it first (the app's own
			// workspaces are listed afresh each pass, not swept).
			return err
		}
		// Its users' daily usage counts: erasing each workspace deleted its
		// own, and this the rest (any left by a workspace erased before
		// erasing deleted them).
		if _, err := tx.Exec(ctx, `DELETE FROM usage_daily WHERE app_id = $1`, app); err != nil {
			return err
		}
		// Its keys: deleting the app deleted them, but a previous revision
		// deleting it while both served only revoked them.
		if _, err := apps.DeleteKeys(ctx, tx, app); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE app_deletions SET data_deleted_at = now(), last_attempt_at = now(), last_error = NULL WHERE app_id = $1`, app); err != nil {
			return err
		}
		done = true
		return apps.Log(ctx, tx, apps.LogEntry{AppID: app, Actor: audit.Hub, Action: apps.LogUsersDataDeleted,
			Detail: map[string]any{"workspaces_erased": erased, "log_deleted_after": requested.Add(apps.LogRetention).UTC().Format(time.RFC3339)}})
	})
	return done, n, err
}

// purgeApp deletes, in the transaction's workspace (one the app didn't
// keep), what the app made there, because it was deleted (reason
// "app_deleted"): its grants (revoked first, which
// ends what they allowed and records it), its subscriptions, its events
// and every delivery of them, webhooks, connector
// tickets, stream links and sign-ins. It records what it deleted in the
// workspace's audit log; n is how many rows went.
func purgeApp(ctx context.Context, tx pgx.Tx, app, reason string) (n int64, err error) {
	rows, _ := tx.Query(ctx, `SELECT id::text FROM grants WHERE app_id = $1 AND revoked_at IS NULL`, app)
	live, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, id := range live {
		if _, err := grants.Revoke(ctx, tx, id, audit.Hub); err != nil {
			return 0, err
		}
	}
	// The app's subscriptions: made by it, under its grants, or to its
	// event types and topics.
	rows, _ = tx.Query(ctx, `SELECT id::text FROM subscriptions s
		WHERE created_by = 'app:' || $1 OR grant_id IN (SELECT id FROM grants WHERE app_id = $1)
			OR event_type = 'topics.' || $1 OR starts_with(event_type, 'app.' || $1 || '.')
			OR id IN (SELECT subscription_id FROM topic_subscriptions WHERE app_id = $1)
			OR id IN (SELECT subscription_id FROM event_subscriptions WHERE source_kind = 'app' AND source_id = $1)`, app)
	subs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	// Wait for a pipeline runner mid-step on one of them, as eraseWorkspace
	// does, so nothing it writes lands after.
	if _, err := tx.Exec(ctx, `UPDATE subscription_cursors SET updated_at = now() WHERE subscription_id = ANY($1::uuid[])`, subs); err != nil {
		return 0, err
	}
	// Its events: from it, of its types, or about it (Wakeshift's notices
	// naming it).
	const appEvents = `SELECT id FROM events WHERE source = 'app:' || $1 OR type = 'topics.' || $1
		OR starts_with(type, 'app.' || $1 || '.') OR attrs->>'app' = $1`
	counts := map[string]int64{}
	// del runs one deletion from table, counting what went.
	del := func(table, sql string, args ...any) error {
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("deleting %s: %w", table, err)
		}
		if k := tag.RowsAffected(); k > 0 {
			counts[table] = k
			n += k
		}
		return nil
	}
	// Webhooks first, with the deliverer's note of them.
	rows, _ = tx.Query(ctx, `DELETE FROM mcp_event_webhooks WHERE app_id = $1 RETURNING id`, app)
	webhooks, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("deleting mcp_event_webhooks: %w", err)
	}
	if len(webhooks) > 0 {
		counts["mcp_event_webhooks"] = int64(len(webhooks))
		n += int64(len(webhooks))
		if _, err := tx.Exec(ctx, `DELETE FROM mcp_event_targets WHERE tenant_id = current_tenant() AND webhook_id = ANY($1)`, webhooks); err != nil {
			return 0, err
		}
	}
	// Agent-hosted webhook destinations registered through its results (the
	// pass removes their targets).
	if gone, err := streams.RemoveAgentWebhooks(ctx, tx, "hub", "app_deleted", `w.app_id = $1`, app); err != nil {
		return 0, fmt.Errorf("deleting agent_webhooks: %w", err)
	} else if gone > 0 {
		counts["agent_webhooks"] = int64(gone)
		n += int64(gone)
	}
	// Grok Bot's webhook setups, challenges and keys made through its
	// results (the destinations' index rows go with them).
	for _, table := range []string{"agent_webhook_setups", "bot_challenges", "bot_keys"} {
		if err := del(table, `DELETE FROM `+table+` WHERE app_id = $1`, app); err != nil {
			return 0, err
		}
	}
	// Deliveries before their subscriptions and events (foreign keys);
	// what refers to a subscription before it.
	if err := del("deliveries", `DELETE FROM deliveries WHERE subscription_id = ANY($2::uuid[]) OR event_id IN (`+appEvents+`)`, app, subs); err != nil {
		return 0, err
	}
	if err := del("subscription_cursors", `DELETE FROM subscription_cursors WHERE subscription_id = ANY($1::uuid[])`, subs); err != nil {
		return 0, err
	}
	if err := del("event_subscriptions", `DELETE FROM event_subscriptions WHERE subscription_id = ANY($2::uuid[]) OR (source_kind = 'app' AND source_id = $1)`, app, subs); err != nil {
		return 0, err
	}
	if err := del("topic_subscriptions", `DELETE FROM topic_subscriptions WHERE subscription_id = ANY($2::uuid[]) OR app_id = $1`, app, subs); err != nil {
		return 0, err
	}
	if err := del("subscriptions", `DELETE FROM subscriptions WHERE id = ANY($1::uuid[])`, subs); err != nil {
		return 0, err
	}
	if err := del("events", `DELETE FROM events WHERE id IN (`+appEvents+`)`, app); err != nil {
		return 0, err
	}
	if err := del("pending_additions", `DELETE FROM pending_additions WHERE app_id = $1`, app); err != nil {
		return 0, err
	}
	if err := del("connector_tickets", `DELETE FROM connector_tickets WHERE app_id = $1`, app); err != nil {
		return 0, err
	}
	if err := del("session_setups", `DELETE FROM session_setups WHERE app_id = $1`, app); err != nil {
		return 0, err
	}
	if err := del("client_requests", `DELETE FROM client_requests WHERE app_id = $1`, app); err != nil {
		return 0, err
	}
	// Stream links before the grants they name.
	if err := del("stream_links", `DELETE FROM stream_links WHERE created_by = 'app:' || $1 OR grant_id IN (SELECT id FROM grants WHERE app_id = $1)`, app); err != nil {
		return 0, err
	}
	// Sign-ins: the app's own (client app:<id>), and every one to its MCP
	// server, granted or not: the app is gone. Not if another app that isn't
	// deleted has the same MCP URL: those sign-ins are that app's too.
	const signIns = `client_id = 'app:' || $1 OR (resource = (SELECT mcp_url FROM apps WHERE id = $1)
		AND NOT EXISTS (SELECT 1 FROM apps o WHERE o.id <> $1 AND o.mcp_url = resource AND o.deleted_at IS NULL))`
	if err := del("oauth_refresh_tokens", `DELETE FROM oauth_refresh_tokens WHERE `+signIns, app); err != nil {
		return 0, err
	}
	if err := del("oauth_codes", `DELETE FROM oauth_codes WHERE `+signIns, app); err != nil {
		return 0, err
	}
	if err := del("grant_paces", `DELETE FROM grant_paces WHERE grant_id IN (SELECT id FROM grants WHERE app_id = $1)`, app); err != nil {
		return 0, err
	}
	if err := del("grants", `DELETE FROM grants WHERE app_id = $1`, app); err != nil {
		return 0, err
	}
	if n == 0 && len(live) == 0 {
		return 0, nil
	}
	return n + int64(len(live)), audit.Record(ctx, tx, audit.Entry{Actor: audit.Hub, Action: audit.AppDataDeleted, AppID: app,
		Detail: map[string]any{"reason": reason, "grants_revoked": len(live), "deleted": counts}})
}

// deleteAppLogs deletes the logs of apps deleted over apps.LogRetention
// ago whose users' data is gone: how many.
func deleteAppLogs(ctx context.Context, d *db.DB) (int, error) {
	rows, _ := d.Pool.Query(ctx, `SELECT app_id FROM app_deletions
		WHERE data_deleted_at IS NOT NULL AND log_deleted_at IS NULL AND requested_at <= now() - $1::interval`, apps.LogRetention.String())
	due, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, app := range due {
		err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `DELETE FROM app_log WHERE app_id = $1`, app); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE app_deletions SET log_deleted_at = now() WHERE app_id = $1`, app)
			return err
		})
		if err != nil {
			return n, fmt.Errorf("deleting app %s's log: %w", app, err)
		}
		n++
	}
	return n, nil
}
