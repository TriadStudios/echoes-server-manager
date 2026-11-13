package e2e

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/release"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

const steamPassword = "Branch-Pw1"

// A Steam source with a fake SteamCMD: the first automatic check installs
// the branch's build, a manual install of the same build id changes
// nothing, a new build id is installed, an instance set to auto_update
// restart waits while players are on and updates once the server is
// empty, a broken new build is rolled back and not tried again, and a
// wrong password is refused. The branch password never reaches a command
// line or any file but the secrets file.
func TestSteamSourceAndAutoUpdate(t *testing.T) {
	steamDir := t.TempDir()
	content := filepath.Join(t.TempDir(), "content")
	exes := testkit.MakeContent(t, content, testkit.BuildOptions{Version: "x"})
	broken := filepath.Join(t.TempDir(), "broken")
	testkit.MakeContent(t, broken, testkit.BuildOptions{Version: "x", Fake: map[string]any{"FakeFailStart": true}})
	record := filepath.Join(steamDir, "record.jsonl")
	ctl := testkit.SteamControl{AppID: "1000001", BuildID: "100", Content: content, Branch: "testing", Password: steamPassword, Record: record}
	exe := testkit.FakeSteamCMD(t, steamDir, ctl)
	tune := func(o *service.Options) {
		o.SchedulerInterval = 100 * time.Millisecond
		o.SteamTick = 100 * time.Millisecond
		// The password is set before the service starts, so its first
		// automatic check can install.
		os.MkdirAll(filepath.Join(o.Root, "secrets"), 0o700)
		os.WriteFile(filepath.Join(o.Root, "secrets", "steam.json"), []byte(`{"passwords":{"main":"`+steamPassword+`"}}`), 0o600)
	}
	h := newESMTuned(t, "process", tune, func(cfg map[string]any) {
		cfg["steamcmd"] = exe
		cfg["steam_sources"] = []map[string]any{{"name": "main", "app_id": "1000001", "branch": "testing", "executable": exes,
			"default_map": testkit.DefaultMap, "channel": "live", "check_every": "1h"}}
	})

	// The first check runs at start and installs the build.
	var sl api.SteamList
	testkit.Eventually(t, 20*time.Second, func() bool {
		h.must(http.StatusOK, "GET", "/api/v1/steam", nil, &sl)
		return len(sl.Sources) == 1 && !sl.Sources[0].Running && !sl.Sources[0].Checked.IsZero()
	}, "the automatic check did not run: %+v", sl)
	if s := sl.Sources[0]; !s.PasswordSet || s.Version != "steam-main-100" || s.BuildID != "100" || s.Error != "" {
		t.Fatalf("source: %+v", s)
	}
	var bl api.BuildList
	h.must(http.StatusOK, "GET", "/api/v1/builds", nil, &bl)
	if len(bl.Builds) != 1 || bl.Builds[0].Version != "steam-main-100" {
		t.Fatalf("builds: %+v", bl)
	}
	var b api.BuildView
	h.must(http.StatusOK, "POST", "/api/v1/builds", api.InstallRequest{Source: "steam:main"}, &b)
	if b.Version != "steam-main-100" {
		t.Fatalf("same build id: %+v", b)
	}

	// An instance on that build, set to update itself.
	v := h.create("auto", "steam-main-100", nil)
	h.must(http.StatusOK, "PATCH", "/api/v1/instances/auto", map[string]any{"auto_update": "restart"}, nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/auto/start?wait=true", struct{}{}, nil)
	data := h.dataDir(v.Name)
	if err := testkit.SetPlayers(data, 2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	ctl.BuildID = "101"
	testkit.SetSteamControl(t, steamDir, ctl)
	h.must(http.StatusCreated, "POST", "/api/v1/builds", api.InstallRequest{Source: "steam:main"}, &b)
	if b.Version != "steam-main-101" {
		t.Fatalf("new build id: %+v", b)
	}
	var iv instances.View
	testkit.Eventually(t, 10*time.Second, func() bool {
		h.must(http.StatusOK, "GET", "/api/v1/instances/auto", nil, &iv)
		return iv.UpdateAvailable && iv.Update.Pending && strings.Contains(iv.Update.Waiting, "2 players")
	}, "the update does not wait for the players: %+v", iv.Update)
	time.Sleep(500 * time.Millisecond)
	if st := h.status("auto"); st.Build != "steam-main-100" || st.State != instances.Running {
		t.Fatalf("updated with players on: %+v", st)
	}

	// The server empties: the update runs.
	if err := testkit.SetPlayers(data, 0); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 30*time.Second, func() bool {
		h.must(http.StatusOK, "GET", "/api/v1/instances/auto", nil, &iv)
		return iv.Build == "steam-main-101" && iv.Status.State == instances.Running && !iv.UpdateAvailable
	}, "the instance was not updated: %+v %+v", iv.Status, iv.Update)
	if testkit.SecondSignal(data) {
		t.Fatal("the update stopped the server with a second signal")
	}

	// A broken build: rolled back, and not tried again.
	ctl.BuildID, ctl.Content = "102", broken
	testkit.SetSteamControl(t, steamDir, ctl)
	h.must(http.StatusCreated, "POST", "/api/v1/builds", api.InstallRequest{Source: "steam:main"}, &b)
	testkit.Eventually(t, 30*time.Second, func() bool {
		h.must(http.StatusOK, "GET", "/api/v1/instances/auto", nil, &iv)
		return iv.Update.LastError != "" && iv.Status.State == instances.Running
	}, "no failed automatic update: %+v %+v", iv.Status, iv.Update)
	if iv.Build != "steam-main-101" || iv.Update.Pending || !strings.Contains(iv.Update.Waiting, "not tried again") {
		t.Fatalf("after the failed update: build %s, %+v", iv.Build, iv.Update)
	}

	// The audit log has the automatic steps.
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=200", nil, &al)
	var okUpdates, failedUpdates, steamInstalls int
	for _, e := range al.Entries {
		switch {
		case e.Action == "instance.auto_update" && e.Actor.Kind == "system" && e.OK:
			okUpdates++
		case e.Action == "instance.auto_update" && e.Actor.Kind == "system":
			failedUpdates++
		case e.Action == "steam.install" && e.Actor.Kind == "system":
			steamInstalls++
		}
	}
	if okUpdates != 1 || failedUpdates != 1 || steamInstalls != 1 {
		t.Fatalf("audit: %d ok updates, %d failed, %d automatic installs: %+v", okUpdates, failedUpdates, steamInstalls, al.Entries)
	}

	// A wrong password is refused, and never repeated.
	var pr api.SteamPasswordResult
	h.must(http.StatusOK, "PUT", "/api/v1/steam/main/password", api.SteamPasswordRequest{Password: "Wrong-Pw2"}, &pr)
	if !pr.PasswordSet {
		t.Fatalf("password result %+v", pr)
	}
	var eb api.ErrorBody
	if st := h.call("POST", "/api/v1/builds", h.admin, api.InstallRequest{Source: "steam:main"}, &eb); st != http.StatusUnprocessableEntity ||
		eb.Error.Code != "wrong_password" || strings.Contains(eb.Error.Message, "Wrong-Pw2") {
		t.Fatalf("wrong password: %d %+v", st, eb)
	}

	runs, leaked := testkit.SteamRecords(t, record)
	if leaked || len(runs) < 4 {
		t.Fatalf("%d SteamCMD runs, password on a command line: %v", len(runs), leaked)
	}
	filepath.WalkDir(h.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(p) == "steam.json" {
			return nil
		}
		if b, _ := os.ReadFile(p); strings.Contains(string(b), steamPassword) || strings.Contains(string(b), "Wrong-Pw2") {
			t.Errorf("%s holds a branch password", p)
		}
		return nil
	})
}

