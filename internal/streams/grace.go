package streams

// Renewing a session link that expired while nothing used it (the grace
// path).
//
// A session's link stops working SessionLinkTTL after its last use (a
// connector's, after SessionLinkTTL: it gets the next one from a poll near
// its end), so a listener that goes quiet for longer (a laptop asleep, the
// network down) used to come back to 401 and stop for good, while its
// conversation still counted as connected and its messages piled up. Now
// a request on such a link, within RenewGrace of its expiry, is answered
// as a renewal: the link's successor (same session, same position), the
// way the link would have rotated had its holder been awake. A poll gets
// 200 with no deliveries and rotate set; a wait, its hub.next line with the
// new url; a stream (WebSocket, SSE or lines), its hub.rotate frame.
//
// Only for the link that was still the session's to use:
//   - a session's link of a kind that rotates: a connector's (it redeemed a
//     ticket) or a sliding one (Monitor's and background commands');
//   - expired, within RenewGrace, and not revoked;
//   - the latest of its chain: no other link for the same agent and
//     session, with acks or without as it, from a connector or not as it,
//     was made at or after it (a rotation's successor, a newer redeem, a
//     newer command's link: whoever holds that one is the listener now);
//   - its creator still in the workspace and its grant live (loadTx), and
//     its session still watching (its row locked before the link's, so an
//     unsubscribe stopping the session and a renewal take turns: the
//     stop's revoke never misses the successor).
//
// Single use: the renewal revokes the link (revoked_at, with renewed_at
// saying why) in the transaction that mints its successor, the link's row
// locked first, so of two requests with the same link, however close,
// exactly one gets a successor; the other is refused as a revoked link is.
// Anything else keeps today's refusal (401, and hub.closed saying why).

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/audit"
)

// RenewGrace is how long after a session link expires a request on it can
// still renew it (the grace path). Links are deleted a day after they
// expire (appapi.CleanUp), so this is as long as the record lasts.
const RenewGrace = 24 * time.Hour

// errNoGrace: the link can't be renewed; the request is refused as before.
var errNoGrace = errors.New("streams: the link can't be renewed")

// renewalNote is what a wait's hub.next line and a stream's hub.rotate
// frame say on a renewal.
const renewalNote = "This command's link expired while nothing used it (the computer may have been asleep, or offline), so Wakeshift renewed it: run the command again now, as before, with this url (a new link). Nothing was missed."

