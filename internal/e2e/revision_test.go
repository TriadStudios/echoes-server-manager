package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// callWith sends a request over TCP with the bearer token and these header
// lines (a name may have several values, each sent as a line of its own),
// and returns the status, the headers and the body of the answer.
func (h *harness) callWith(token, method, path string, body any, lines http.Header) (int, http.Header, string) {
	h.t.Helper()
	req := newRequest(h.t, method, h.base+path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	for name, vals := range lines {
		for _, v := range vals {
			req.Header.Add(name, v)
		}
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func jsonUnmarshal(raw string, v any) error { return json.Unmarshal([]byte(raw), v) }

// ifMatch is the header lines of a change made for the revisions given
// (one line each).
func ifMatch(vals ...string) http.Header { return http.Header{"If-Match": vals} }

func errorOf(t *testing.T, raw string) api.ErrorDetail {
	t.Helper()
	var eb api.ErrorBody
	if err := jsonUnmarshal(raw, &eb); err != nil {
		t.Fatalf("not an error answer: %q: %v", raw, err)
	}
	return eb.Error
}

// An instance shows its revision wherever it is shown, with the same
// number as the ETag of the calls that give one instance: made at 1, and
// every stored change adds 1 (a change of settings, also one that sets
// nothing, and a change of secrets). A call without If-Match, with
// If-Match: * or with the revision the instance is at is made; one for any
// other revision is 412 "changed" and does nothing, which the message says
// with the revision the instance is at; a header that is not "*" or one
// revision in quotes is 400 "bad_if_match", before anything is looked at.
func TestRevisionAndIfMatchOverTheAPI(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	const path = "/api/v1/instances/alpha"

	// Creating: revision 1, with its ETag.
	st, hdr, raw := h.callWith(h.admin, "POST", "/api/v1/instances", map[string]any{"name": "alpha", "bind_ip": "127.0.0.1", "build": "1.0", "rules": fastRules(nil)}, nil)
	if st != http.StatusCreated || hdr.Get("ETag") != `"1"` || !strings.Contains(raw, `"revision":1`) {
		t.Fatalf("create: %d, ETag %q: %s", st, hdr.Get("ETag"), raw)
	}

	get := func(token string) (instances.View, string) {
		t.Helper()
		st, hdr, raw := h.callWith(token, "GET", path, nil, nil)
		if st != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, st, raw)
		}
		var v instances.View
		if err := jsonUnmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v, hdr.Get("ETag")
	}
	rev := func() int64 { v, tag := get(h.admin); checkTag(t, v.Revision, tag); return v.Revision }
	listed := func(token string) int64 {
		t.Helper()
		var list api.InstanceList
		if st := h.call("GET", "/api/v1/instances", token, nil, &list); st != http.StatusOK || len(list.Instances) != 1 {
			t.Fatalf("list: %d %+v", st, list)
		}
		return list.Instances[0].Revision
	}
	if rev() != 1 || listed(h.admin) != 1 || listed(h.token("read")) != 1 {
		t.Fatal("a new instance is not at revision 1 everywhere")
	}

	// A change without If-Match: as before, and one more revision.
	st, hdr, raw = h.callWith(h.admin, "PATCH", path, map[string]any{"display_name": "Alpha"}, nil)
	var out api.InstanceChange
	if err := jsonUnmarshal(raw, &out); st != http.StatusOK || err != nil || out.Revision != 2 || hdr.Get("ETag") != `"2"` || out.DisplayName != "Alpha" {
		t.Fatalf("PATCH without If-Match: %d, ETag %q, revision %d: %s", st, hdr.Get("ETag"), out.Revision, raw)
	}
	// Nothing to set is a stored change too.
	if st, _, raw = h.callWith(h.admin, "PATCH", path, map[string]any{}, nil); st != http.StatusOK || rev() != 3 {
		t.Fatalf("an empty change: %d %s", st, raw)
	}

	// The revision it is at: made.
	st, hdr, raw = h.callWith(h.admin, "PATCH", path, map[string]any{"max_players": 12}, ifMatch(`"3"`))
	if err := jsonUnmarshal(raw, &out); st != http.StatusOK || err != nil || out.Revision != 4 || hdr.Get("ETag") != `"4"` || out.MaxPlayers != 12 {
		t.Fatalf("PATCH with If-Match \"3\": %d, ETag %q: %s", st, hdr.Get("ETag"), raw)
	}
	// The same call again: the instance is at 4 now.
	before, _ := get(h.admin)
	st, hdr, raw = h.callWith(h.admin, "PATCH", path, map[string]any{"max_players": 13}, ifMatch(`"3"`))
	if e := errorOf(t, raw); st != http.StatusPreconditionFailed || e.Code != "changed" || !strings.Contains(e.Message, "changed since they were read") ||
		!strings.Contains(e.Message, "revision 4") || !strings.Contains(e.Message, "revision 3") {
		t.Fatalf("PATCH with a stale If-Match: %d %s", st, raw)
	}
	if after, _ := get(h.admin); after.MaxPlayers != before.MaxPlayers || after.Revision != 4 || !after.Updated.Equal(before.Updated) {
		t.Fatalf("a refused change changed the instance: %+v then %+v", before.Instance, after.Instance)
	}
	// A revision the instance never had is refused the same way.
	if st, _, raw = h.callWith(h.admin, "PATCH", path, map[string]any{"max_players": 14}, ifMatch(`"900"`)); st != http.StatusPreconditionFailed || errorOf(t, raw).Code != "changed" {
		t.Fatalf("PATCH with If-Match \"900\": %d %s", st, raw)
	}
	// * is any revision.
	if st, _, raw = h.callWith(h.admin, "PATCH", path, map[string]any{"max_players": 15}, ifMatch("*")); st != http.StatusOK || rev() != 5 {
		t.Fatalf("PATCH with If-Match *: %d %s", st, raw)
	}
	if st, _, raw = h.callWith(h.admin, "PATCH", path, map[string]any{"max_players": 16}, ifMatch(" * ")); st != http.StatusOK || rev() != 6 {
		t.Fatalf("PATCH with If-Match ' * ': %d %s", st, raw)
	}

	// What is not "*" or one revision in quotes is 400, whatever the
	// instance is at, and nothing is done.
	for _, bad := range [][]string{{"6"}, {`W/"6"`}, {`"abc"`}, {`"0"`}, {`"06"`}, {`"-6"`}, {`"6`}, {`6"`}, {`'6'`}, {`"6", "7"`}, {`"6",*`}, {`*, "6"`}, {`"99999999999999999999"`},
		{""}, {`"6"`, `"6"`}, {`"6"`, "*"}, {"**"}, {"null"}} {
		st, _, raw := h.callWith(h.admin, "PATCH", path, map[string]any{"max_players": 20}, ifMatch(bad...))
		if e := errorOf(t, raw); st != http.StatusBadRequest || e.Code != "bad_if_match" || !strings.Contains(e.Message, "If-Match") {
			t.Errorf("PATCH with If-Match %q: %d %s", bad, st, raw)
		}
		st, _, raw = h.callWith(h.admin, "PUT", path+"/secrets", map[string]any{"server_password": "join me 7"}, ifMatch(bad...))
		if e := errorOf(t, raw); st != http.StatusBadRequest || e.Code != "bad_if_match" {
			t.Errorf("PUT secrets with If-Match %q: %d %s", bad, st, raw)
		}
	}
	if v, _ := get(h.admin); v.Revision != 6 || v.MaxPlayers != 16 || v.Secrets.ServerPasswordSet {
		t.Fatalf("a refused call changed the instance: %+v", v)
	}

	// Secrets: the same condition, the revision in the answer and its
	// ETag, and a stored change that makes an older revision stale.
	st, hdr, raw = h.callWith(h.admin, "PUT", path+"/secrets", map[string]any{"server_password": "join me 7"}, ifMatch(`"5"`))
	if e := errorOf(t, raw); st != http.StatusPreconditionFailed || e.Code != "changed" || !strings.Contains(e.Message, "revision 6") {
		t.Fatalf("PUT secrets with a stale If-Match: %d %s", st, raw)
	}
	if v, _ := get(h.admin); v.Secrets.ServerPasswordSet || v.Revision != 6 {
		t.Fatalf("a refused change of secrets changed the instance: %+v", v)
	}
	st, hdr, raw = h.callWith(h.admin, "PUT", path+"/secrets", map[string]any{"server_password": "join me 7"}, ifMatch(`"6"`))
	var sr api.SecretsResult
	if err := jsonUnmarshal(raw, &sr); st != http.StatusOK || err != nil || sr.Revision != 7 || hdr.Get("ETag") != `"7"` || !sr.Secrets.ServerPasswordSet {
		t.Fatalf("PUT secrets with If-Match \"6\": %d, ETag %q: %s", st, hdr.Get("ETag"), raw)
	}
	if st, _, raw = h.callWith(h.admin, "PATCH", path, map[string]any{"max_players": 17}, ifMatch(`"6"`)); st != http.StatusPreconditionFailed {
		t.Fatalf("a change for the revision before the secrets changed: %d %s", st, raw)
	}
	if st, _, raw = h.callWith(h.admin, "PUT", path+"/secrets", map[string]any{"admin_password": "admin 8 chars"}, nil); st != http.StatusOK || rev() != 8 {
		t.Fatalf("PUT secrets without If-Match: %d %s", st, raw)
	}

	// If-Match is read on these two calls only: a start, a stop or a read
	// with one that does not fit is not refused for it.
	if st, _, raw = h.callWith(h.admin, "GET", path, nil, ifMatch(`"1"`)); st != http.StatusOK {
		t.Fatalf("GET with If-Match: %d %s", st, raw)
	}

	// The refused change is in the audit log as a failed change.
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=500", nil, &al)
	failed := 0
	for _, e := range al.Entries {
		if e.Action == "instance.change" && !e.OK && strings.Contains(e.Error, "changed since they were read") {
			failed++
		}
	}
	if failed < 3 {
		t.Errorf("the audit log has %d refused changes of a stale revision, want at least 3", failed)
	}
}

func checkTag(t *testing.T, rev int64, tag string) {
	t.Helper()
	if tag != fmt.Sprintf("%q", fmt.Sprint(rev)) {
		t.Fatalf("revision %d, ETag %s", rev, tag)
	}
}

// Of changes made at the same time for the same revision, over HTTP,
// exactly one is made (200) and the others are 412; the instance holds what
// the one set and is at the next revision.
func TestOnlyOnePatchForOneRevisionWinsOverHTTP(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "1.0", fastRules(nil))
	const path = "/api/v1/instances/alpha"
	const n = 16
	for round := 0; round < 3; round++ {
		var v instances.View
		h.must(http.StatusOK, "GET", path, nil, &v)
		tag := fmt.Sprintf("%q", fmt.Sprint(v.Revision))
		codes := make([]int, n)
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				codes[i], _, _ = h.callWith(h.admin, "PATCH", path, map[string]any{"display_name": fmt.Sprintf("writer-%02d", i)}, ifMatch(tag))
			}()
		}
		close(gate)
		wg.Wait()
		won, refused := -1, 0
		for i, c := range codes {
			switch c {
			case http.StatusOK:
				if won >= 0 {
					t.Fatalf("round %d: PATCHes %d and %d were both made for revision %d", round, won, i, v.Revision)
				}
				won = i
			case http.StatusPreconditionFailed:
				refused++
			default:
				t.Fatalf("round %d: PATCH %d: %d", round, i, c)
			}
		}
		if won < 0 || refused != n-1 {
			t.Fatalf("round %d: %d made, %d refused (%v)", round, n-refused, refused, codes)
		}
		var after instances.View
		h.must(http.StatusOK, "GET", path, nil, &after)
		if after.Revision != v.Revision+1 || after.DisplayName != fmt.Sprintf("writer-%02d", won) {
			t.Fatalf("round %d: revision %d, display name %q; PATCH %d won at revision %d", round, after.Revision, after.DisplayName, won, v.Revision)
		}
	}
}

