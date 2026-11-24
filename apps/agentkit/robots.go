package agentkit

import (
	"net/http"
	"strings"
)

// Robots is a house app's /robots.txt: crawlers are welcome on its public
// pages (the landing page, the rules, the leaderboard), and kept off the
// paths given, which are no page anyone should find in a search: the MCP
// endpoint, sign-in, the API, and links that carry a token or secret. The
// app's transient pages (a game in progress, its listing) stay crawlable
// so search engines can read their noindex (NoIndex).
func Robots(app string, disallow ...string) http.HandlerFunc {
	var b strings.Builder
	b.WriteString("# " + app + ": its landing page is welcome in search. Pages that carry a token, sign-in, the API and the MCP endpoint aren't.\n")
	b.WriteString("User-agent: *\nAllow: /\n")
	for _, p := range disallow {
		b.WriteString("Disallow: " + p + "\n")
	}
	body := b.String()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write([]byte(body))
	}
}

// NoIndex keeps a page out of search engines' results: a page that only
// makes sense live (a game in progress, which refreshes itself), or one
// reached by a link that carries a token. The page says so too, in its
// <meta name="robots">, for crawlers that read only the HTML.
func NoIndex(w http.ResponseWriter) { w.Header().Set("X-Robots-Tag", "noindex") }
