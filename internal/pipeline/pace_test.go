package pipeline

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/subscriptions"
)

// scheduleBySQL is how pacing scheduled before it read the grant's window
// once a step: the same rule, asked of the database for every target, over
// every subscription the grant has had. The reference pace.next must
// match.
func scheduleBySQL(tx pgx.Tx, grant string, limits []apps.WakeLimit, now time.Time) (time.Time, error) {
	const grantDeliveries = `FROM deliveries d JOIN subscriptions s ON s.tenant_id = d.tenant_id AND s.id = d.subscription_id WHERE s.grant_id = $1`
	t := now
	var latest *time.Time
	if err := tx.QueryRow(ctx, `SELECT max(d.available_at) `+grantDeliveries+` AND d.available_at > $2`, grant, now).Scan(&latest); err != nil {
		return t, err
	}
	if latest != nil {
		t = *latest
	}
	for range 20 {
		moved := false
		for _, l := range limits {
			w := time.Duration(l.Seconds) * time.Second
			var nth time.Time
			err := tx.QueryRow(ctx, `SELECT d.available_at `+grantDeliveries+` AND d.available_at > $2 AND d.available_at <= $3
				ORDER BY d.available_at DESC OFFSET $4 LIMIT 1`, grant, t.Add(-w), t, l.Max-1).Scan(&nth)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return t, err
			}
			t = nth.Add(w + time.Millisecond)
			moved = true
		}
		if !moved {
			break
		}
	}
	return t, nil
}

// Pacing schedules exactly as it did when it asked the database for every
// target: over grants with wake-ups past, scheduled ahead and spread over
// several subscriptions (one of them ended), for tight limits that move
// the schedule, the platform's defaults alone, and a limit larger than the
// grant's history. Each scheduled wake-up is delivered before the next is
// scheduled, as in a step.
func TestPacingSchedulesAsBefore(t *testing.T) {
	cases := []struct {
		name   string
		limits string
		seeded int
	}{
		{"tight", `[{"seconds": 1, "max": 2}, {"seconds": 10, "max": 5}, {"seconds": 60, "max": 8}]`, 40},
		{"one an hour", `[{"seconds": 3600, "max": 1}]`, 3},
		{"defaults", `[]`, 60},
		{"bursts", `[{"seconds": 5, "max": 3}, {"seconds": 120, "max": 20}]`, 25},
		{"room", `[{"seconds": 86400, "max": 1000}]`, 10},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			grant, s1 := f.grant("claude-code", c.limits, time.Hour, map[string]string{"channel": "C1"})
			rng := rand.New(rand.NewPCG(uint64(i), 7))
			var subs []string
			f.tx(func(tx pgx.Tx) error {
				subs = append(subs, s1.ID)
				for range 3 {
					s, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "slack.message",
						CreatedBy: "app:pacer", GrantID: grant})
					if err != nil {
						return err
					}
					subs = append(subs, s.ID)
				}
				_, err := subscriptions.Cancel(ctx, tx, subs[3])
				return err
			})
			e := f.slack("e1", "C1")
			// The grant's history: wake-ups over the last two minutes and
			// up to two minutes ahead, on all its subscriptions. A user's
			// own subscription (no grant) adds some that mustn't count.
			own := f.subscribe("claude-code", "slack.message", nil)
			n := 0
			insert := func(tx pgx.Tx, sub string, at time.Time) error {
				n++
				_, err := tx.Exec(ctx, `INSERT INTO deliveries (tenant_id, id, agent_id, target, event_id, event_seq, subscription_id, available_at)
					VALUES (current_tenant(), gen_random_uuid(), 'claude-code', $1, $2, $3, $4, $5)`, fmt.Sprintf("t%d", n), e.ID, e.Seq, sub, at)
				return err
			}
			f.tx(func(tx pgx.Tx) error {
				for range c.seeded {
					at := time.Now().Add(time.Duration(rng.Int64N(int64(4*time.Minute))) - 2*time.Minute)
					if err := insert(tx, subs[rng.IntN(len(subs))], at); err != nil {
						return err
					}
				}
				for range 20 {
					if err := insert(tx, own.ID, time.Now().Add(time.Duration(rng.Int64N(int64(time.Minute))))); err != nil {
						return err
					}
				}
				return nil
			})

			f.tx(func(tx pgx.Tx) error {
				p, err := pacing(ctx, tx, subs[0], map[string]*pace{})
				if err != nil {
					return err
				}
				if err := p.load(ctx, tx); err != nil {
					return err
				}
				now := p.dbNow
				for k := range 30 {
					now = now.Add(time.Duration(rng.Int64N(int64(400 * time.Millisecond)))).Truncate(time.Microsecond)
					got := p.next(now)
					want, err := scheduleBySQL(tx, grant, p.limits, now)
					if err != nil {
						return err
					}
					if !got.Equal(want) {
						t.Fatalf("wake-up %d at %v: scheduled for %v, before for %v", k, now, got, want)
					}
					if got.After(now.Add(maxDelay)) {
						continue // passed over: not delivered
					}
					if err := insert(tx, subs[k%3], got); err != nil {
						return err
					}
					if err := p.record(ctx, tx, got); err != nil {
						return err
					}
				}
				return nil
			})
		})
	}
}

