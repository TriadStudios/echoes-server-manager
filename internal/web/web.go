// Package web serves ESM's web page from files embedded in the binary.
//
// The static folder is where the built web interface goes (index.html, one
// script, one stylesheet and a public/ folder). Until it is built, it holds
// a placeholder page that points to the command line. Every response
// carries a strict Content-Security-Policy: no inline scripts or styles,
// nothing loaded from other origins, no framing.
//
// Paths that name no file get index.html, so the page can route on the
// client side. Paths that try to leave the folder or reach something that
// is not a page file get 404 (see fileName); a folder is never listed.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
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
	return serve(files)
}

// serve serves the files of files for GET and HEAD requests.
func serve(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetSecurityHeaders(w.Header())
		name, ok := fileName(r.URL.Path)
		if !ok {
			notFound(w)
			return
		}
		if fi, err := fs.Stat(files, name); name != "" && err == nil && fi.IsDir() {
			notFound(w) // a folder is never listed
			return
		}
		b, err := fs.ReadFile(files, name)
		if name == "" || err != nil {
			name = "index.html"
			if b, err = fs.ReadFile(files, name); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "the page is missing from this build")
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

// fileName turns a request path (as decoded from the URL) into the name of
// a page file, "" for the index. It refuses, whatever the files are, every
// path that could reach outside the page folder or something that is not
// meant to be served, on any system:
//
//   - a "." or ".." segment, however it was written ("%2e%2e" too);
//   - a backslash (also "%5c"), which Windows takes for a folder separator;
//   - a colon, as in a drive letter ("/C:/Windows/win.ini") or a Windows
//     stream name ("file::$DATA");
//   - a segment that starts with a dot (dotfiles and dot folders);
//   - control characters, NUL among them.
func fileName(p string) (string, bool) {
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	if strings.ContainsAny(p, `\:`) || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", false
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if strings.HasPrefix(seg, ".") {
			return "", false
		}
	}
	return strings.TrimPrefix(path.Clean(p), "/"), true
}

func notFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "no such page")
}

// writeError writes ESM's error shape, {"error": {"code", "message"}}.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	b, _ := json.Marshal(map[string]map[string]string{"error": {"code": code, "message": msg}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}