// Only admins set Steam passwords and update ESM itself; the read role
// sees the sources but never a password.
func TestSteamAndSelfUpdateRoles(t *testing.T) {
	h := newESM(t, func(cfg map[string]any) {
		cfg["steam_sources"] = []map[string]any{{"name": "main", "app_id": "1000001", "branch": "testing",
			"executable": map[string]string{"linux": "a/b"}, "channel": "live", "check_every": "0"}}
	})
	var op, rd api.NewToken
	h.must(http.StatusCreated, "POST", "/api/v1/tokens", api.TokenRequest{Name: "op", Role: "operator"}, &op)
	h.must(http.StatusCreated, "POST", "/api/v1/tokens", api.TokenRequest{Name: "rd", Role: "read"}, &rd)
	if st := h.call("PUT", "/api/v1/steam/main/password", op.Token, api.SteamPasswordRequest{Password: "Op-Pw1234"}, nil); st != http.StatusForbidden {
		t.Fatalf("operator sets a Steam password: %d", st)
	}
	if st := h.call("POST", "/api/v1/box/self-update", op.Token, struct{}{}, nil); st != http.StatusForbidden {
		t.Fatalf("operator self-update: %d", st)
	}
	if st := h.call("POST", "/api/v1/builds", op.Token, api.InstallRequest{Source: "steam:main"}, nil); st != http.StatusForbidden {
		t.Fatalf("operator Steam install: %d", st)
	}
	h.must(http.StatusOK, "PUT", "/api/v1/steam/main/password", api.SteamPasswordRequest{Password: "Admin-Pw1"}, nil)
	var raw json.RawMessage
	if st := h.call("GET", "/api/v1/steam", rd.Token, nil, &raw); st != http.StatusOK || strings.Contains(string(raw), "Admin-Pw1") ||
		!strings.Contains(string(raw), `"password_set":true`) {
		t.Fatalf("read role: %d %s", st, raw)
	}
	var eb api.ErrorBody
	if st := h.call("PUT", "/api/v1/steam/nope/password", h.admin, api.SteamPasswordRequest{Password: "Admin-Pw1"}, &eb); st != http.StatusNotFound {
		t.Fatalf("unknown source: %d %+v", st, eb)
	}
	// In tests no release is looked up unless the test names a server.
	if st := h.call("POST", "/api/v1/box/self-update", h.admin, struct{}{}, &eb); st != http.StatusUnprocessableEntity {
		t.Fatalf("self-update in tests: %d %+v", st, eb)
	}
	var box api.Box
	h.must(http.StatusOK, "GET", "/api/v1/box", nil, &box)
	if box.UpdateAvailable || box.Release.Enabled || box.Release.Note == "" {
		t.Fatalf("release status in tests: %+v", box.Release)
	}
}

