package server

import (
	"net/http"
	"strings"
	"time"
)

// AdvisoriesURL is where a vulnerability in the server's code can be
// reported privately (GitHub's security advisories for the public
// repository), split so tools that rewrite the module path
// leave it alone.
const AdvisoriesURL = "https://github.com/wakeshift/" + "server/security/advisories/new"

// securityTxt is the server's /.well-known/security.txt (RFC 9116): who to
// tell about a vulnerability in this deployment (its support address,
// WAKESHIFT_SUPPORT_EMAIL) and in the server's code (AdvisoriesURL). Its
// Expires is always about a year out, so a self-hosted server never serves
// a stale one. The hosted service's public site serves its own.
func securityTxt(supportEmail string) http.HandlerFunc {
	contact := ""
	if e := strings.TrimSpace(supportEmail); e != "" && !strings.ContainsAny(e, "\r\n") {
		contact = "Contact: mailto:" + e + "\n"
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		expires := time.Now().UTC().Truncate(24*time.Hour).AddDate(1, 0, 0)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write([]byte(contact + "Contact: " + AdvisoriesURL + "\n" +
			"Expires: " + expires.Format(time.RFC3339) + "\n" +
			"Preferred-Languages: en\n"))
	}
}

// The console's host is the app, not the site: what crawlers and coding
// agents look for at a host's root is either sent to the site or refused,
// never answered with the app's HTML and a 200, which reads as a page
// that exists.

// siteFiles are the docs for coding agents the hosted site serves,
// by their lowercased path, and their path there. A request for one on the
// console's host is redirected to the site.
var siteFiles = map[string]string{
	"/llms.txt":      "/llms.txt",
	"/llms-full.txt": "/llms-full.txt",
	"/agents.md":     "/AGENTS.md",
}

// robots is the console's robots.txt, which asks crawlers to stay out: its
// pages are behind sign-in, or consent and confirm pages nobody should find
// in a search. The public pages are the site's, which has its own
// robots.txt and sitemap. Only the root, the sign-in page every page of the
// site links, may be fetched, so search engines read its noindex (spa)
// rather than list the address without it; "$" ends the path for the
// crawlers that read it (Google's, Bing's), and the others stay out.
func robots(siteURL string) http.HandlerFunc {
	body := "# The Wakeshift console.\n"
	if siteURL != "" {
		body = "# The Wakeshift console. Its public pages, and their sitemap, are at " + siteURL + ".\n"
	}
	body += "# Its pages, sign-in included, are noindex.\nUser-agent: *\nAllow: /$\nDisallow: /\n"
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write([]byte(body))
	}
}

// discovery wraps the app: the site's files redirect to siteURL, or are
// 404s without one. (Other paths that look like files are the app's 404s:
// spa.)
func discovery(siteURL string, app http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if to, ok := siteFiles[strings.ToLower(r.URL.Path)]; ok && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if siteURL == "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "public, max-age=3600")
			http.Redirect(w, r, siteURL+to, http.StatusMovedPermanently)
			return
		}
		app.ServeHTTP(w, r)
	})
}