// renewExpired renews a session link that expired (the grace path),
// returning its successor's URL (wss://…) and expiry and the link it
// replaced. header is the request's connector header: recorded, and with
// check, an outdated connector is refused (*UpdateRequired) as load
// refuses it. errNoGrace if the link can't be renewed.
func (s *Server) renewExpired(ctx context.Context, c claims, header string, check bool) (next string, exp time.Time, old Link, err error) {
	if c.Tenant == "" || c.ID == "" || time.Since(time.Unix(c.Exp, 0)) > RenewGrace {
		return "", time.Time{}, old, errNoGrace // its token's expiry is past any grace already
	}
	var refused error
	var logged func()
	err = s.DB.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		var l Link
		var slide *time.Time
		var revoked, expired, recent bool
		var created time.Time
		// Its session's row locked first, as an unsubscribe that stops the
		// session locks it (appapi's lockWatching) before revoking its
		// links: a renewal and that stop take turns, so the stop's revoke
		// sees the successor, or the renewal sees the session stopped.
		// Read unlocked first for the target, which the link's own lock
		// below confirms.
		var target, agent string
		err := tx.QueryRow(ctx, `SELECT target, agent_id FROM stream_links WHERE id = $1`, c.ID).Scan(&target, &agent)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoGrace
		}
		if err != nil {
			return err
		}
		if !uuidRE.MatchString(target) {
			return errNoGrace
		}
		var watching bool
		err = tx.QueryRow(ctx, `SELECT watching FROM sessions WHERE id = $1::uuid AND agent_id = $2 FOR NO KEY UPDATE`, target, agent).Scan(&watching)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !watching {
			return errNoGrace
		}
		if err != nil {
			return err
		}
		// Locked: two renewals of the same link take turns, and the second
		// finds it revoked.
		err = tx.QueryRow(ctx, `SELECT id, agent_id, target, ack, created_by, coalesce(grant_id::text, ''), expires_at, slide_until, coalesce(connector, ''),
				revoked_at IS NOT NULL, expires_at <= now(), expires_at > now() - $2::interval, created_at
			FROM stream_links WHERE id = $1 FOR UPDATE`, c.ID, RenewGrace.String()).
			Scan(&l.ID, &l.Agent, &l.Target, &l.Ack, &l.CreatedBy, &l.GrantID, &l.ExpiresAt, &slide, &l.Connector, &revoked, &expired, &recent, &created)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoGrace
		}
		if err != nil {
			return err
		}
		if slide != nil {
			l.SlideUntil = *slide
		}
		// A session's link of a kind that rotates, expired within the
		// grace, not revoked.
		if revoked || !expired || !recent || l.Target != target || l.Agent != agent || (l.Connector == "" && l.SlideUntil.IsZero()) {
			return errNoGrace
		}
		// Its creator still in the workspace, its grant live.
		if _, err := loadTx(ctx, tx, l.ID, true); errors.Is(err, errLinkInvalid) {
			return errNoGrace
		} else if err != nil {
			return err
		}
		// The latest of its chain: nothing made for the same stream, of the
		// same kind, at or after it (revoked or not).
		var superseded bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_links WHERE agent_id = $1 AND target = $2 AND ack = $3
				AND (connector IS NULL) = $4 AND id <> $5 AND created_at >= $6)`,
			l.Agent, l.Target, l.Ack, l.Connector == "", l.ID, created).Scan(&superseded); err != nil {
			return err
		}
		if superseded {
			return errNoGrace
		}
		// The connector on it, recorded; an outdated one refused (nothing
		// renewed: the updated one renews it).
		if refused, err = s.seen(ctx, tx, &l, header, check); err != nil || refused != nil {
			return err
		}
		n := NewLink{Agent: l.Agent, Target: l.Target, Ack: l.Ack, CreatedBy: l.CreatedBy, GrantID: l.GrantID, TTL: SessionLinkTTL, Connector: l.Connector}
		if !l.SlideUntil.IsZero() {
			n.Slide = SessionLinkMax
		}
		succ, err := mint(ctx, tx, n)
		if err != nil {
			return err
		}
		// Single use: renewed once, by whoever came first.
		// successor_id: a wait in loop mode on this link follows it there.
		if _, err := tx.Exec(ctx, `UPDATE stream_links SET revoked_at = now(), renewed_at = now(), successor_id = $2 WHERE id = $1`, l.ID, succ.ID); err != nil {
			return err
		}
		kind := "connector"
		if l.Connector == "" {
			kind = "session"
		}
		expiredFor := time.Since(l.ExpiresAt).Round(time.Second)
		app, _ := strings.CutPrefix(l.CreatedBy, "app:")
		if app == l.CreatedBy {
			app = ""
		}
		if err := audit.Record(ctx, tx, audit.Entry{Actor: audit.Hub, Action: audit.StreamLinkRenewed, AgentID: l.Agent, AppID: app, GrantID: l.GrantID,
			Detail: map[string]any{"session": l.Target, "link": l.ID, "successor": succ.ID, "kind": kind, "expired_seconds": int64(expiredFor.Seconds())}}); err != nil {
			return err
		}
		next, exp, old, logged = s.urlOf(c.Tenant, succ), succ.expiry(), l, func() {
			slog.Info("streams: renewed an expired link (grace)", "tenant", c.Tenant, "agent", l.Agent, "target", l.Target, "link", l.ID, "successor", succ.ID,
				"kind", kind, "expired_for", expiredFor.String())
		}
		return nil
	})
	if err == nil && refused == nil {
		logged() // never the link's token or URL
	}
	if err == nil {
		err = refused
	}
	return next, exp, old, err
}