// A panel that changes the rules (the whole map is replaced) reads, merges
// its change into the rules it read and writes with If-Match. A change made
// meanwhile, on the page or the command line, is never overwritten: the
// panel is told (412), reads again, merges again and goes on.
func TestPanelChangesRulesWithoutOverwritingAnotherChange(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	site := h.token("site")
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "1.0", fastRules(map[string]string{"Banner": "one"}))
	const path = "/api/v1/instances/alpha"

	read := func() (instances.View, string) {
		t.Helper()
		st, hdr, raw := h.callWith(site, "GET", path, nil, nil)
		var v instances.View
		if err := jsonUnmarshal(raw, &v); st != http.StatusOK || err != nil {
			t.Fatalf("site GET: %d %s", st, raw)
		}
		return v, hdr.Get("ETag")
	}
	v, tag := read()
	checkTag(t, v.Revision, tag)

	// While the panel works on what it read, an admin sets another rule.
	h.must(http.StatusOK, "PATCH", path, map[string]any{"rules": fastRules(map[string]string{"Banner": "one", "MOTD": "from the page"})}, nil)

	merged := map[string]string{}
	for k, val := range v.Rules {
		merged[k] = val
	}
	merged["HarvestRate"] = "2"
	st, _, raw := h.callWith(site, "PATCH", path, map[string]any{"rules": merged}, ifMatch(tag))
	if e := errorOf(t, raw); st != http.StatusPreconditionFailed || e.Code != "changed" {
		t.Fatalf("the panel's change over the one made meanwhile: %d %s", st, raw)
	}
	if now, _ := read(); now.Rules["MOTD"] != "from the page" || now.Rules["HarvestRate"] != "" {
		t.Fatalf("rules after the refused change: %v", now.Rules)
	}

	// Read again, merge again, write.
	v, tag = read()
	merged = map[string]string{}
	for k, val := range v.Rules {
		merged[k] = val
	}
	merged["HarvestRate"] = "2"
	st, hdr, raw := h.callWith(site, "PATCH", path, map[string]any{"rules": merged}, ifMatch(tag))
	if st != http.StatusOK || hdr.Get("ETag") != fmt.Sprintf("%q", fmt.Sprint(v.Revision+1)) {
		t.Fatalf("the panel's change after reading again: %d, ETag %q: %s", st, hdr.Get("ETag"), raw)
	}
	if now, _ := read(); now.Rules["MOTD"] != "from the page" || now.Rules["HarvestRate"] != "2" || now.Rules["Banner"] != "one" {
		t.Fatalf("rules after the merge: %v", now.Rules)
	}
}

