// Package web serves ESM's web page from files embedded in the binary.
//
// The static folder is where the built web interface goes (index.html, one
// script, one stylesheet and a public/ folder). Until it is built, it holds
// a placeholder page that points to the command line. Every response
// carries a strict Content-Security-Policy: no inline scripts or styles,
// nothing loaded from other origins, no framing.
//
// Paths that name no file get index.html, so the page can route on the
// client side.
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed static
var static embed.FS

// CSP is the Content-Security-Policy of every page response.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// SetSecurityHeaders adds the security headers ESM sends with every
// response.
func SetSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

// types maps file extensions to content types. A fixed list keeps answers
// the same on every system (the system's own table may differ).
var types = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

// Handler serves the embedded files for GET and HEAD requests.
func Handler() http.Handler {
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetSecurityHeaders(w.Header())
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		b, err := fs.ReadFile(files, name)
		if name == "" || err != nil {
			name = "index.html"
			if b, err = fs.ReadFile(files, name); err != nil {
				http.Error(w, "page missing", http.StatusInternalServerError)
				return
			}
		}
		ct, ok := types[path.Ext(name)]
		if !ok {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
	})
}