// A grant's window is read by the grant's own index: it doesn't join the
// grant's subscriptions (once one index probe each, ended ones included),
// so its cost doesn't grow with how many the grant has had.
func TestPaceWindowReadsByGrant(t *testing.T) {
	f := setup(t)
	grant, _ := f.grant("claude-code", `[]`, time.Hour, nil)
	f.tx(func(tx pgx.Tx) error {
		conds, plan := dbtest.IndexConds(t, tx, "deliveries", paceWindowSQL, grant, time.Now().Add(-time.Hour), 3600)
		if len(conds) == 0 {
			t.Fatalf("the window doesn't read deliveries: %s", plan)
		}
		for _, c := range conds {
			if !strings.Contains(c, "grant_id = ") {
				t.Errorf("the window reads deliveries without grant_id in its index condition: %q\nplan: %s", c, plan)
			}
		}
		if strings.Contains(string(plan), `"subscriptions"`) {
			t.Errorf("the window reads subscriptions: %s", plan)
		}
		return nil
	})
}

// A delivery carries its subscription's grant (the deliveries_grant
// trigger), however it's inserted; a user's own subscription's carries
// none.
func TestDeliveriesCarryTheirGrant(t *testing.T) {
	f := setup(t)
	grant, s := f.grant("claude-code", `[]`, time.Hour, nil)
	own := f.subscribe("codex", "slack.message", nil)
	e := f.slack("e1", "C1")
	f.tx(func(tx pgx.Tx) error {
		for _, sub := range []subscriptions.Subscription{s, own} {
			if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: sub.AgentID, SubscriptionID: sub.ID, Event: e}); err != nil {
				return err
			}
		}
		rows, _ := tx.Query(ctx, `SELECT agent_id || '=' || coalesce(grant_id::text, '') FROM deliveries ORDER BY agent_id`)
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if want := []string{"claude-code=" + grant, "codex="}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("deliveries' grants %v, want %v", got, want)
		}
		return nil
	})
}

// End to end: a step's wake-ups under a grant stay within its limits, for
// targets across events and the grant's subscriptions, scheduled in one
// step.
func TestStepKeepsAGrantsLimits(t *testing.T) {
	f := setup(t)
	grant, _ := f.grant("claude-code", `[{"seconds": 60, "max": 3}]`, time.Hour, map[string]string{"channel": "C1"})
	f.tx(func(tx pgx.Tx) error {
		_, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "slack.message",
			Filters: map[string]string{"channel": "C2"}, CreatedBy: "app:pacer", GrantID: grant})
		return err
	})
	for i := range 4 {
		f.slack(fmt.Sprintf("a%d", i), "C1")
		f.slack(fmt.Sprintf("b%d", i), "C2")
	}
	if n, err := (&Runner{DB: f.d}).Tenant(ctx, f.tenant); err != nil || n != 8 {
		t.Fatalf("created %d, %v", n, err)
	}
	var ats []time.Time
	f.tx(func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT available_at FROM deliveries ORDER BY available_at`)
		var err error
		ats, err = pgx.CollectRows(rows, pgx.RowTo[time.Time])
		return err
	})
	// Any 3 in a row, and the 4th is a minute past the first.
	for i := 3; i < len(ats); i++ {
		if ats[i].Sub(ats[i-3]) <= time.Minute {
			t.Fatalf("4 wake-ups within a minute: %v", ats)
		}
	}
}
