package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// files stands for a page folder, with things in it that must never be
// served.
var files = fstest.MapFS{
	"esm.css":            {Data: []byte("body{}")},
	"esm.js":             {Data: []byte("'use strict';")},
	"public/logo.svg":    {Data: []byte("<svg></svg>")},
	"public/.hidden":     {Data: []byte("SECRET")},
	".env":               {Data: []byte("SECRET=1")},
	".git/config":        {Data: []byte("SECRET")},
	"public/deep/a.json": {Data: []byte("{}")},
}

func get(t *testing.T, p string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Files(files).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
	return rec
}

func TestSecurityHeadersExact(t *testing.T) {
	want := "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
		"font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	for _, p := range []string{"/esm.css", "/esm.js", "/missing", "/.env"} {
		h := get(t, p).Header()
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

func TestFilesAndTypes(t *testing.T) {
	for p, ct := range map[string]string{
		"/esm.css":         "text/css; charset=utf-8",
		"/esm.js":          "text/javascript; charset=utf-8",
		"/public/logo.svg": "image/svg+xml",
	} {
		rec := get(t, p)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ct {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	rec := httptest.NewRecorder()
	Files(files).ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/esm.css", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d %q", rec.Code, rec.Body)
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
	for _, leak := range []string{"module ", "SECRET", "<svg", "body{", "use strict", "{}\n"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("%s: the answer shows a file: %q", what, rec.Body)
		}
	}
}

// No request path leaves the folder or reaches a file that is not meant to
// be served, however it is written: dot segments (also encoded),
// backslashes (also %5c), drive letters and Windows stream names, dotfiles,
// control characters.
func TestNoWalkOut(t *testing.T) {
	for _, target := range []string{
		"/../../go.mod",
		"/%2e%2e/%2e%2e/go.mod",
		"/%2E%2E/%2E%2E/go.mod",
		"/..%2f..%2fgo.mod",
		"/esm.css/../../go.mod",
		"/./esm.css",
		"/..%5c..%5cgo.mod",
		"/..%5C..%5Cwindows%5Cwin.ini",
		"/%5c%5cserver%5cshare%5cfile",
		"/C:/Windows/win.ini",
		"/c:%5cwindows%5cwin.ini",
		"/%43:/Windows/win.ini",
		"/C:",
		"/esm.css::$DATA",
		"/.env",
		"/.git/config",
		"/%2egit/config",
		"/public/.hidden",
		"/public/%2ehidden",
		"/esm.css%00.txt",
		"/a%0d%0aSet-Cookie:x",
		"/missing.css",
	} {
		checkNotFound(t, target, get(t, target))
	}
	// Literal backslashes, as some clients send them unencoded.
	for _, p := range []string{`/..\..\go.mod`, `/C:\Windows\win.ini`, `/public\logo.svg`} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.URL.Path = p
		rec := httptest.NewRecorder()
		Files(files).ServeHTTP(rec, r)
		checkNotFound(t, p, rec)
	}
}

// A folder is never listed, also the top one.
func TestNoListings(t *testing.T) {
	for _, p := range []string{"/", "/public", "/public/", "/public/deep", "/public/deep/"} {
		checkNotFound(t, p, get(t, p))
	}
}
