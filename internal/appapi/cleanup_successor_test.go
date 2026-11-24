package appapi_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/sessions"
)

// A renewed link (successor_id) is kept until a day past its token's own
// expiry, so a wait loop holding it can still follow its chain (streams,
// waitloop.go); a link nothing renewed goes a day after it expired, as
// before.
func TestCleanUpKeepsRenewedLinksForTheirChain(t *testing.T) {
	d, tenant := quizzerUser(t)
	ctx := context.Background()
	var kept, gone, followed string
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: "a", CreatedBy: "app:quizzer"})
		if err != nil {
			return err
		}
		ins := func(expired, slideUntil string, successor bool) (string, error) {
			var id string
			err := tx.QueryRow(ctx, `INSERT INTO stream_links (tenant_id, agent_id, target, created_by, expires_at, slide_until, successor_id)
				VALUES (current_tenant(), 'claude-code', $1, 'app:quizzer', now() - $2::interval, now() - $3::interval, CASE WHEN $4 THEN gen_random_uuid() END) RETURNING id`,
				s.ID, expired, slideUntil, successor).Scan(&id)
			return id, err
		}
		if kept, err = ins("30 hours", "2 hours", true); err != nil { // renewed; its token expired 2h ago: kept
			return err
		}
		if followed, err = ins("50 hours", "26 hours", true); err != nil { // renewed; token past a day: gone
			return err
		}
		gone, err = ins("30 hours", "2 hours", false) // not renewed: gone
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, _, err := appapi.CleanUp(ctx, tx, 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	exists := func(id string) bool {
		var ok bool
		d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_links WHERE id = $1)`, id).Scan(&ok)
		})
		return ok
	}
	if !exists(kept) || exists(gone) || exists(followed) {
		t.Fatalf("kept %v, gone %v, past its chain %v", exists(kept), exists(gone), exists(followed))
	}
}
