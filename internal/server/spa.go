package server

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// spa serves the React app: files that exist are served as they are (hashed
// assets are cached for a year); any other path gets index.html, so client-side
// routes work on reload. A path that looks like a file (its last segment has
// an extension: /index.md, /sitemap.xml, /wp-login.php) and isn't one is a
// 404, not the app's HTML with a 200, which crawlers and agents would read
// as a page that exists. None of the app's routes has a dot in its last
// segment, nor do the IDs in them (apps', organizations'), so its deep links
// still get the app.
func spa(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		// Paths the app never routes to: a missing asset (an old hash after
		// a deploy) or an unknown server path is a 404, not the app's HTML.
		// admin/: where the core once had admin pages (now the admin
		// service's): nothing there is the app's.
		for _, prefix := range []string{"assets/", "auth/", "v1/", "admin/"} {
			if strings.HasPrefix(name, prefix) {
				if info, err := fs.Stat(dist, name); err != nil || info.IsDir() || prefix != "assets/" {
					http.NotFound(w, r)
					return
				}
			}
		}
		if name != "" {
			if info, err := fs.Stat(dist, name); err == nil && !info.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		index, err := fs.ReadFile(dist, "index.html")
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "The web app isn't built. Run `make web`, or use the Vite dev server (`make dev-web`).", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		// The console stays out of search, sign-in included (robots).
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		w.Write(index)
	})
}
