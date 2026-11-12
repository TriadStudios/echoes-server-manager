package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	escape := get(t, "/../../go.mod")
	if escape.Code != http.StatusOK || escape.Body.String() != body {
		t.Fatalf("escape attempt: %d", escape.Code)
	}
}
