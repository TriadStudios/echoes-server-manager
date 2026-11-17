package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// callRaw sends body as it is, as JSON, with a bearer token, and returns the
// status and the code of an error answer.
func (h *harness) callRaw(token, method, path, body string) (int, string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var eb api.ErrorBody
	json.Unmarshal(b, &eb)
	return resp.StatusCode, eb.Error.Code
}

// A body is exactly one JSON value, whoever sends it and to whichever call:
// a closing brace or bracket after the object, which the decoder once did
// not see, and a second value are refused with 400 and nothing is changed.
// Before, `{"extra_args":[...],"bind_ip":"..."}}` was taken by the change
// call for the object it holds, past the check of what a site token may
// change.
func TestDataAfterTheBodyIsRefused(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	site := h.token("site")
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "1.0", fastRules(nil))
	const path = "/api/v1/instances/alpha"
	raw := func() string {
		var b json.RawMessage
		h.must(http.StatusOK, "GET", path, nil, &b)
		return string(b)
	}
	before := raw()
	tokens := func() int {
		var tl api.TokenList
		h.must(http.StatusOK, "GET", "/api/v1/tokens", nil, &tl)
		return len(tl.Tokens)
	}
	nTokens := tokens()

	bodies := []string{
		`{"extra_args":["-Verbose"],"bind_ip":"127.0.0.1"}}`,
		`{"limits":{"memory_max":"4G"}}]`,
		`{"limits":{"memory_max":"4G"}} ]`,
		`{"display_name":"Trailing"} }`,
		`{"display_name":"Trailing"}]`,
		`{"display_name":"Trailing"}x`,
		`{"display_name":"Trailing"}{"extra_args":["-Verbose"]}`,
		`{"display_name":"Trailing"} {"bind_ip":"127.0.0.1"}`,
		`{"display_name":"Trailing"}` + "\n" + `{"build":"1.0"}`,
		`{"display_name":"Trailing"},`,
	}
	for name, token := range map[string]string{"site": site, "admin": h.admin} {
		for _, body := range bodies {
			if st, code := h.callRaw(token, "PATCH", path, body); st != http.StatusBadRequest || code != "bad_request" {
				t.Errorf("%s token, PATCH %s: %d %q, want 400", name, body, st, code)
			}
		}
		// Calls other than the change call take one value too.
		for _, c := range []struct{ method, path, body string }{
			{"POST", "/api/v1/instances/alpha/update", `{"to":"1.0"}}`},
			{"POST", "/api/v1/instances/alpha/restore", `{"backup":"b"}]`},
			{"POST", "/api/v1/builds", `{"source":"steam:ghost"}}`},
			{"POST", "/api/v1/builds/prune", `{}}`},
			{"POST", "/api/v1/box/release-check", `{} {}`},
		} {
			if st, code := h.callRaw(token, c.method, c.path, c.body); st != http.StatusBadRequest || code != "bad_request" {
				t.Errorf("%s token, %s %s %s: %d %q, want 400", name, c.method, c.path, c.body, st, code)
			}
		}
	}
	if st, code := h.callRaw(h.admin, "POST", "/api/v1/tokens", `{"name":"extra","role":"admin"}}`); st != http.StatusBadRequest || code != "bad_request" {
		t.Errorf("admin token, POST /tokens with a brace after it: %d %q", st, code)
	}
	if st, code := h.callRaw(h.admin, "POST", "/api/v1/tokens", `{"name":"extra","role":"read"} {"name":"more","role":"admin"}`); st != http.StatusBadRequest || code != "bad_request" {
		t.Errorf("admin token, POST /tokens with two objects: %d %q", st, code)
	}
	if after := raw(); after != before {
		t.Fatalf("a refused call changed the instance:\n%s\n%s", before, after)
	}
	if n := tokens(); n != nTokens {
		t.Fatalf("a refused call made a token: %d tokens, were %d", n, nTokens)
	}

	// A body that is exactly one value, white space around it or not, is
	// taken as ever.
	for _, body := range []string{`{"display_name":"Fine"}`, "{\"display_name\":\"Fine\"}\n", " \t{\"display_name\":\"Fine\"} \r\n\n"} {
		if st, code := h.callRaw(site, "PATCH", path, body); st != http.StatusOK {
			t.Errorf("site token, PATCH %q: %d %q", body, st, code)
		}
	}
	if st, code := h.callRaw(h.admin, "PATCH", path, `{"extra_args":["-Verbose"]}`); st != http.StatusOK {
		t.Errorf("admin token, PATCH extra_args: %d %q", st, code)
	}
}
