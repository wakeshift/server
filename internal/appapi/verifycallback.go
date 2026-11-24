package appapi

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dblimit"
	"github.com/wakeshift/server/internal/mcpevents"
)

// Verification POSTs are rate-limited per destination (SEP-3415, Webhook
// security, Endpoint verification: they SHOULD be), counted across the
// core's instances (rate_counts), in layers:
//
//   - VerifyPerPrincipalHost: one principal (an app's user) at one host.
//     A principal sending failing subscribes at a shared callback host
//     (ChatGPT's) uses up its own count, not the host's for everyone. A
//     principal's later subscriptions to a URL it verified send nothing
//     (verifiedFor), so a real one needs a POST only for a URL it hasn't
//     used. Some clients give each subscription a URL of its own, so a
//     principal can need a POST per subscription: 30 a minute is above
//     its whole quota of live webhooks (mcpevents.MaxWebhooks, 20) at once.
//   - VerifyPerOwnerHost: every principal of one account's apps (the
//     account that made them; an app no account made counts alone) at one
//     host: a fifth of VerifyPerHost, so an app making up principals can't
//     use up a shared host's backstop for everyone else's apps by itself.
//   - VerifyPerHost: every principal at one host, the backstop against
//     apps that make up principals to flood someone else's host: 600 a
//     minute, 10 small POSTs a second at most.
//   - VerifyPerAddr: every principal at one address the host resolves to
//     (mcpevents.Resolve; the POST goes only to those, mcpevents.Pinned),
//     so names made up under a wildcard DNS record don't each get a
//     host's count at one server.
//
// A POST is counted under all of them at once, or none (dblimit.TakeAll):
// a verification refused at any limit, the host's, an address's or the
// account's, uses up nothing under the others, the principal's own count
// included, and concurrent ones can't race past a limit. And failures are
// counted too: a verification that was sent and failed (no echo, an
// error, no answer) counts against its account, at its host
// (VerifyFailuresPerOwnerHost) and in all (VerifyFailuresPerOwner); past
// either, the account's apps' subscribes that need a verification there
// (or anywhere) are refused without sending one. Each is counted as a
// failure as it's sent, and given back if it didn't fail, so a burst of
// concurrent ones can't all go out before any has failed. Real users'
// callbacks echo; made-up principals' don't.
//
// Variables so tests can lower them.
var (
	VerifyPerPrincipalHost     = dblimit.Limit{Name: "mcp-events-verify-principal-host", Max: 30, Window: time.Minute}
	VerifyPerOwnerHost         = dblimit.Limit{Name: "mcp-events-verify-owner-host", Max: 120, Window: time.Minute}
	VerifyPerHost              = dblimit.Limit{Name: "mcp-events-verify-host", Max: 600, Window: time.Minute}
	VerifyPerAddr              = dblimit.Limit{Name: "mcp-events-verify-addr", Max: 600, Window: time.Minute}
	VerifyFailuresPerOwnerHost = dblimit.Limit{Name: "mcp-events-verify-failures-owner-host", Max: 20, Window: time.Minute}
	VerifyFailuresPerOwner     = dblimit.Limit{Name: "mcp-events-verify-failures-owner", Max: 120, Window: time.Minute}
)

// errVerifyHostBusy is a verification refused at any limit: this
// principal, this account, or everyone sent the callback's host its quota
// of challenges this minute, so this one isn't sent. As SEP-3415 says, it
// surfaces only as challenge_failed.
var errVerifyHostBusy = &mcpevents.CallbackError{Reason: mcpevents.ReasonChallenge,
	Detail: "Wakeshift has sent this callback's host as many verification requests as it allows for now"}

