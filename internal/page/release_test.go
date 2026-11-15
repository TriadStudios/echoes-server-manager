package page_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/release"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
)

// The ESM version card of the Box screen has a "Check now" button for
// every role while the release check is on. The script sends it to POST
// /api/v1/box/release-check, with the CSRF token like every change, and
// then shows the Box screen again with a fixed notice. The card then shows
// the newest release and when ESM looked, and an admin gets the update
// button. A second check right after asks the release server nothing and
// gets the other notice. While the release server has asked ESM to wait,
// the card says until when and a check by hand asks it nothing. With the
// check off there is no button.
func TestReleaseCheckButton(t *testing.T) {
	off := newSite(t) // in tests no release is looked up unless a server is named
	off.setup()
	if b := off.checkPage("/box", "session", off.csrf, off.session); strings.Contains(b, `data-act="release-check"`) ||
		!strings.Contains(b, "releases are not looked up in tests") {
		t.Errorf("the Box screen with the check off offers a check:\n%s", b)
	}

	var looks atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		looks.Add(1)
		json.NewEncoder(w).Encode(release.Release{Tag: "v0.2.0", URL: "https://example.invalid/v0.2.0"})
	}))
	t.Cleanup(srv.Close)
	s := newSite(t, func(o *service.Options) {
		o.Version, o.ReleaseAPI, o.ReleaseHTTP = "v0.1.0", srv.URL+"/latest", srv.Client()
	})
	s.setup()
	read := s.token("read")
	if n := looks.Load(); n != 0 {
		t.Fatalf("%d lookups before anyone looked at the box", n)
	}

	// What the script sends, without the CSRF token: refused, nothing
	// looked up.
	req, _ := http.NewRequest("POST", s.base+"/api/v1/box/release-check", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.session})
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || looks.Load() != 0 {
		t.Fatalf("a check without the CSRF token: %d, %d lookups", resp.StatusCode, looks.Load())
	}
	// And with it, as the script sends it.
	var st release.Status
	s.must(http.StatusOK, "POST", "/api/v1/box/release-check", "session", struct{}{}, &st)
	if st.Latest != "v0.2.0" || !st.Available || st.Reused || st.Current != "v0.1.0" || looks.Load() != 1 {
		t.Fatalf("the check: %+v (%d lookups)", st, looks.Load())
	}

	admin := s.checkPage("/box?done=release", "session", s.csrf, s.session)
	rd := s.checkPage("/box", read, "", read)
	for who, b := range map[string]string{"admin": admin, "read": rd} {
		for _, want := range []string{
			`<button type="button" class="btn" data-act="release-check">Check now</button>`,
			`<dd class="mono">v0.2.0</dd>`,
			"ESM v0.2.0 is out.",
			`<dt>Last looked</dt><dd><time datetime="`,
		} {
			if !strings.Contains(b, want) {
				t.Errorf("the Box screen of %s lacks %q", who, want)
			}
		}
	}
	if !strings.Contains(admin, `data-act="self-update"`) || !strings.Contains(admin, "Update ESM to v0.2.0") || strings.Contains(rd, `data-act="self-update"`) {
		t.Error("the update button after the check: admins only")
	}
	if !strings.Contains(admin, "Looked for a newer ESM release; the result is under ESM version.") {
		t.Error("the notice after a check")
	}
	if n := looks.Load(); n != 1 {
		t.Fatalf("showing the result looked it up again: %d lookups", n)
	}

	// Right after, with a read token: the same result, not looked up again.
	s.must(http.StatusOK, "POST", "/api/v1/box/release-check", read, struct{}{}, &st)
	if !st.Reused || st.Latest != "v0.2.0" || looks.Load() != 1 {
		t.Fatalf("a check right after: %+v (%d lookups)", st, looks.Load())
	}
	if _, b := s.get("/box?done=release-recent", "session"); !strings.Contains(b, "ESM looked less than a minute ago, so that result is shown; it did not ask again.") {
		t.Error("the notice after a reused check")
	}

	// A release server that refuses as asked too often: the card says until
	// when ESM waits, and a check by hand meanwhile asks it nothing.
	var refused atomic.Int32
	limited := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refused.Add(1)
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(limited.Close)
	lim := newSite(t, func(o *service.Options) {
		o.Version, o.ReleaseAPI, o.ReleaseHTTP = "v0.1.0", limited.URL+"/latest", limited.Client()
	})
	lim.setup()
	lim.must(http.StatusOK, "POST", "/api/v1/box/release-check", "session", struct{}{}, &st)
	if st.RetryAfter.IsZero() || st.Reused || !strings.Contains(st.Note, "asked to wait until") || refused.Load() != 1 {
		t.Fatalf("a check refused with 429: %+v (%d lookups)", st, refused.Load())
	}
	lim.must(http.StatusOK, "POST", "/api/v1/box/release-check", "session", struct{}{}, &st)
	if !st.Reused || st.RetryAfter.IsZero() || refused.Load() != 1 {
		t.Fatalf("a check while waiting: %+v (%d lookups)", st, refused.Load())
	}
	b := lim.checkPage("/box?done=release-wait", "session", lim.csrf, lim.session)
	if !strings.Contains(b, "The release server asked ESM to wait before it asks again; the note under ESM version says until when.") ||
		!strings.Contains(b, "the release server asked to wait until "+st.RetryAfter.UTC().Format(time.RFC3339)) {
		t.Errorf("the Box screen while ESM waits:\n%s", b)
	}
	if refused.Load() != 1 {
		t.Fatalf("%d lookups while ESM waits", refused.Load())
	}

	_, js := s.get("/static/esm.js", "")
	for _, need := range []string{
		"'release-check': async function ()",
		"call('POST', '/api/v1/box/release-check')",
		"var code = r.retry_after ? 'release-wait' : (r.reused ? 'release-recent' : 'release');",
		"return '/box?done=' + code + '#esm-h';",
		"go(releaseCheckPath(r));",
	} {
		if !strings.Contains(js, need) {
			t.Errorf("esm.js lacks %q", need)
		}
	}
}
