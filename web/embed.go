// Package web embeds the built WebUI (web/dist, from `mise run
// web:build`) and serves it as a single-page app.
//
// dist is tracked with only a .gitkeep so that a fresh clone compiles;
// such a binary answers every page request with a note to build the UI.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// csp allows nothing but the app's own scripts and styles. Mail bodies are
// rendered as text in M1; when M2 adds the sandboxed iframe (§12.2), its
// frame-src/child-src goes here.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Handler serves the WebUI. Files under assets/ carry a content hash in
// their name and are cached for a year; index.html is never cached, so a
// new build is picked up on the next load. Any other path that is not a
// file gets index.html: the client routes it (/enroll, /recover, ...).
func Handler() http.Handler {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	return serve(root)
}

func serve(root fs.FS) http.Handler {
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("The WebUI is not built into this binary. Run `mise run web:build` and rebuild yuuka.\n"))
		})
	}
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" && !strings.HasPrefix(path.Base(name), ".") {
			if st, err := fs.Stat(root, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					h.Set("Cache-Control", "no-cache")
				}
				files.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(name, "assets/") {
				// A stale hashed asset must 404, not get HTML with a 200.
				http.NotFound(w, r)
				return
			}
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		_, _ = w.Write(index)
	})
}
