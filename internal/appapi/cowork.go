package appapi

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/streams"
)

// coworkResult: a Claude Cowork task is woken by a Monitor it starts on
// its session's stream link, as Claude Code is with nothing installed
// (harnesses.MonitorLoop), and by nothing else: plugin hooks don't run in a
// task, and nothing is installed, so no result carries a beacon. Its
// client name is claude.ai chat's too, so every note says what to do with
// a Monitor tool and what without one (harnesses.CoworkNote).
// Subscribing gives the command (the full note once, then the short one),
// or says the Monitor already runs; other results add nothing (what
// arrives waits for the session until a Monitor connects).
func (a *API) coworkResult(ctx context.Context, tx pgx.Tx, cv *conversation, why resultFor) (toolResult, error) {
	s := cv.session
	var out toolResult
	cv.noBeacon = "nothing_to_install"
	if why == forAnnotate {
		return out, nil
	}
	running := false
	if s.Connected {
		var err error
		if running, err = streams.LiveOnWorkingLink(ctx, tx, s.Agent, s.ID, false); err != nil {
			return out, err
		}
	}
	if running {
		out.Text = join(harnesses.MonitorRunningNote(), passSessionIfNew(cv))
		return out, nil
	}
	link, err := a.sessionLink(ctx, tx, cv)
	if err != nil {
		return out, err
	}
	url := streams.WaitURL(link)
	asked, err := askedBefore(ctx, tx, cv)
	if err != nil {
		return out, err
	}
	if asked {
		out.Text = join(harnesses.CoworkAgainNote(appName(cv), url), passSessionIfNew(cv))
		return out, nil
	}
	out.Text = join(harnesses.CoworkNote(appName(cv), url), passSession(cv))
	return out, markAsked(ctx, tx, cv)
}
