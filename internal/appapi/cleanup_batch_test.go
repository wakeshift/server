package appapi_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/sessions"
)

// A backlog bigger than one batch drains: CleanUp and PrunePendingAdditions
// delete batch at a time, several batches a pass (at most 20), so a
// workspace that piled up tickets, links and pending-addition records
// doesn't fail every pass on the job's statement timeout. What hasn't
// expired long enough stays.
func TestCleanUpAndPruneDrainABacklogInBatches(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	const batch, backlog = 3, 3*20 + 2 // more than a pass takes
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: "a", CreatedBy: "app:quizzer"})
		if err != nil {
			return err
		}
		for _, q := range []string{
			`INSERT INTO connector_tickets (tenant_id, token_hash, session_id, app_id, grant_id, expires_at)
				SELECT current_tenant(), sha256(convert_to($1 || i::text, 'UTF8')), $2, 'quizzer', gen_random_uuid(), now() - $3::interval
				FROM generate_series(1, $4) i`,
			`INSERT INTO stream_links (tenant_id, agent_id, target, created_by, expires_at)
				SELECT current_tenant(), 'claude-code', $2, 'app:quizzer', now() - $3::interval FROM generate_series(1, $4) i WHERE $1 <> ''`,
			`INSERT INTO pending_additions (tenant_id, id, app_id, session_id, agent_id, mode, first, expires_at)
				SELECT current_tenant(), 'pa_' || $1 || i::text, 'quizzer', $2, 'claude-code', 'topic', true, now() - $3::interval FROM generate_series(1, $4) i`,
		} {
			// The backlog, and one of each not expired long enough.
			if _, err := tx.Exec(ctx, q, "old", s.ID, "3 days", backlog); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, q, "new", s.ID, "1 second", 1); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO pending_addition_ids (id, app_id, tenant_id) SELECT id, 'quizzer', $1 FROM unnest($2::text[]) id`,
		tenant, append(ids("pa_old", backlog), "pa_new1")); err != nil {
		t.Fatal(err)
	}
	pass := func() (tickets, links, records int64) {
		t.Helper()
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 30000`); err != nil {
				return err
			}
			var err error
			if tickets, links, err = appapi.CleanUp(ctx, tx, batch); err != nil {
				return err
			}
			records, err = appapi.PrunePendingAdditions(ctx, tx, batch)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	// A pass takes 20 batches of each; the next, the rest.
	if tk, l, r := pass(); tk != batch*20 || l != batch*20 || r != batch*20 {
		t.Fatalf("the first pass deleted %d tickets, %d links, %d records; want %d of each", tk, l, r, batch*20)
	}
	if tk, l, r := pass(); tk != 2 || l != 2 || r != 2 {
		t.Fatalf("the second pass deleted %d tickets, %d links, %d records; want 2 of each", tk, l, r)
	}
	if tk, l, r := pass(); tk != 0 || l != 0 || r != 0 {
		t.Fatalf("a pass with nothing left deleted %d, %d, %d", tk, l, r)
	}
	var tickets, links, records, recordIDs int
	err = d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM connector_tickets), (SELECT count(*) FROM stream_links), (SELECT count(*) FROM pending_additions)`).
			Scan(&tickets, &links, &records)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pending_addition_ids WHERE tenant_id = $1`, tenant).Scan(&recordIDs); err != nil {
		t.Fatal(err)
	}
	if tickets != 1 || links != 1 || records != 1 || recordIDs != 1 {
		t.Fatalf("left %d tickets, %d links, %d records, %d record IDs; want the one recent of each", tickets, links, records, recordIDs)
	}
}

// ids are prefix1 … prefixN.
func ids(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + strconv.Itoa(i+1)
	}
	return out
}