// For the site role the limits on what it may change come first: a body
// with a field it may not change is 403 whatever If-Match says (the
// revision it names, an old one, "*", a header that is not one) and whether
// the revision matches, and the answer is the same in every case, so it
// tells nothing about the revision. A change of fields it may change is
// conditional like an admin's. It never changes secrets, with If-Match or
// without.
func TestSiteTokenAndIfMatch(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	site := h.token("site")
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "1.0", fastRules(nil))
	const path = "/api/v1/instances/alpha"
	var v instances.View
	h.must(http.StatusOK, "GET", path, nil, &v)
	cur := fmt.Sprintf("%q", fmt.Sprint(v.Revision))
	old := fmt.Sprintf("%q", fmt.Sprint(v.Revision+7))

	var first string
	for _, c := range []http.Header{nil, ifMatch(cur), ifMatch(old), ifMatch("*"), ifMatch("banana"), ifMatch(""), ifMatch(cur, cur), ifMatch(`W/` + cur)} {
		st, _, raw := h.callWith(site, "PATCH", path, map[string]any{"display_name": "Hijacked", "extra_args": []string{"-Verbose"}}, c)
		if e := errorOf(t, raw); st != http.StatusForbidden || e.Code != "forbidden" || !strings.Contains(e.Message, "may not change extra_args") {
			t.Fatalf("site token, a refused field, If-Match %q: %d %s", c.Values("If-Match"), st, raw)
		}
		if first == "" {
			first = raw
		} else if raw != first {
			t.Errorf("site token, a refused field, If-Match %q: the answer differs from the one without:\n%s\n%s", c.Values("If-Match"), raw, first)
		}
	}
	st, _, raw := h.callWith(site, "PATCH", path, map[string]any{"limits": map[string]any{"memory_max": "4G"}}, ifMatch(old))
	if e := errorOf(t, raw); st != http.StatusForbidden || e.Code != "forbidden" {
		t.Fatalf("site token, a refused field with a stale If-Match: %d %s", st, raw)
	}
	var now instances.View
	if h.must(http.StatusOK, "GET", path, nil, &now); now.Revision != v.Revision || now.DisplayName != v.DisplayName {
		t.Fatalf("a refused call changed the instance: %+v", now.Instance)
	}

	// A field it may change: conditional.
	if st, _, raw = h.callWith(site, "PATCH", path, map[string]any{"display_name": "Alpha"}, ifMatch(old)); st != http.StatusPreconditionFailed || errorOf(t, raw).Code != "changed" {
		t.Fatalf("site token, a stale If-Match: %d %s", st, raw)
	}
	st, hdr, raw := h.callWith(site, "PATCH", path, map[string]any{"display_name": "Alpha"}, ifMatch(cur))
	if st != http.StatusOK || hdr.Get("ETag") != fmt.Sprintf("%q", fmt.Sprint(v.Revision+1)) {
		t.Fatalf("site token, a change for the revision it is at: %d, ETag %q: %s", st, hdr.Get("ETag"), raw)
	}
	if st, _, raw = h.callWith(site, "PATCH", path, map[string]any{"display_name": "Alpha 2"}, ifMatch("*")); st != http.StatusOK {
		t.Fatalf("site token, If-Match *: %d %s", st, raw)
	}
	if st, _, raw = h.callWith(site, "PATCH", path, map[string]any{"display_name": "Alpha 3"}, ifMatch("banana")); st != http.StatusBadRequest || errorOf(t, raw).Code != "bad_if_match" {
		t.Fatalf("site token, a malformed If-Match: %d %s", st, raw)
	}
	// Secrets stay the admin's.
	for _, c := range []http.Header{nil, ifMatch("*"), ifMatch(`"1"`)} {
		if st, _, raw = h.callWith(site, "PUT", path+"/secrets", map[string]any{"server_password": "join me 7"}, c); st != http.StatusForbidden || errorOf(t, raw).Code != "forbidden" {
			t.Fatalf("site token, secrets with If-Match %q: %d %s", c.Values("If-Match"), st, raw)
		}
	}
	// It reads the revision like every role.
	if st, hdr, raw = h.callWith(site, "GET", path, nil, nil); st != http.StatusOK || hdr.Get("ETag") == "" || !strings.Contains(raw, `"revision":`) {
		t.Fatalf("site token reads: %d, ETag %q", st, hdr.Get("ETag"))
	}
	for _, role := range []string{"read", "operator"} {
		if st, hdr, _ := h.callWith(h.token(role), "GET", path, nil, nil); st != http.StatusOK || hdr.Get("ETag") == "" {
			t.Errorf("%s token reads: %d, ETag %q", role, st, hdr.Get("ETag"))
		}
		// And is refused the change as before, whatever If-Match says.
		if st, _, raw := h.callWith(h.token(role), "PATCH", path, map[string]any{"display_name": "x"}, ifMatch("*")); st != http.StatusForbidden {
			t.Errorf("%s token changes: %d %s", role, st, raw)
		}
	}
}

