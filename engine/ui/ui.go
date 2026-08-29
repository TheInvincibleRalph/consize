// Package ui embeds the read-only dashboard (index.html, app.js, api.js,
// styles.css) into the API binary. One Deployment then serves both the
// JSON API and the page that consumes it — same origin, no CORS, and
// the UI is versioned atomically with the backend it talks to.
package ui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed index.html api.js app.js styles.css
var files embed.FS

// Handler serves the embedded dashboard. The app is hash-routed, so the
// only server-side paths are "/" and direct asset requests; anything
// else (including a stale deep link) falls back to index.html.
//
// Assets are served via ServeContent from the embed rather than
// http.FileServer, which redirects any path ending in index.html to its
// parent directory — the opposite of what an SPA fallback wants.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		b, err := fs.ReadFile(files, name)
		if err != nil {
			b, err = fs.ReadFile(files, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			name = "index.html"
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
	})
}
