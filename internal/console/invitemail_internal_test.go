package console

import (
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/apps/email/emailtest"
	"github.com/wakeshift/server/internal/orgs"
)

// An organization invite's email, as sent: go test -run TestInviteMail
// -update rewrites testdata/, which shows it in a browser.
func TestInviteMail(t *testing.T) {
	const link = "https://app.wakeshift.ai/join#token"
	expires := time.Date(2026, 11, 4, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		role orgs.Role
		can  string
	}{
		{orgs.Developer, "As a developer, you can create apps in Acme Robotics and manage their keys."},
		{orgs.Admin, "As an admin, you can manage the apps in Acme Robotics and invite developers."},
		{orgs.Owner, "As an owner, you can manage everything in Acme Robotics, including its admins."},
	} {
		s := orgs.Sent{Invite: orgs.Invite{Email: "cy@example.com", Role: tc.role, ExpiresAt: expires}, Token: "token",
			OrgName: "Acme\n Robotics", From: "ada@example.com"}
		m := inviteMail(s, link)
		if m.To != "cy@example.com" || m.Subject != inviteSubject {
			t.Fatalf("to %q, subject %q", m.To, m.Subject)
		}
		if !strings.Contains(m.Text, "ada@example.com invited you to join Acme Robotics on Wakeshift") || !strings.Contains(m.Text, tc.can) ||
			!strings.Contains(m.Text, "only for cy@example.com, until 4 November 2026") ||
			!strings.Contains(m.Text, "If you weren't expecting this invite, ignore this email.") {
			t.Errorf("%s:\n%s", tc.role, m.Text)
		}
		if tc.role == orgs.Developer {
			emailtest.Golden(t, "org-invite", m.HTML, m.Text, link)
		}
	}

	// A name with markup stays text.
	m := inviteMail(orgs.Sent{Invite: orgs.Invite{Email: "cy@example.com", Role: orgs.Developer, ExpiresAt: expires},
		OrgName: "<b>Acme</b>", From: "ada@example.com"}, link)
	if strings.Contains(m.HTML, "<b>Acme</b>") || !strings.Contains(m.HTML, "&lt;b&gt;Acme&lt;/b&gt;") {
		t.Errorf("HTML:\n%s", m.HTML)
	}
}

// An organization's name is its owner's text: one that reads as a link, a
// domain or an address never reaches the email, nor does any name reach
// the subject or preheader.
func TestInviteMailPhishingNames(t *testing.T) {
	const link = "https://app.wakeshift.ai/join#token"
	expires := time.Date(2026, 11, 4, 18, 0, 0, 0, time.UTC)
	for _, name := range []string{
		"Acme: verify your account at https://evil.example/login",
		"Acme Security — wakeshift-support.com",
		"evil.example",
		"Acme (write to help@evil.example)",
		"www.evil",
		"Acme //evil",
		"Acme mailto:x",
		"evil\u3002example",
		"evil\uFF0Eexample",
		"Acme\u202Eelpmaxe.live",
		"Acme\u200B Robotics",
	} {
		m := inviteMail(orgs.Sent{Invite: orgs.Invite{Email: "cy@example.com", Role: orgs.Admin, ExpiresAt: expires},
			OrgName: name, From: "ada@example.com"}, link)
		for _, part := range []string{"evil", "wakeshift-support", "help@", "www.", "mailto", "\u202E", "\u200B"} {
			if strings.Contains(m.Text, part) || strings.Contains(m.Subject, part) || (part != "mailto" && strings.Contains(m.HTML, part)) {
				t.Errorf("%q: the email carries %q:\n%s", name, part, m.Text)
			}
		}
		if !strings.Contains(m.Text, "ada@example.com invited you to join an organization on Wakeshift as an admin.") ||
			!strings.Contains(m.Text, "manage the apps in the organization") {
			t.Errorf("%q:\n%s", name, m.Text)
		}
	}
	// Ordinary names stay, punctuation and all; the subject and preheader
	// never carry one.
	for _, name := range []string{"Acme Robotics", "Acme: R&D", "O'Brien & Sons (EU)", "Zürich Labs", "株式会社アクメ", "Acme 2.0"} {
		m := inviteMail(orgs.Sent{Invite: orgs.Invite{Email: "cy@example.com", Role: orgs.Admin, ExpiresAt: expires},
			OrgName: name, From: "ada@example.com"}, link)
		if !strings.Contains(m.Text, "join "+name+" on Wakeshift") {
			t.Errorf("%q was dropped:\n%s", name, m.Text)
		}
		if m.Subject != inviteSubject || strings.Contains(m.HTML, "Join "+name+" as") {
			t.Errorf("%q: subject %q", name, m.Subject)
		}
	}
}
