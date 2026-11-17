package api

import (
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
)

// async is read as strconv.ParseBool reads it, as wait is: what the OpenAPI
// file says. The spellings that mean true start a job, those that mean false
// and an empty value make the call that waits, and any other is 422 and does
// nothing.
func TestAsyncIsReadAsParseBoolReadsIt(t *testing.T) {
	jr := newJobRig(t)
	close(jr.gate) // a self-update ends at once
	for _, v := range []string{"1", "t", "T", "TRUE", "true", "True"} {
		if rec := jr.call(auth.RoleAdmin, "POST", "/api/v1/box/self-update?async="+v, `{"force":true}`); rec.Code != http.StatusAccepted || rec.Header().Get("Location") == "" {
			t.Errorf("async=%s: %d %s", v, rec.Code, rec.Body)
		}
	}
	for _, v := range []string{"0", "f", "F", "FALSE", "false", "False", ""} {
		if rec := jr.call(auth.RoleAdmin, "POST", "/api/v1/box/self-update?async="+v, `{"force":true}`); rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
			t.Errorf("async=%q: %d %s", v, rec.Code, rec.Body)
		}
	}
	// The six jobs and the seven calls that waited have all done their update
	// (a job does it a moment after it is started).
	const started = 6 + 7
	deadline := time.Now().Add(30 * time.Second)
	for {
		jr.mu.Lock()
		n := jr.calls
		jr.mu.Unlock()
		if n == started {
			break
		}
		if n > started || time.Now().After(deadline) {
			t.Fatalf("%d updates after %d calls", n, started)
		}
		runtime.Gosched()
	}
	before := started
	for _, v := range []string{"yes", "no", "on", "off", "tRuE", "fAlSe", "2", "y", "n", "true ", "%20", "null"} {
		rec := jr.call(auth.RoleAdmin, "POST", "/api/v1/box/self-update?async="+url.QueryEscape(v), `{"force":true}`)
		if rec.Code != http.StatusUnprocessableEntity || errorCode(rec) != "invalid" || rec.Header().Get("Location") != "" {
			t.Errorf("async=%q: %d %s", strconv.Quote(v), rec.Code, rec.Body)
		}
	}
	jr.mu.Lock()
	defer jr.mu.Unlock()
	if jr.calls != before {
		t.Errorf("a value that is none started %d updates", jr.calls-before)
	}
}
