package console

// Organization invites and joining. Owners and admins invite an address
// with a role; Wakeshift emails https://<console>/join#<token> (the token
// in the fragment, so it never reaches a server log or a Referer) and
// shows the link once, to copy into a chat, or instead when email isn't
// set up. Tokens and links are never logged, nor the addresses they're
// sent to.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/email"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mail"
	"github.com/wakeshift/server/internal/orgs"
)

func (a *API) registerInvites(route func(string, http.HandlerFunc)) {
	route("GET /api/v1/orgs/{org}/invites", a.listInvites)
	route("POST /api/v1/orgs/{org}/invites", a.createInvite)
	route("POST /api/v1/orgs/{org}/invites/{id}/revoke", a.revokeInvite)
	route("POST /api/v1/orgs/{org}/invites/{id}/resend", a.resendInvite)
	route("POST /api/v1/join", a.acceptInvite)
}

// registerJoinPreview adds the one route that needs no session: what an
// invite is for, to whoever holds its token.
func (a *API) registerJoinPreview(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/join/preview", a.previewInvite)
}

func (a *API) mailer() mail.Sender {
	if a.Mail == nil {
		return mail.None{}
	}
	return a.Mail
}

// joinURL is an invite's link: the token in the fragment.
func (a *API) joinURL(token string) string {
	return strings.TrimSuffix(a.Auth.PublicURL, "/") + "/join#" + token
}

func (a *API) listInvites(w http.ResponseWriter, r *http.Request) {
	var list []orgs.Invite
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = orgs.Invites(ctx, tx, me(r), r.PathValue("org"))
		return err
	})
	respondInvites(w, map[string]any{"invites": list, "email": a.mailer().Ready(r.Context())}, err)
}

