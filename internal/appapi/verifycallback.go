package appapi

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dblimit"
	"github.com/wakeshift/server/internal/mcpevents"
)

// Verification POSTs are rate-limited per destination host (SEP-3415,
// Webhook security, Endpoint verification: they SHOULD be), counted across
// the core's instances (rate_counts), in two layers:
//
//   - VerifyPerPrincipalHost: one principal (an app's user) at one host.
//     A principal sending failing subscribes at a shared callback host
//     (ChatGPT's) uses up its own count, not the host's for everyone. A
//     principal's later subscriptions to a URL it verified send nothing
//     (verifiedFor), so a real one needs a POST only for a URL it hasn't
//     used. Some clients give each subscription a URL of its own, so a
//     principal can need a POST per subscription: 30 a minute is above
//     its whole quota of live webhooks (mcpevents.MaxWebhooks, 20) at once.
//   - VerifyPerHost: every principal at one host, the backstop against an
//     app that makes up principals to flood someone else's host: 600 a
//     minute, 10 small POSTs a second at most. Only a POST its principal's
//     limit allowed is counted here, so one principal takes at most
//     VerifyPerPrincipalHost.Max of it; it takes 20 principals at the
//     limit at once to hold up a host.
//
// Variables so tests can lower them.
var (
	VerifyPerPrincipalHost = dblimit.Limit{Name: "mcp-events-verify-principal-host", Max: 30, Window: time.Minute}
	VerifyPerHost          = dblimit.Limit{Name: "mcp-events-verify-host", Max: 600, Window: time.Minute}
)

// errVerifyHostBusy is a verification refused for either limit: this
// principal, or everyone, sent the callback's host its quota of challenges
// this minute, so this one isn't sent. As SEP-3415 says, it surfaces only
// as challenge_failed.
var errVerifyHostBusy = &mcpevents.CallbackError{Reason: mcpevents.ReasonChallenge,
	Detail: "Wakeshift has sent this callback's host as many verification requests as it allows for now"}

// verifyCallback sends callback its verification challenge (mcpevents.Verify)
// for app's principal, unless the principal is past VerifyPerPrincipalHost
// at its host or everyone is past VerifyPerHost there. A failure to count
// refuses it: the limits keep the POSTs off other people's hosts.
func (a *API) verifyCallback(ctx context.Context, app, principal, callback, sub string, secret []byte) error {
	if a.VerifyLimits != nil {
		host := callbackHost(callback)
		refused := func(l dblimit.Limit) error {
			slog.Warn("mcp events: a callback verification was refused at a limit", "app", app, "limit", l.Name)
			return errVerifyHostBusy
		}
		// The host's backstop is read first, without counting, so a
		// refusal there doesn't use up the principal's own count.
		if over, _, err := a.VerifyLimits.Over(ctx, a.DB.Pool, VerifyPerHost, host, a.now()); err != nil {
			return err
		} else if over {
			return refused(VerifyPerHost)
		}
		for _, l := range []struct {
			limit   dblimit.Limit
			subject string
		}{{VerifyPerPrincipalHost, app + "\x00" + principal + "\x00" + host}, {VerifyPerHost, host}} {
			ok, _, err := a.VerifyLimits.Take(ctx, a.DB.Pool, l.limit, l.subject, a.now())
			if err != nil {
				return err
			}
			if !ok {
				return refused(l.limit)
			}
		}
	}
	return mcpevents.Verify(ctx, a.eventsClient(), callback, sub, secret, a.now())
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
