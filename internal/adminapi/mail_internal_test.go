package adminapi

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/apps/email/emailtest"
	"github.com/wakeshift/server/internal/accounts"
)

// The waitlist's invitation, as sent: go test -run TestInvitationMail
// -update rewrites testdata/, which shows it in a browser.
func TestInvitationMail(t *testing.T) {
	const console = "https://app.wakeshift.ai"
	m := invitationMail(accounts.WaitlistInvite{Email: "ada@example.com", Name: " Ada \n Lovelace ", Provider: "google"}, console)
	if m.To != "ada@example.com" || m.Subject != "Your Wakeshift account is ready" {
		t.Fatalf("to %q, subject %q", m.To, m.Subject)
	}
	emailtest.Golden(t, "waitlist-invitation", m.HTML, m.Text, console+"/")
	if !strings.HasPrefix(m.Text, "YOU'RE OFF THE WAITLIST\nYour Wakeshift account is ready\n===============================\n\nHi Ada Lovelace,\n\n") || !strings.Contains(m.Text, "the Google account you joined the waitlist with") ||
		!strings.Contains(m.Text, "You sign in through Google. Wakeshift never asks for your password.") {
		t.Errorf("text:\n%s", m.Text)
	}

	// No name, and a provider it doesn't know: no greeting, and no name
	// for the provider.
	m = invitationMail(accounts.WaitlistInvite{Email: "bob@example.com", Provider: "other"}, console+"/")
	if strings.Contains(m.Text, "Hi ") || strings.Contains(m.HTML, "Hi ,") || !strings.HasPrefix(m.Text, "YOU'RE OFF THE WAITLIST\nYour Wakeshift account is ready\n===============================\n\nThanks for waiting. Sign in with the account you joined the waitlist with") ||
		!strings.Contains(m.Text, "You sign in through the account you joined with.") {
		t.Errorf("without a name or a known provider:\n%s", m.Text)
	}
}
