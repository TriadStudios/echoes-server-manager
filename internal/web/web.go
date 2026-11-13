// Package web holds what every reply ESM writes to a browser shares: the
// security headers (a strict Content-Security-Policy among them), and a
// file server for the web page's static files that cannot be walked out
// of.
//
// The Content-Security-Policy allows no inline scripts or styles, nothing
// loaded from other origins, and no framing.
package web

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// CSP is the Content-Security-Policy of every response.
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
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

// Files serves the files of files for GET and HEAD requests, at the
// request path. Everything else is 404 in ESM's JSON error shape: a path
// that names no file, a folder (never listed), and every path that could
// reach outside files or something not meant to be served (see fileName).
func Files(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetSecurityHeaders(w.Header())
		name, ok := fileName(r.URL.Path)
		if !ok || name == "" {
			NotFound(w)
			return
		}
		fi, err := fs.Stat(files, name)
		if err != nil || !fi.Mode().IsRegular() {
			NotFound(w) // missing, or a folder: never listed
			return
		}
		b, err := fs.ReadFile(files, name)
		if err != nil {
			NotFound(w)
			return
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
// a file, "" for the folder itself. It refuses, whatever the files are,
// every path that could reach outside the folder or something that is not
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

// NotFound writes the 404 answer of a refused or missing file.
func NotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "no such file")
}

// writeError writes ESM's error shape, {"error": {"code", "message"}}.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	b, _ := json.Marshal(map[string]map[string]string{"error": {"code": code, "message": msg}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}
