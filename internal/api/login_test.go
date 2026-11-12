package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Login and setup take a JSON body only (a form on another site cannot
// send one) and refuse a request from another origin. Both checks come
// before anything else, so nothing else is needed to answer here.
func TestLoginAndSetupNeedJSONFromTheSameOrigin(t *testing.T) {
	h := New(Options{}).Handler()
	for _, path := range []string{"/api/v1/login", "/api/v1/setup"} {
		cases := []struct {
			ctype, origin string
			status        int
			code          string
		}{
			{"", "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
			{"application/x-www-form-urlencoded", "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
			{"multipart/form-data; boundary=x", "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
			{"text/plain", "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
			{"application/json", "http://evil.example", http.StatusForbidden, "origin"},
			{"application/json", "http://example.com:8741", http.StatusForbidden, "origin"},
			{"application/json", "null", http.StatusForbidden, "origin"},
			{"application/json", "https://example.com:8740", http.StatusForbidden, "origin"},
			{"text/plain", "http://evil.example", http.StatusForbidden, "origin"},
		}
		for _, c := range cases {
			r := httptest.NewRequest(http.MethodPost, "http://example.com:8740"+path, strings.NewReader(`{"username":"a","password":"b","code":"c"}`))
			if c.ctype != "" {
				r.Header.Set("Content-Type", c.ctype)
			}
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			var eb ErrorBody
			json.Unmarshal(rec.Body.Bytes(), &eb)
			if rec.Code != c.status || eb.Error.Code != c.code {
				t.Errorf("%s with %q from %q: %d %s, want %d %s", path, c.ctype, c.origin, rec.Code, rec.Body, c.status, c.code)
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Errorf("%s with %q from %q: a cookie was set", path, c.ctype, c.origin)
			}
		}
	}
}