// A self-update through the API takes a signed release from a release
// server, replaces the binary, keeps the old one, and asks the service to
// end so its service manager starts the new one.
func TestSelfUpdateThroughTheAPI(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	name := release.AssetName(runtime.GOOS, runtime.GOARCH)
	bin := []byte("the new esm")
	sum := sha256.Sum256(bin)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			json.NewEncoder(w).Encode(release.Release{Tag: "v9.9.9", URL: srv.URL, Assets: []release.Asset{
				{Name: name, URL: srv.URL + "/bin"}, {Name: release.SumsName, URL: srv.URL + "/sums"}, {Name: release.SigName, URL: srv.URL + "/sig"}}})
		case "/bin":
			w.Write(bin)
		case "/sums":
			w.Write([]byte(sums))
		case "/sig":
			w.Write(release.Sign(priv, []byte(sums)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "esm-under-test")
	os.WriteFile(exe, []byte("the old esm"), 0o755)
	h := newESMTuned(t, "process", func(o *service.Options) {
		o.ReleaseAPI, o.ReleaseHTTP, o.ReleaseKey, o.Executable = srv.URL+"/latest", srv.Client(), pub, exe
	})
	var eb api.ErrorBody
	if st := h.call("POST", "/api/v1/box/self-update", h.admin, struct{}{}, &eb); st != http.StatusConflict || eb.Error.Code != "unknown_version" {
		t.Fatalf("a development build without force: %d %+v", st, eb)
	}
	var res api.SelfUpdateResult
	h.must(http.StatusOK, "POST", "/api/v1/box/self-update", api.SelfUpdateRequest{Force: true}, &res)
	if res.To != "v9.9.9" || !res.Restarting || res.Previous != exe+".previous" {
		t.Fatalf("result %+v", res)
	}
	if b, _ := os.ReadFile(exe); string(b) != "the new esm" {
		t.Fatalf("binary %q", b)
	}
	if b, _ := os.ReadFile(exe + ".previous"); string(b) != "the old esm" {
		t.Fatalf("previous %q", b)
	}
	select {
	case <-h.svc.Exit():
	case <-time.After(5 * time.Second):
		t.Fatal("the service was not asked to end after the update")
	}
	var box api.Box
	testkit.Eventually(t, 5*time.Second, func() bool {
		h.must(http.StatusOK, "GET", "/api/v1/box", nil, &box)
		return box.Release.Latest == "v9.9.9"
	}, "the release check: %+v", box.Release)
}
