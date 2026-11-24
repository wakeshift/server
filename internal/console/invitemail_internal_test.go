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
			OrgName: "Acme\n Robotics", From: "Ada Lovelace"}
		m := inviteMail(s, link)
		if m.To != "cy@example.com" || m.Subject != "Ada Lovelace invited you to join Acme Robotics on Wakeshift" {
			t.Fatalf("to %q, subject %q", m.To, m.Subject)
		}
		if !strings.Contains(m.Text, tc.can) || !strings.Contains(m.Text, "only for cy@example.com, until 4 November 2026") ||
			!strings.Contains(m.Text, "If you weren't expecting this invite, ignore this email.") {
			t.Errorf("%s:\n%s", tc.role, m.Text)
		}
		if tc.role == orgs.Developer {
			emailtest.Golden(t, "org-invite", m.HTML, m.Text, link)
		}
	}

	// A name with markup stays text.
	m := inviteMail(orgs.Sent{Invite: orgs.Invite{Email: "cy@example.com", Role: orgs.Developer, ExpiresAt: expires},
		OrgName: "<b>Acme</b>", From: `"Ada" <ada@example.com>`}, link)
	if strings.Contains(m.HTML, "<b>Acme</b>") || strings.Contains(m.HTML, "<ada@example.com>") || !strings.Contains(m.HTML, "&lt;b&gt;Acme&lt;/b&gt;") {
		t.Errorf("HTML:\n%s", m.HTML)
	}
}
