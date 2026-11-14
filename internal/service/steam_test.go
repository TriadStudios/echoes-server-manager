package service

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// steamRig is a service with two Steam sources, main and other, served by
// a fake SteamCMD, and a build not from Steam (1.0.0) installed before it
// started.
type steamRig struct {
	t        *testing.T
	s        *Service
	steamDir string
	ctl      testkit.SteamControl
}

func newSteamRig(t *testing.T, keep int) *steamRig {
	t.Helper()
	root := shortRoot(t)
	r := &steamRig{t: t, steamDir: t.TempDir()}
	content := filepath.Join(t.TempDir(), "content")
	exes := testkit.MakeContent(t, content, testkit.BuildOptions{Version: "x"})
	r.ctl = testkit.SteamControl{AppID: "1000001", BuildID: "1", Content: content}
	exe := testkit.FakeSteamCMD(t, r.steamDir, r.ctl)
	source := func(name string) map[string]any {
		return map[string]any{"name": name, "app_id": "1000001", "executable": exes, "default_map": testkit.DefaultMap,
			"channel": "live", "check_every": "0"}
	}
	writeConfig(t, root, map[string]any{"steamcmd": exe, "keep_steam_builds": keep,
		"steam_sources": []map[string]any{source("main"), source("other")}})

	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	imports := t.TempDir()
	dir := testkit.MakeBuild(t, imports, testkit.BuildOptions{Version: "1.0.0"})
	if _, err := builds.New(st, builds.Options{ImportDir: imports}).Install(context.Background(), dir, ""); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if r.s, err = Start(Options{Root: root, Version: "test", Stderr: &syncBuffer{}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, r.s) })
	return r
}

// install makes the source's branch serve build id and installs it.
func (r *steamRig) install(source, id string) {
	r.t.Helper()
	r.ctl.BuildID = id
	testkit.SetSteamControl(r.t, r.steamDir, r.ctl)
	b, installed, err := r.s.steam.Install(context.Background(), source, false)
	if err != nil || !installed || b.Version != "steam-"+source+"-"+id {
		r.t.Fatalf("install of %s %s: %+v %v %v", source, id, b, installed, err)
	}
}

// installed lists the installed versions, sorted.
func (r *steamRig) installed() []string {
	r.t.Helper()
	list, err := builds.New(r.s.st, builds.Options{}).List()
	if err != nil {
		r.t.Fatal(err)
	}
	out := []string{}
	for _, b := range list {
		out = append(out, b.Version)
	}
	slices.Sort(out)
	return out
}

func (r *steamRig) want(versions ...string) {
	r.t.Helper()
	slices.Sort(versions)
	if got := r.installed(); !slices.Equal(got, versions) {
		r.t.Fatalf("builds %v, want %v", got, versions)
	}
}

// After each install from a Steam source, the source's builds beyond the
// newest keep_steam_builds by build id (as numbers, not by install time)
// are removed. Builds of other sources and builds not from Steam stay.
func TestSteamInstallKeepsTheNewestBuildsOfTheSource(t *testing.T) {
	r := newSteamRig(t, 3)
	r.install("other", "7")
	for _, id := range []string{"100", "98", "102", "99", "101"} {
		r.install("main", id)
	}
	r.want("1.0.0", "steam-other-7", "steam-main-100", "steam-main-101", "steam-main-102")

	var al []string
	entries, err := r.s.audit.Recent(100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Action == "build.prune" && e.Actor.Kind == "system" && e.OK {
			al = append(al, e.Target)
		}
	}
	if len(al) != 2 || !strings.Contains(strings.Join(al, " "), "steam-main-98") || !strings.Contains(strings.Join(al, " "), "steam-main-99") {
		t.Fatalf("audit entries of the removals: %v", al)
	}
}

// A build an instance uses stays, however old it is.
func TestSteamInstallKeepsBuildsInUse(t *testing.T) {
	r := newSteamRig(t, 3)
	r.install("main", "100")
	bind := "127.0.0.1"
	if _, err := r.s.inst.Create(instances.CreateRequest{Name: "old", Build: "steam-main-100",
		Settings: instances.Settings{BindIP: &bind}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"101", "102", "103", "104"} {
		r.install("main", id)
	}
	r.want("1.0.0", "steam-main-100", "steam-main-102", "steam-main-103", "steam-main-104")
}
