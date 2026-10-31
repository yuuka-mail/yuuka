package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return w
}

func TestServe(t *testing.T) {
	t.Parallel()
	h := serve(fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html>shell")},
		"favicon.svg":          {Data: []byte("<svg/>")},
		"assets/index-abc.js":  {Data: []byte("js")},
		".gitkeep":             {},
		"assets/index-abc.css": {Data: []byte("css")},
	})
	cases := []struct {
		path, body, cache string
		code              int
	}{
		{"/", "<!doctype html>shell", "no-store", 200},
		{"/enroll", "<!doctype html>shell", "no-store", 200},     // client route
		{"/index.html", "<!doctype html>shell", "no-store", 200}, // never via the file server's redirect
		{"/assets/index-abc.js", "js", "public, max-age=31536000, immutable", 200},
		{"/favicon.svg", "<svg/>", "no-cache", 200},
		{"/assets/gone-123.js", "404 page not found\n", "", 404}, // stale asset: no HTML with 200
		{"/.gitkeep", "<!doctype html>shell", "no-store", 200},   // dotfiles are not served
	}
	for _, c := range cases {
		w := get(t, h, c.path)
		if w.Code != c.code || w.Body.String() != c.body || w.Header().Get("Cache-Control") != c.cache {
			t.Errorf("%s: %d %q cache %q", c.path, w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
		}
	}
	if got := get(t, h, "/").Header().Get("Content-Security-Policy"); got != csp {
		t.Errorf("CSP %q", got)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestServeUnbuilt(t *testing.T) {
	t.Parallel()
	w := get(t, serve(fstest.MapFS{".gitkeep": {}}), "/")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unbuilt: %d", w.Code)
	}
}
