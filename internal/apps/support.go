package apps

import "fmt"

// DefaultSupportEmail is where app owners write to Wakeshift's hosted
// service (WAKESHIFT_SUPPORT_EMAIL's default).
const DefaultSupportEmail = "support@wakeshift.ai"

// Support is the address a refusal tells app owners to write to: the
// server's configured one (WAKESHIFT_SUPPORT_EMAIL, carried by each service
// that answers requests), else DefaultSupportEmail. There's no package-wide
// setting, so two servers in one process each name their own.
func Support(configured string) string {
	if configured == "" {
		return DefaultSupportEmail
	}
	return configured
}

// SuspendedMessage is the text a suspended app's refusal (ErrSuspended,
// 403 suspended) answers with, naming where its owners can write.
func SuspendedMessage(support string) string {
	return "this app is suspended; its owners can write to " + Support(support)
}

// Message is the text a WorkspaceLimitError's refusal (403
// workspace_limit) answers with, naming where to ask for more.
func (e WorkspaceLimitError) Message(support string) string {
	return fmt.Sprintf("this app has reached its limit of %d workspaces; forget users it no longer needs, or write to %s to raise it", e.Cap, Support(support))
}
