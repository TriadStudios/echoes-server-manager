package e2e

import (
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// A build is a program that runs with ESM's own rights, so only an admin
// installs one; an operator still updates an instance to a build an admin
// installed. A local source must be inside the import folder.
func TestOnlyAdminsInstallBuilds(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	op := h.token("operator")
	dir := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "1.0"})
	var eb api.ErrorBody
	if st := h.call("POST", "/api/v1/builds", op, api.InstallRequest{Source: dir}, &eb); st != http.StatusForbidden {
		t.Fatalf("operator installs a build: %d %+v", st, eb)
	}
	h.must(http.StatusCreated, "POST", "/api/v1/builds", api.InstallRequest{Source: dir}, nil)
	h.install(testkit.BuildOptions{Version: "1.1"})
	h.create("alpha", "1.0", fastRules(nil))
	var st struct {
		State string `json:"state"`
		Build string `json:"build"`
	}
	if code := h.call("POST", "/api/v1/instances/alpha/update", op, api.UpdateRequest{To: "1.1"}, &st); code != http.StatusOK {
		t.Fatalf("operator update to an installed build: %d %+v", code, st)
	}
	outside := testkit.MakeBuild(t, filepath.Join(t.TempDir(), "x"), testkit.BuildOptions{Version: "2.0"})
	if code := h.call("POST", "/api/v1/builds", h.admin, api.InstallRequest{Source: outside}, &eb); code != http.StatusUnprocessableEntity || eb.Error.Code != "source_not_allowed" {
		t.Fatalf("a source outside the import folder: %d %+v", code, eb)
	}
}

// The service answers on TCP only for its own host names: a page of
// another site whose name now points at 127.0.0.1 gets 421, page and API
// alike. A name in public_hosts is ESM's own; the socket is not checked.
func TestOnlyOwnHostNames(t *testing.T) {
	t.Parallel()
	h := newESM(t, func(cfg map[string]any) { cfg["public_hosts"] = []string{"esm.example.test"} })
	_, port, _ := net.SplitHostPort(h.svc.Addr())
	for host, want := range map[string]int{
		"rebind.example:" + port: http.StatusMisdirectedRequest,
		"esm.example.test":       http.StatusOK,
		"localhost:" + port:      http.StatusOK,
	} {
		for _, path := range []string{"/api/v1/setup", "/"} {
			req, _ := http.NewRequest(http.MethodGet, h.base+path, nil)
			req.Host = host
			resp, err := h.http.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != want {
				t.Errorf("%s with Host %q: %d, want %d", path, host, resp.StatusCode, want)
			}
		}
	}
	if st := h.callSock("GET", "/api/v1/setup", nil, nil); st != http.StatusOK {
		t.Fatalf("socket: %d", st)
	}
}
