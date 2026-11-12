package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"testing/fstest"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestSecurityHeadersExact(t *testing.T) {
	want := "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
		"font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	for _, p := range []string{"/", "/style.css", "/instances/alpha/logs"} {
		rec := get(t, p)
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got != want {
			t.Errorf("%s: CSP %q, want %q", p, got, want)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: headers %v", p, h)
		}
		if strings.Contains(h.Get("Content-Security-Policy"), "unsafe") {
			t.Errorf("%s: CSP allows unsafe sources", p)
		}
	}
}

func TestIndexAndFallback(t *testing.T) {
	rec := get(t, "/")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("index: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not built yet") || !strings.Contains(body, `href="/style.css"`) {
		t.Fatalf("index body: %s", body)
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "<style") || strings.Contains(body, "style=") {
		t.Fatal("index has inline script or style")
	}
	css := get(t, "/style.css")
	if css.Code != http.StatusOK || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("style.css: %d %s", css.Code, css.Header().Get("Content-Type"))
	}
	deep := get(t, "/instances/alpha/settings")
	if deep.Code != http.StatusOK || deep.Body.String() != body {
		t.Fatalf("client route fallback: %d", deep.Code)
	}
}

// checkNotFound asserts the 404 of a refused path: ESM's error shape, the
// security headers (frame-ancestors too), and nothing of a file.
func checkNotFound(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	var eb struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if rec.Code != http.StatusNotFound || json.Unmarshal(rec.Body.Bytes(), &eb) != nil || eb.Error.Code != "not_found" {
		t.Errorf("%s: %d %q, want the 404 error", what, rec.Code, rec.Body)
	}
	h := rec.Header()
	if h.Get("Content-Security-Policy") != CSP || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%s: security headers missing: %v", what, h)
	}
	for _, leak := range []string{"module ", "[fonts]", "SECRET", "logo"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("%s: the answer shows a file: %q", what, rec.Body)
		}
	}
}

// No request path leaves the page folder or reaches a file that is not
// meant to be served, however it is written: dot segments (also encoded),
// backslashes (also %5c), drive letters and Windows stream names, dotfiles,
// control characters.
func TestNoWalkOut(t *testing.T) {
	for _, target := range []string{
		"/../../go.mod",
		"/%2e%2e/%2e%2e/go.mod",
		"/%2E%2E/%2E%2E/go.mod",
		"/..%2f..%2fgo.mod",
		"/style.css/../../go.mod",
		"/./style.css",
		"/..%5c..%5cgo.mod",
		"/..%5C..%5Cwindows%5Cwin.ini",
		"/%5c%5cserver%5cshare%5cfile",
		"/C:/Windows/win.ini",
		"/c:%5cwindows%5cwin.ini",
		"/%43:/Windows/win.ini",
		"/C:",
		"/style.css::$DATA",
		"/.env",
		"/.git/config",
		"/%2egit/config",
		"/public/.hidden",
		"/index.html%00.txt",
		"/a%0d%0aSet-Cookie:x",
	} {
		checkNotFound(t, target, get(t, target))
	}
	// Literal backslashes, as some clients send them unencoded.
	for _, p := range []string{`/..\..\go.mod`, `/C:\Windows\win.ini`, `/static\index.html`} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.URL.Path = p
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, r)
		checkNotFound(t, p, rec)
	}
}

// A folder is never listed, and dotfiles are never served, also when the
// page folder has them.
func TestNoListingsNoDotfiles(t *testing.T) {
	files := fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><p>index</p>")},
		"public/logo.svg":    {Data: []byte("<svg></svg>")},
		"public/.hidden":     {Data: []byte("SECRET")},
		".env":               {Data: []byte("SECRET=1")},
		"public/deep/a.json": {Data: []byte("{}")},
	}
	h := serve(files)
	do := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec
	}
	for _, p := range []string{"/public", "/public/", "/public/deep", "/public/deep/"} {
		checkNotFound(t, p, do(p))
	}
	for _, p := range []string{"/.env", "/public/.hidden", "/public/%2ehidden"} {
		checkNotFound(t, p, do(p))
	}
	if rec := do("/public/logo.svg"); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("a file in a folder: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := do("/somewhere/else"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "index") {
		t.Fatalf("client route: %d %q", rec.Code, rec.Body)
	}
	// The embedded page folder has no dotfiles at all.
	sub, _ := fs.Sub(static, "static")
	fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if p != "." && strings.HasPrefix(path.Base(p), ".") {
			t.Errorf("the page folder has %s", p)
		}
		return nil
	})
}
