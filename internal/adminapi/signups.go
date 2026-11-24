package adminapi

// Sign-ups: the cap on accounts, and inviting people off the waitlist or
// removing them. Each change and its admin_log entries (with the reason)
// are one transaction. Invitation emails go after it commits, through the
// core's mail sender, and their recipients are never logged.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/email"
	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mail"
)

// The admin_log actions for sign-ups.
const (
	ActionSignupCapSet    = "signups.cap_set"
	ActionWaitlistInvited = "waitlist.invited"
	ActionWaitlistRemoved = "waitlist.removed"
)

// MaxBatch is the most waitlist entries one request invites or removes.
const MaxBatch = 100

func (a *API) registerSignups(route func(string, func(http.ResponseWriter, *http.Request, string))) {
	route("PUT /internal/admin/v1/signups/cap", a.signupCap)
	route("POST /internal/admin/v1/waitlist/invite", a.inviteWaitlist)
	route("POST /internal/admin/v1/waitlist/remove", a.removeWaitlist)
}

// uuidRE is a waitlist entry's ID.
var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (a *API) signupCap(w http.ResponseWriter, r *http.Request, admin string) {
	var in struct {
		Cap    *int   `json:"cap"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &in, &in.Reason) {
		return
	}
	if in.Cap == nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "cap is a whole number of accounts")
		return
	}
	var old int
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		if old, err = accounts.SetSignupCap(ctx, tx, *in.Cap); err != nil {
			return err
		}
		return logAdminTarget(ctx, tx, admin, ActionSignupCapSet, "signups", "cap", in.Reason, map[string]int{"cap": *in.Cap, "old": old})
	})
	if errors.Is(err, accounts.ErrBadCap) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "the cap is a whole number from 0 to 10,000,000")
		return
	}
	respond(w, map[string]int{"cap": *in.Cap, "old": old}, err)
}

type batch struct {
	IDs    []string `json:"ids"`
	Reason string   `json:"reason"`
}

// validBatch checks a batch's IDs: 1 to MaxBatch waitlist entry IDs.
func validBatch(w http.ResponseWriter, ids []string) bool {
	if len(ids) == 0 || len(ids) > MaxBatch {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("choose 1 to %d people", MaxBatch))
		return false
	}
	for _, id := range ids {
		if !uuidRE.MatchString(id) {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "ids are waitlist entries' IDs")
			return false
		}
	}
	return true
}

// inviteWaitlist invites entries: each one's next sign-in makes their
// account. Then it emails each, recording which emails went. Inviting an
// entry already invited sends its email again; one that has its account
// is skipped.
func (a *API) inviteWaitlist(w http.ResponseWriter, r *http.Request, admin string) {
	var in batch
	if !decode(w, r, &in, &in.Reason) || !validBatch(w, in.IDs) {
		return
	}
	var invited []accounts.WaitlistInvite
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		if invited, err = accounts.InviteFromWaitlist(ctx, tx, in.IDs, admin); err != nil {
			return err
		}
		for _, e := range invited {
			if err := logAdminTarget(ctx, tx, admin, ActionWaitlistInvited, "waitlist", e.ID, in.Reason, map[string]bool{"again": e.Again}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	emailed := 0
	sender := a.mailer()
	ready := sender.Ready(r.Context())
	for _, e := range invited {
		if !ready {
			break
		}
		if err := sender.Send(r.Context(), invitationMail(e, a.ConsoleURL)); err != nil {
			// The error never holds the message or its recipient.
			slog.Warn("adminapi: emailing a waitlist invitation failed", "entry", e.ID, "err", err)
			continue
		}
		emailed++
		if err := accounts.MarkEmailed(r.Context(), a.DB.Pool, e.ID); err != nil {
			slog.Warn("adminapi: recording a waitlist invitation's email failed", "entry", e.ID, "err", err)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"invited": len(invited), "emailed": emailed, "skipped": len(in.IDs) - len(invited), "mail_ready": ready})
}

func (a *API) removeWaitlist(w http.ResponseWriter, r *http.Request, admin string) {
	var in batch
	if !decode(w, r, &in, &in.Reason) || !validBatch(w, in.IDs) {
		return
	}
	var removed []string
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		if removed, err = accounts.RemoveFromWaitlist(ctx, tx, in.IDs); err != nil {
			return err
		}
		for _, id := range removed {
			if err := logAdminTarget(ctx, tx, admin, ActionWaitlistRemoved, "waitlist", id, in.Reason, nil); err != nil {
				return err
			}
		}
		return nil
	})
	respond(w, map[string]int{"removed": len(removed)}, err)
}

func (a *API) mailer() mail.Sender {
	if a.Mail == nil {
		return mail.None{}
	}
	return a.Mail
}

var providerNames = map[string]string{"google": "Google", "github": "GitHub"}

// invitationMail is the email for an entry invited off the waitlist.
func invitationMail(e accounts.WaitlistInvite, consoleURL string) mail.Message {
	link := strings.TrimSuffix(consoleURL, "/") + "/"
	how, through := "the account you joined the waitlist with", "the account you joined with"
	if p := providerNames[e.Provider]; p != "" {
		how, through = "the "+p+" account you joined the waitlist with", p
	}
	greeting := ""
	if name := strings.Join(strings.Fields(e.Name), " "); name != "" {
		greeting = "Hi " + name + ","
	}
	r := email.Render(email.Wakeshift, email.Email{
		Subject:   "Your Wakeshift account is ready",
		Preheader: "Sign in and build your first app that wakes agents.",
		Kicker:    "You're off the waitlist",
		Heading:   "Your Wakeshift account is ready",
		Greeting:  greeting,
		Body: []string{
			"Thanks for waiting. Sign in with " + how + " and you're in.",
			"Start with the quickstart at wakeshift.ai/docs/quickstart. In about five minutes, your MCP server offers an event, " +
				"an agent subscribes to it, and the agent wakes up when the event fires.",
		},
		Button: email.Button{Label: "Sign in to Wakeshift", URL: link},
		Notes:  []string{"You sign in through " + through + ". Wakeshift never asks for your password."},
		Reason: "You're getting this email because you joined the Wakeshift waitlist.",
	})
	return mail.Message{To: e.Email, Subject: "Your Wakeshift account is ready", Text: r.Text, HTML: r.HTML}
}