// The order of the checks of a change that carries If-Match, as the docs
// give it: the form of the header (400), the instance (404), what is wrong
// with the request whatever the instance is (422), the revision (412), and
// then the change against the instance at that revision (422, 409). For the
// site role the limits on the body (403) come before all of them.
func TestChecksOfAChangeWithIfMatchRunInTheDocumentedOrder(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	site := h.token("site")
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "1.0", fastRules(nil))
	const path = "/api/v1/instances/alpha"
	var v instances.View
	h.must(http.StatusOK, "GET", path, nil, &v)
	stale := ifMatch(fmt.Sprintf("%q", fmt.Sprint(v.Revision+5)))
	current := ifMatch(fmt.Sprintf("%q", fmt.Sprint(v.Revision)))

	type call struct {
		what   string
		token  string
		method string
		path   string
		body   any
		lines  http.Header
		status int
		code   string
	}
	for _, c := range []call{
		// The form of the header comes before the instance and the request.
		{"a malformed If-Match, an instance that is not there", h.admin, "PATCH", "/api/v1/instances/ghost", map[string]any{"display_name": "x"}, ifMatch("7"), 400, "bad_if_match"},
		{"a malformed If-Match, a build", h.admin, "PATCH", path, map[string]any{"build": "1.0"}, ifMatch("7"), 400, "bad_if_match"},
		{"a malformed If-Match, a short password", h.admin, "PUT", path + "/secrets", map[string]any{"server_password": "short"}, ifMatch("7"), 400, "bad_if_match"},
		// The instance before the revision.
		{"a stale If-Match, an instance that is not there", h.admin, "PATCH", "/api/v1/instances/ghost", map[string]any{"display_name": "x"}, stale, 404, "not_found"},
		{"a stale If-Match, secrets of an instance that is not there", h.admin, "PUT", "/api/v1/instances/ghost/secrets", map[string]any{"server_password": "join me 7"}, stale, 404, "not_found"},
		// What is wrong whatever the instance is, before the revision, for any revision.
		{"a stale If-Match, a build", h.admin, "PATCH", path, map[string]any{"build": "1.0"}, stale, 422, "invalid"},
		{"the current If-Match, a build", h.admin, "PATCH", path, map[string]any{"build": "1.0"}, current, 422, "invalid"},
		{"a stale If-Match, a short password", h.admin, "PUT", path + "/secrets", map[string]any{"server_password": "short"}, stale, 422, "password_too_short"},
		{"the current If-Match, a short password", h.admin, "PUT", path + "/secrets", map[string]any{"server_password": "short"}, current, 422, "password_too_short"},
		{"a stale If-Match, a token with a space", h.admin, "PUT", path + "/secrets", map[string]any{"announcements_token": "has a space"}, stale, 422, "invalid"},
		// The revision before what depends on the instance.
		{"a stale If-Match, zero players", h.admin, "PATCH", path, map[string]any{"max_players": 0}, stale, 412, "changed"},
		{"the current If-Match, zero players", h.admin, "PATCH", path, map[string]any{"max_players": 0}, current, 422, "invalid"},
		{"a stale If-Match, a rule that is a switch of its own", h.admin, "PATCH", path, map[string]any{"rules": map[string]string{"MULTIHOME": "x"}}, stale, 412, "changed"},
		{"the current If-Match, a rule that is a switch of its own", h.admin, "PATCH", path, map[string]any{"rules": map[string]string{"MULTIHOME": "x"}}, current, 422, "invalid"},
		// The site role: its limits on the body come first.
		{"site, a stale If-Match, a build", site, "PATCH", path, map[string]any{"build": "1.0"}, stale, 403, "forbidden"},
		{"site, a malformed If-Match, a build", site, "PATCH", path, map[string]any{"build": "1.0"}, ifMatch("7"), 403, "forbidden"},
		{"site, a stale If-Match, a field it may change", site, "PATCH", path, map[string]any{"display_name": "x"}, stale, 412, "changed"},
		{"site, a malformed If-Match, a field it may change", site, "PATCH", path, map[string]any{"display_name": "x"}, ifMatch("7"), 400, "bad_if_match"},
	} {
		st, _, raw := h.callWith(c.token, c.method, c.path, c.body, c.lines)
		if got := errorOf(t, raw); st != c.status || got.Code != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.what, st, raw, c.status, c.code)
		}
	}
	// Nothing was changed by any of them.
	var after instances.View
	h.must(http.StatusOK, "GET", path, nil, &after)
	if after.Revision != v.Revision || after.DisplayName != v.DisplayName || after.Secrets.ServerPasswordSet {
		t.Fatalf("refused calls changed the instance: revision %d, was %d; %+v", after.Revision, v.Revision, after.Instance)
	}

	// A password that occurs in a setting: refused against the instance, so
	// after the revision.
	h.must(http.StatusOK, "PATCH", path, map[string]any{"display_name": "join me 7"}, nil)
	h.must(http.StatusOK, "GET", path, nil, &v)
	stale = ifMatch(fmt.Sprintf("%q", fmt.Sprint(v.Revision-1)))
	current = ifMatch(fmt.Sprintf("%q", fmt.Sprint(v.Revision)))
	if st, _, raw := h.callWith(h.admin, "PUT", path+"/secrets", map[string]any{"server_password": "join me 7"}, stale); st != http.StatusPreconditionFailed || errorOf(t, raw).Code != "changed" {
		t.Errorf("a password in a setting, a stale If-Match: %d %s", st, raw)
	}
	if st, _, raw := h.callWith(h.admin, "PUT", path+"/secrets", map[string]any{"server_password": "join me 7"}, current); st != http.StatusUnprocessableEntity || errorOf(t, raw).Code != "password_in_settings" {
		t.Errorf("a password in a setting, the current If-Match: %d %s", st, raw)
	}
}