func (a *API) createInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string    `json:"email"`
		Role  orgs.Role `json:"role"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	var s orgs.Sent
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		s, err = orgs.CreateInvite(ctx, tx, me(r), r.PathValue("org"), in.Email, in.Role)
		return err
	})
	if err != nil {
		respondInvites(w, nil, err)
		return
	}
	a.respondSent(w, r, s)
}

func (a *API) resendInvite(w http.ResponseWriter, r *http.Request) {
	var s orgs.Sent
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		s, err = orgs.ResendInvite(ctx, tx, me(r), r.PathValue("org"), r.PathValue("id"))
		return err
	})
	if err != nil {
		respondInvites(w, nil, err)
		return
	}
	a.respondSent(w, r, s)
}

// respondSent emails a committed invite, then answers with its link (shown
// once) and whether the email went.
func (a *API) respondSent(w http.ResponseWriter, r *http.Request, s orgs.Sent) {
	link := a.joinURL(s.Token)
	emailed := false
	if m := a.mailer(); m.Ready(r.Context()) {
		if err := m.Send(r.Context(), inviteMail(s, link)); err != nil {
			// The error never holds the message or its recipient.
			slog.Warn("console: emailing an invite failed", "invite", s.Invite.ID, "err", err)
		} else {
			emailed = true
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"invite": s.Invite, "url": link, "emailed": emailed})
}

// oneLine keeps a name on one line, for a subject.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// roleCan says what a role lets an invitee do in org, for the invite's
// email (orgs/roles.go has the full list).
func roleCan(r orgs.Role, org string) string {
	switch r {
	case orgs.Developer:
		return "create apps in " + org + " and manage their keys"
	case orgs.Admin:
		return "manage the apps in " + org + " and invite developers"
	case orgs.Owner:
		return "manage everything in " + org + ", including its admins"
	}
	return ""
}

// inviteSubject is every invite email's subject: fixed, so no member's
// text reaches an inbox list (mailsafe.go).
const inviteSubject = "You're invited to an organization on Wakeshift"

// inviteMail is an invite's email. It names the inviter by their email
// address, and the organization only if its name can't read as a link
// (mailName); the subject and preheader carry neither.
func inviteMail(s orgs.Sent, link string) mail.Message {
	from := oneLine(s.From)
	org, heading, button := "an organization", "Join an organization on Wakeshift", "Join the organization"
	can := "the organization"
	if name := mailName(s.OrgName); name != "" {
		org, heading, button, can = name, "Join "+name+" on Wakeshift", "Join "+name, name
	}
	role := string(s.Invite.Role)
	article := "a"
	if role == "owner" || role == "admin" {
		article = "an"
	}
	expires := s.Invite.ExpiresAt.UTC().Format("2 January 2006")
	invited := fmt.Sprintf("%s invited you to join %s on Wakeshift as %s %s.", from, org, article, role)
	if can := roleCan(s.Invite.Role, can); can != "" {
		invited += fmt.Sprintf(" As %s %s, you can %s.", article, role, can)
	}
	r := email.Render(email.Wakeshift, email.Email{
		Subject:   inviteSubject,
		Preheader: fmt.Sprintf("Join as %s %s. The link works until %s.", article, role, expires),
		Kicker:    "Invitation",
		Heading:   heading,
		Body:      []string{invited},
		Button:    email.Button{Label: button, URL: link},
		Notes: []string{
			fmt.Sprintf("The link works once, only for %s, until %s. You accept by signing in with Google or GitHub. "+
				"Wakeshift never asks for your password.", s.Invite.Email, expires),
			"If you weren't expecting this invite, ignore this email. Nothing changes unless you accept.",
		},
		Reason: from + " sent this invite from the Wakeshift console.",
	})
	return mail.Message{To: s.Invite.Email, Subject: inviteSubject, Text: r.Text, HTML: r.HTML}
}

func (a *API) revokeInvite(w http.ResponseWriter, r *http.Request) {
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) error {
		return orgs.RevokeInvite(ctx, tx, me(r), r.PathValue("org"), r.PathValue("id"))
	})
	respondInvites(w, map[string]bool{"revoked": true}, err)
}

// maxToken is longer than any token Wakeshift makes (43 characters).
const maxToken = 100

// previewInvite says what an invite is for (the organization, who sent it,
// the role), to whoever holds its token, signed in or not.
func (a *API) previewInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	var p orgs.Preview
	err := orgs.ErrInviteInvalid
	if in.Token != "" && len(in.Token) <= maxToken {
		err = a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
			p, err = orgs.PreviewInvite(ctx, tx, in.Token)
			return err
		})
	}
	respondInvites(w, p, err)
}

// acceptInvite joins the organization, as the signed-in user, whose
// verified email must be the invite's.
func (a *API) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	var org string
	err := orgs.ErrInviteInvalid
	if in.Token != "" && len(in.Token) <= maxToken {
		err = a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
			org, err = orgs.Accept(ctx, tx, me(r), in.Token)
			return err
		})
	}
	respondInvites(w, map[string]string{"org_id": org}, err)
}

func respondInvites(w http.ResponseWriter, v any, err error) {
	var wrong *orgs.WrongAccountError
	switch {
	case errors.As(err, &wrong):
		httpx.WriteJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"code": "wrong_account",
			"message": "This invite is for " + wrong.Masked + ". Sign in with that account."}, "email": wrong.Masked})
	case errors.Is(err, orgs.ErrInviteInvalid):
		httpx.WriteError(w, http.StatusNotFound, "invalid_invite", "This invite isn't valid any more. Ask for a new one.")
	case errors.Is(err, orgs.ErrInviterCant):
		httpx.WriteError(w, http.StatusConflict, "inviter_cant", "Whoever invited you can't any more. Ask for a new invite.")
	case errors.Is(err, orgs.ErrAlreadyMember):
		httpx.WriteError(w, http.StatusConflict, "already_member", "That's a member already.")
	case errors.Is(err, orgs.ErrInvitePending):
		httpx.WriteError(w, http.StatusConflict, "invite_pending", "That address has an invite waiting. Resend it instead.")
	case errors.Is(err, orgs.ErrTooManyInvites):
		httpx.WriteError(w, http.StatusBadRequest, "too_many", fmt.Sprintf("At most %d invites can wait at once. Revoke one first.", orgs.MaxPendingInvites))
	case errors.Is(err, orgs.ErrTooManyMembers):
		httpx.WriteError(w, http.StatusConflict, "too_many_members", fmt.Sprintf("An organization has at most %d members.", orgs.MaxMembers))
	case errors.Is(err, orgs.ErrRateLimited):
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "Too many invite emails for now. Try again later.")
	default:
		respondOrgs(w, v, err)
	}
}