// verifyCallback sends callback its verification challenge (mcpevents.Verify)
// for app's principal, within the limits above. A failure to count refuses
// it: the limits keep the POSTs off other people's hosts.
func (a *API) verifyCallback(ctx context.Context, app, principal, callback, sub string, secret []byte) error {
	if a.VerifyLimits == nil {
		return mcpevents.Verify(ctx, a.eventsClient(), callback, sub, secret, a.now())
	}
	host := callbackHost(callback)
	owner, err := appOwner(ctx, a.DB.Pool, app)
	if err != nil {
		return err
	}
	refused := func(l dblimit.Limit) error {
		slog.Warn("mcp events: a callback verification was refused at a limit", "app", app, "limit", l.Name)
		return errVerifyHostBusy
	}
	// An account past its failures is refused before anything else (a
	// lookup, a count). Then each verification counts as a failure up
	// front, with the rest (below), so concurrent ones can't all pass
	// before any has failed; one that doesn't fail gives it back.
	failures := []dblimit.Count{{Limit: VerifyFailuresPerOwner, Subject: owner}, {Limit: VerifyFailuresPerOwnerHost, Subject: owner + "\x00" + host}}
	for _, c := range failures {
		if over, _, err := a.VerifyLimits.Over(ctx, a.DB.Pool, c.Limit, c.Subject, a.now()); err != nil {
			return err
		} else if over {
			return refused(c.Limit)
		}
	}
	// Resolved once, and the POST sent only to what was counted: a name
	// that resolves to nothing public sends nothing (and isn't counted).
	addrs, err := mcpevents.Resolve(ctx, callback)
	if err != nil {
		return mcpevents.Unreachable(callback, err)
	}
	// Counted under every limit, or (refused at any) under none.
	counts := []dblimit.Count{{Limit: VerifyPerPrincipalHost, Subject: app + "\x00" + principal + "\x00" + host},
		{Limit: VerifyPerOwnerHost, Subject: owner + "\x00" + host}, {Limit: VerifyPerHost, Subject: host}}
	counts = append(counts, failures...)
	for _, ip := range addrs {
		counts = append(counts, dblimit.Count{Limit: VerifyPerAddr, Subject: ip.String()})
	}
	taken := a.now()
	if l, err := a.VerifyLimits.TakeAll(ctx, a.DB.Pool, counts, taken); err != nil {
		return err
	} else if l != nil {
		return refused(*l)
	}
	verr := mcpevents.Verify(mcpevents.Pinned(ctx, addrs), a.eventsClient(), callback, sub, secret, a.now())
	if !errors.As(verr, new(*mcpevents.CallbackError)) {
		// It didn't fail (or failed on this side, not the callback's).
		for _, c := range failures {
			if err := a.VerifyLimits.GiveBack(context.WithoutCancel(ctx), a.DB.Pool, c.Limit, c.Subject, taken); err != nil {
				slog.Warn("mcp events: giving back a verification's failure count", "app", app, "err", err)
			}
		}
	}
	return verr
}

// appOwner is who VerifyPerOwnerHost and the failure limits count app
// under: the account that made it, or, for an app no account made (one
// configured on the server), the app itself. apps is platform-level.
func appOwner(ctx context.Context, q dblimit.Querier, app string) (string, error) {
	var by string
	err := q.QueryRow(ctx, `SELECT coalesce(created_by::text, '') FROM apps WHERE id = $1`, app).Scan(&by)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if by == "" {
		return "app:" + app, nil
	}
	return "user:" + by, nil
}

// callbackHost is the host VerifyPerHost counts a callback under: its
// hostname, lowercased, without a trailing dot or the port.
func callbackHost(callback string) string {
	u, err := url.Parse(callback)
	if err != nil {
		return callback
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// verifiedFor reports whether principal has a live webhook subscription to
// app's events at callback in the transaction's workspace (SEP-3415,
// Endpoint verification: verification is cached per (principal, url)). Its
// callback answered a challenge when it was made, and that covers the
// principal's other subscriptions there, whatever their event or
// arguments, for as long as one is live; never another principal's, and
// never one whose webhook ended (a 410, a revocation) or lapsed.
func verifiedFor(ctx context.Context, tx pgx.Tx, app, principal, callback string) (bool, error) {
	if strings.TrimSpace(principal) == "" {
		return false, nil
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_subscriptions s
		JOIN mcp_event_webhooks w ON w.tenant_id = s.tenant_id AND w.id = s.webhook_id
		WHERE s.source_kind = $1 AND s.source_id = $2 AND s.principal = $3 AND s.webhook_url = $4
		AND s.destination = 'webhook' AND s.ended_at IS NULL
		AND w.app_id = $2 AND w.url = $4 AND w.expires_at > now() AND w.ending_reason IS NULL AND w.finished_at IS NULL)`,
		sourceApp, app, principal, callback).Scan(&ok)
	return ok, err
}
