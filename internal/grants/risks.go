package grants

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
)

// Risk is one thing the consent screen says, written by Wakeshift from
// facts in the workspace, not by the app.
type Risk struct {
	Key    string `json:"key"`   // what kind: peers, strangers, act, expiry
	Level  string `json:"level"` // "caution" or "info"
	Title  string `json:"title"`
	Detail string `json:"detail"`
	// Summary is the risk in one sentence, for the consent screen's single
	// warning (see Warning).
	Summary string `json:"summary,omitempty"`
}

// Warning is the one warning a consent screen shows: the most serious risk
// in a sentence or two. The rest are under Details.
type Warning struct {
	Level string `json:"level"` // "caution" or "info"
	Text  string `json:"text"`
}

// AgentWarning picks the warning for an agent consent: strangers reaching
// the agent, else a reminder that messages can lead it to act.
func AgentWarning(app apps.App, agent string, risks []Risk) Warning {
	const act = " It can run commands on your computer, so only allow apps you trust."
	for _, r := range risks {
		if r.Key == "strangers" {
			return Warning{Level: "caution", Text: r.Summary + act}
		}
	}
	return Warning{Level: "info", Text: fmt.Sprintf("Messages from %s can lead %s to act, and it can run commands on your computer.", app.Name, agent)}
}

// Pacing describes wake-up limits: "10 a second, 450 per 5 minutes and
// 3,600 an hour".
func Pacing(limits []apps.WakeLimit) string {
	var parts []string
	for _, l := range limits {
		parts = append(parts, fmt.Sprintf("%s %s", humanize(l.Max), per(l.Seconds)))
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// DryRun says what a grant would have done recently.
type DryRun struct {
	Days   int `json:"days"`
	Events int `json:"events"` // events in that time the agent would have been woken by
}

const dryRunDays = 7

// AgentConsent describes what letting app act for agent means: its risks
// and a dry run. It reads the workspace (the transaction's tenant).
func AgentConsent(ctx context.Context, tx pgx.Tx, app apps.App, agent string) ([]Risk, DryRun, error) {
	scope := app.Manifest.Agent
	if scope == nil {
		return nil, DryRun{}, fmt.Errorf("grants: %s doesn't act for agents", app.Name)
	}
	var risks []Risk
	wake := patterns(scope.Wake)

	// Other agents on the same app can post where this one listens.
	rows, err := tx.Query(ctx, `SELECT agent_id FROM grants WHERE app_id = $1 AND kind = 'agent' AND agent_id <> $2
		AND revoked_at IS NULL AND expires_at > now() ORDER BY agent_id`, app.ID, agent)
	if err != nil {
		return nil, DryRun{}, err
	}
	peers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, DryRun{}, err
	}
	// Other people's agents reach this one through the app: posting where
	// it listens (Post), or through the app's own membership of channels
	// and games (OthersCanWake).
	others := app.Manifest.OthersCanWake
	if len(peers) > 0 && (len(scope.Post) > 0 || others) {
		risks = append(risks, Risk{Key: "peers", Level: "info", Title: "Other agents can wake " + agent,
			Detail:  fmt.Sprintf("%s also use %s, and can post where %s listens.", strings.Join(peers, ", "), app.Name, agent),
			Summary: fmt.Sprintf("%s can post where %s listens.", strings.Join(peers, ", "), agent)})
	}

	if others {
		risks = append(risks, Risk{Key: "strangers", Level: "caution", Title: "Agents of people you don't know can wake " + agent,
			Summary: fmt.Sprintf("People you don't know can send %s messages.", agent),
			Detail:  fmt.Sprintf("%s passes on to %s what other people and their agents send there, and they can mention it. Wakeshift marks what %s passes on as from outside you. To stop it, revoke %s on the Apps page.", app.Name, agent, app.Name, app.Name)})
	}
	risks = append(risks,
		Risk{Key: "act", Level: "caution", Title: "Messages can lead " + agent + " to act",
			Detail: "Agents such as Claude Code and Codex can run commands on your computer. A message that wakes " + agent + " is input it may act on; Wakeshift marks it as coming from outside you, and the agent decides what to do."},
		Risk{Key: "expiry", Level: "info", Title: fmt.Sprintf("Until %s", time.Now().AddDate(0, 0, scope.TTLDays).Format("2 January 2006")),
			Detail: fmt.Sprintf("%d days, unless you revoke it sooner from the Apps page.", scope.TTLDays) + wakeLimits(scope.Limits())},
	)

	n, err := recentEvents(ctx, tx, wake)
	return risks, DryRun{Days: dryRunDays, Events: n}, err
}

func patterns(list []apps.ScopeEvent) []string {
	out := make([]string, len(list))
	for i, e := range list {
		out[i] = e.Pattern
	}
	return out
}

// recentEvents counts the workspace's events in the dry-run window that
// match any of patterns.
func recentEvents(ctx context.Context, tx pgx.Tx, pats []string) (int, error) {
	if len(pats) == 0 {
		return 0, nil
	}
	likes := make([]string, len(pats))
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	for i, p := range pats {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			likes[i] = esc.Replace(prefix) + "%"
		} else {
			likes[i] = esc.Replace(p)
		}
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE received_at > now() - make_interval(days => $2) AND type LIKE ANY($1)`,
		likes, dryRunDays).Scan(&n)
	return n, err
}

// wakeLimits describes the pace: "At most 10 wake-ups a second, 450 per 5
// minutes and 3,600 an hour; more wait their turn."
func wakeLimits(limits []apps.WakeLimit) string {
	var parts []string
	for _, l := range limits {
		parts = append(parts, fmt.Sprintf("%s %s", humanize(l.Max), per(l.Seconds)))
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return " At most " + parts[0] + " wake-ups; more wait their turn."
	}
	return " Wake-ups are paced: at most " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1] + "; more wait their turn."
}

func per(seconds int) string {
	switch {
	case seconds == 1:
		return "a second"
	case seconds == 60:
		return "a minute"
	case seconds == 3600:
		return "an hour"
	case seconds == 86400:
		return "a day"
	case seconds%3600 == 0:
		return fmt.Sprintf("per %d hours", seconds/3600)
	case seconds%60 == 0:
		return fmt.Sprintf("per %d minutes", seconds/60)
	}
	return fmt.Sprintf("per %d seconds", seconds)
}

func humanize(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
