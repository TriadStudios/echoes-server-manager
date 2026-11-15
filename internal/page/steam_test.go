package page_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// A Steam source is on the Builds screen as "esm steam list" shows it: its
// name, branch, channel, last check and newest build id, and for an admin
// the same install that "esm build install steam:<name>" makes. Once a
// newer build of the source is installed, a server on the older one has an
// update available, which the Dashboard and its Overview say, with how many
// builds it is behind.
func TestSteamSourcesAndUpdates(t *testing.T) {
	steamDir := t.TempDir()
	content := filepath.Join(t.TempDir(), "content")
	exes := testkit.MakeContent(t, content, testkit.BuildOptions{Version: "x"})
	ctl := testkit.SteamControl{AppID: "1000001", BuildID: "100", Content: content}
	exe := testkit.FakeSteamCMD(t, steamDir, ctl)
	s := newSiteConfig(t, map[string]any{
		"steamcmd": exe,
		"steam_sources": []map[string]any{{"name": "main", "app_id": "1000001", "executable": exes,
			"default_map": testkit.DefaultMap, "channel": "live", "check_every": "0"}},
	})
	s.setup()
	oper := s.token("operator")

	// Nothing installed yet: the source is there, never checked.
	body := s.checkPage("/builds", "session", s.csrf)
	for _, want := range []string{`<th scope="row" class="mono">main</th>`, "default branch", `<span class="dim">never</span>`, "none yet",
		`<input type="hidden" name="source" value="steam:main">`, "checks are off", "Sources are set under steam_sources in esm.json"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Steam source of a new box lacks %q", want)
		}
	}
	if strings.Contains(body, "Not available yet") || strings.Contains(body, "No Steam source is set") {
		t.Error("the Steam tab says the sources are not usable")
	}
	if body = s.checkPage("/builds", oper, ""); strings.Contains(body, `value="steam:main"`) || !strings.Contains(body, `<th scope="row" class="mono">main</th>`) {
		t.Error("an operator sees the source but gets no button to install from it")
	}

	// A first build, a server on it, then a newer build and another.
	var b api.BuildView
	s.must(http.StatusCreated, "POST", "/api/v1/builds", s.admin, api.InstallRequest{Source: "steam:main"}, &b)
	if b.Version != "steam-main-100" {
		t.Fatalf("first build: %+v", b)
	}
	s.must(http.StatusCreated, "POST", "/api/v1/instances", s.admin, map[string]any{"name": "alpha", "bind_ip": "127.0.0.1", "build": b.Version}, nil)
	for _, p := range []string{"/", "/servers/alpha"} {
		if body = s.checkPage(p, "session", s.csrf); !strings.Contains(body, "Up to date</span>") || strings.Contains(body, "Update available") {
			t.Errorf("%s: a server on the newest build is up to date", p)
		}
	}
	for _, id := range []string{"101", "102"} {
		ctl.BuildID = id
		testkit.SetSteamControl(t, steamDir, ctl)
		s.must(http.StatusCreated, "POST", "/api/v1/builds", s.admin, api.InstallRequest{Source: "steam:main"}, &b)
	}

	body = s.checkPage("/servers/alpha", "session", s.csrf)
	for _, want := range []string{`<span class="pill avail"><span class="dot" aria-hidden="true"></span>Update available</span>`,
		`<span class="mono">steam-main-100</span><span class="arrow" aria-hidden="true">&rarr;</span><span class="sr">to</span><span class="mono newest">steam-main-102</span>`,
		"2 builds behind.", "auto_update is off: update by hand.",
		`<option value="steam-main-102" selected title="Commit steam app 1000001 build 102">steam-main-102 (live, newest)</option>`,
		`<option value="steam-main-100" disabled title="Commit steam app 1000001 build 100">steam-main-100 (live, running now)</option>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview of a server two builds behind lacks %q", want)
		}
	}
	if body = s.checkPage("/", "session", s.csrf); !strings.Contains(body, "Update available</span><span class=\"mono small\">steam-main-102</span>") {
		t.Error("the dashboard does not say that an update is available")
	}
	rd := s.token("read")
	if body = s.checkPage("/servers/alpha", rd, ""); !strings.Contains(body, "Update available") || !strings.Contains(body, "2 builds behind.") || strings.Contains(body, `data-form="update"`) {
		t.Error("read sees the update and how far behind, but gets no form")
	}

	// The source now shows its newest build id and when it last looked, and
	// the builds are newest first, with the one the server runs and the ones
	// nothing uses said plainly, and what removing those frees.
	body = s.checkPage("/builds", "session", s.csrf)
	for _, want := range []string{`<td class="mono nowrap">102</td>`, "Running on", "Unused", "(frees ", `data-confirm="Remove the 2 builds that no server runs or keeps for a rollback? This frees `} {
		if !strings.Contains(body, want) {
			t.Errorf("the builds of a Steam source lack %q", want)
		}
	}
	if strings.Contains(body, `<span class="dim">never</span>`) {
		t.Error("the source was checked, but the page says never")
	}
	if i, j, k := strings.Index(body, ">steam-main-102<"), strings.Index(body, ">steam-main-101<"), strings.Index(body, ">steam-main-100<"); i < 0 || i > j || j > k {
		t.Errorf("builds are not newest first: %d %d %d", i, j, k)
	}
	if strings.Count(body, ">Newest</span>") != 1 {
		t.Errorf("one Newest badge for the one channel: %d", strings.Count(body, ">Newest</span>"))
	}
}
