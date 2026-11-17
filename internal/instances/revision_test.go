package instances

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// storedRevision is the revision the instance's revision file holds, 1 when
// there is none, as ESM reads it. It also checks that instance.json has no
// revision, and that the file is a decimal number and a line break: what
// ESM writes.
func (r *rig) storedRevision(name string) int64 {
	r.t.Helper()
	r.checkNoRevisionInSettings(name)
	b, err := r.st.ReadFile(revisionFile(name))
	if errors.Is(err, fs.ErrNotExist) {
		return 1
	}
	if err != nil {
		r.t.Fatal(err)
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(string(b), "\n"), 10, 64)
	if err != nil || n < 2 || string(b) != fmt.Sprintf("%d\n", n) {
		r.t.Fatalf("the revision file of %s holds %q", name, b)
	}
	return n
}

// checkNoRevisionInSettings fails if instance.json has a revision: the file
// is what the release before revisions reads (see releasedInstance).
func (r *rig) checkNoRevisionInSettings(name string) {
	r.t.Helper()
	b, err := r.st.ReadFile(instFile(name))
	if err != nil {
		r.t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		r.t.Fatal(err)
	}
	if _, ok := keys["revision"]; ok {
		r.t.Fatalf("instance.json of %s has a revision: %s", name, b)
	}
}

// revisionOf is the revision the instance shows, in a list and in a
// single reading alike.
func (r *rig) revisionOf(name string) int64 {
	r.t.Helper()
	v, err := r.m.Get(name)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, l := range r.m.List() {
		if l.Name == name && l.Revision != v.Revision {
			r.t.Fatalf("the list shows revision %d, a reading %d", l.Revision, v.Revision)
		}
	}
	return v.Revision
}

// An instance is made at revision 1, and every stored change of its
// settings adds 1: a change (also one that changes nothing), a change of
// secrets, and the switch of its build by an update. The revision is in a
// file of its own beside instance.json (a new instance has none: it is at
// 1), so it is the same after ESM starts again; a request that is refused,
// or that only reads, leaves it as it is.
func TestRevisionGrowsWithEveryStoredChange(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2"})
	v, err := r.m.Create(CreateRequest{Name: "alpha", Build: "steam-main-1", Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(nil)}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != 1 || r.storedRevision("alpha") != 1 || r.st.Exists(revisionFile("alpha")) {
		t.Fatalf("a new instance is at revision %d (stored %d, a revision file: %v), want 1 and no file", v.Revision, r.storedRevision("alpha"), r.st.Exists(revisionFile("alpha")))
	}
	want := int64(1)
	step := func(what string) {
		t.Helper()
		want++
		if got := r.revisionOf("alpha"); got != want || r.storedRevision("alpha") != want {
			t.Fatalf("after %s: revision %d (stored %d), want %d", what, got, r.storedRevision("alpha"), want)
		}
	}

	pv, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha")}})
	if err != nil || pv.Revision != 2 {
		t.Fatalf("a change: %v, the view it returns is at revision %d, want 2", err, pv.Revision)
	}
	step("a change")
	if _, _, err := r.m.Patch("alpha", PatchRequest{}); err != nil {
		t.Fatal(err)
	}
	step("a change that sets nothing")

	flags, rev, _, err := r.m.SetSecretsIf("alpha", SecretsRequest{ServerPassword: ptr("join me 7")}, Condition{})
	if err != nil || !flags.ServerPasswordSet || rev != 4 {
		t.Fatalf("secrets: %v, %+v, revision %d, want 4", err, flags, rev)
	}
	step("a change of secrets")
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{AdminPassword: ptr("admin 8 chars")}); err != nil {
		t.Fatal(err)
	}
	step("a change of secrets without a condition")

	// A refused change and a read change nothing.
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{MaxPlayers: ptr(0)}}); !fault.Is(err, fault.Invalid) {
		t.Fatalf("an invalid change: %v", err)
	}
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr("short")}); err == nil {
		t.Fatal("a short password was taken")
	}
	r.m.List()
	if got := r.revisionOf("alpha"); got != want || r.storedRevision("alpha") != want {
		t.Fatalf("after refused changes and reads: revision %d (stored %d), want %d", got, r.storedRevision("alpha"), want)
	}

	// An update switches the build (a stopped instance is started on the
	// new build to see it ready, and stopped again).
	if _, err := r.m.Update(context.Background(), "alpha", "steam-main-2"); err != nil {
		t.Fatal(err)
	}
	if got := r.revisionOf("alpha"); got <= want || r.storedRevision("alpha") != got {
		t.Fatalf("after an update: revision %d (stored %d), was %d", got, r.storedRevision("alpha"), want)
	}
	want = r.revisionOf("alpha")

	// Another start of ESM reads the same revision.
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	if v, err := m.Get("alpha"); err != nil || v.Revision != want {
		t.Fatalf("after a restart of ESM: %v, revision %d, want %d", err, v.Revision, want)
	}
	if v, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Cluster: ptr("main")}}); err != nil || v.Revision != want+1 {
		t.Fatalf("a change after a restart of ESM: %v, revision %d, want %d", err, v.Revision, want+1)
	}
}

// An automatic update of a stopped instance stores the new build, so it is
// a change too.
func TestRevisionGrowsWithAnAutomaticUpdate(t *testing.T) {
	r, _ := autoRig(t, 2)
	r.createOn("a", "steam-main-1", nil)
	before := r.revisionOf("a")
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	if v, _ := r.m.Get("a"); v.Build != "steam-main-2" || v.Revision <= before || r.storedRevision("a") != v.Revision {
		t.Fatalf("after the automatic update: build %s, revision %d (stored %d), was %d", v.Build, v.Revision, r.storedRevision("a"), before)
	}
}

// An instance stored before there were revisions has no revision file, and
// is at 1 when it is read; its first change makes it 2 and writes the file.
// A file that holds no revision (nothing ESM writes) is read as 1, with a
// warning, and a number with white space around it as that number.
func TestRevisionOfAnInstanceStoredBefore(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha")}}); err != nil {
		t.Fatal(err)
	}
	if r.storedRevision("alpha") != 2 {
		t.Fatal("the change did not store a revision")
	}
	// What an earlier release left: no revision file.
	if err := r.st.Remove(revisionFile("alpha")); err != nil {
		t.Fatal(err)
	}

	m := r.newManager(&recordSup{})
	defer m.Close()
	v, err := m.Get("alpha")
	if err != nil || v.Revision != 1 {
		t.Fatalf("an instance stored without a revision: %v, revision %d, want 1", err, v.Revision)
	}
	// Read as 1, a change that names 1 is taken, and makes 2.
	v, _, err = m.PatchIf("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha Two")}}, Condition{Revision: 1})
	if err != nil || v.Revision != 2 || r.storedRevision("alpha") != 2 {
		t.Fatalf("the first change: %v, revision %d (stored %d), want 2", err, v.Revision, r.storedRevision("alpha"))
	}
	m.Close()

	for _, c := range []struct {
		file string
		want int64
		warn bool
	}{
		{"7\n", 7, false}, {" 12 \r\n", 12, false}, {"7", 7, false},
		{"", 1, true}, {"\n", 1, true}, {"abc\n", 1, true}, {"0\n", 1, true}, {"-3\n", 1, true}, {"2.5\n", 1, true},
		{"99999999999999999999\n", 1, true},
	} {
		if err := r.st.WriteFile(revisionFile("alpha"), []byte(c.file), 0o600); err != nil {
			t.Fatal(err)
		}
		before := strings.Count(r.log.String(), "does not hold a revision")
		m := r.newManager(&recordSup{})
		v, err := m.Get("alpha")
		m.Close()
		if err != nil || v.Revision != c.want {
			t.Errorf("a revision file holding %q: %v, revision %d, want %d", c.file, err, v.Revision, c.want)
		}
		if warned := strings.Count(r.log.String(), "does not hold a revision") > before; warned != c.warn {
			t.Errorf("a revision file holding %q: warned %v, want %v", c.file, warned, c.warn)
		}
	}
}

// releasedInstance is Instance as the release before revisions has it, field
// for field with the same JSON names (a copy of the struct of that release).
// That release reads instance.json by refusing every field it does not know,
// so anything this release stores in the file that is not one of these is a
// server that the release cannot start again when it is put back.
type releasedInstance struct {
	Name           string            `json:"name"`
	DisplayName    string            `json:"display_name"`
	Cluster        string            `json:"cluster"`
	Channel        string            `json:"channel"`
	Build          string            `json:"build"`
	Map            string            `json:"map"`
	World          string            `json:"world"`
	Ports          Ports             `json:"ports"`
	BindIP         string            `json:"bind_ip"`
	MaxPlayers     int               `json:"max_players"`
	Rules          map[string]string `json:"rules"`
	Listed         bool              `json:"listed"`
	Autostart      bool              `json:"autostart"`
	RestartOnCrash bool              `json:"restart_on_crash"`
	Schedule       Schedule          `json:"schedule"`
	Backup         BackupPolicy      `json:"backup"`
	Limits         Limits            `json:"limits"`
	ExtraArgs      []string          `json:"extra_args"`
	SecretDelivery string            `json:"secret_delivery"`
	AutoUpdate     string            `json:"auto_update"`
	UpdateMaxWait  config.Duration   `json:"update_max_wait"`
	PreviousBuilds []string          `json:"previous_builds"`
	Created        time.Time         `json:"created"`
	Updated        time.Time         `json:"updated"`
}

// readsLikeTheReleaseBefore loads the folder of an instance the way the
// release before revisions did when it started: instance.json and
// secrets.json each into its struct with every unknown field refused
// (store.ReadJSON), state.json too, and the folders of the instances
// listed by name only. It fails on anything that release would not read.
func (r *rig) readsLikeTheReleaseBefore(name string) releasedInstance {
	r.t.Helper()
	var in releasedInstance
	if err := r.st.ReadJSON(instFile(name), &in); err != nil {
		r.t.Fatalf("the release before revisions cannot read instance.json of %s: %v", name, err)
	}
	// The same file, key for key: nothing more and nothing less.
	b, _ := r.st.ReadFile(instFile(name))
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		r.t.Fatal(err)
	}
	want, _ := json.Marshal(releasedInstance{})
	var wantKeys map[string]json.RawMessage
	json.Unmarshal(want, &wantKeys)
	if got, wanted := slices.Sorted(maps.Keys(keys)), slices.Sorted(maps.Keys(wantKeys)); !slices.Equal(got, wanted) {
		r.t.Fatalf("instance.json of %s has the keys %v, the release before revisions has %v", name, got, wanted)
	}
	var sec Secrets
	if err := r.st.ReadJSON(secretsFile(name), &sec); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.t.Fatalf("the release before revisions cannot read secrets.json of %s: %v", name, err)
	}
	var st Status
	if err := r.st.ReadJSON(stateFile(name), &st); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.t.Fatalf("the release before revisions cannot read state.json of %s: %v", name, err)
	}
	return in
}

// Going back to the release before revisions keeps working: whatever this
// release stores for an instance (a change, secrets, an update, an automatic
// update, a start), instance.json stays what that release reads (no field
// it does not know, which it would refuse), the one new file in the
// instance's folder, revision, is a file it never looks at (it reads the
// files it knows by name; its only listing of instances is of the folders
// under instances/), and what it reads has the values this release stored.
func TestInstanceFolderIsReadByTheReleaseBefore(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2"})
	if _, err := r.m.Create(CreateRequest{Name: "alpha", Build: "steam-main-1", Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(nil)}}); err != nil {
		t.Fatal(err)
	}
	if in := r.readsLikeTheReleaseBefore("alpha"); in.Build != "steam-main-1" || in.World != "alpha" {
		t.Fatalf("a new instance read as the release before reads it: %+v", in)
	}

	more := map[string]string{"HarvestRate": "2", "Banner": "set meanwhile"}
	if _, _, err := r.m.PatchIf("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha"), Rules: &more, Cluster: ptr("main"),
		Schedule: &Schedule{RestartAt: []string{"04:00"}}, Backup: &BackupPolicy{Every: config.Duration(time.Hour), Keep: 4},
		Limits: &Limits{MemoryMax: "4G"}, ExtraArgs: &[]string{"-Verbose"}}}, Condition{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	in := r.readsLikeTheReleaseBefore("alpha")
	if in.DisplayName != "Alpha" || in.Rules["HarvestRate"] != "2" || in.Cluster != "main" || in.Backup.Keep != 4 || in.Limits.MemoryMax != "4G" ||
		!slices.Equal(in.ExtraArgs, []string{"-Verbose"}) || !slices.Equal(in.Schedule.RestartAt, []string{"04:00"}) {
		t.Fatalf("after a change, read as the release before reads it: %+v", in)
	}

	if _, _, _, err := r.m.SetSecretsIf("alpha", SecretsRequest{ServerPassword: ptr("join me 7"), AnnouncementsToken: ptr("token-0123456789")}, Condition{Revision: 2}); err != nil {
		t.Fatal(err)
	}
	r.readsLikeTheReleaseBefore("alpha")

	if _, err := r.m.Update(context.Background(), "alpha", "steam-main-2"); err != nil {
		t.Fatal(err)
	}
	if in = r.readsLikeTheReleaseBefore("alpha"); in.Build != "steam-main-2" || !slices.Equal(in.PreviousBuilds, []string{"steam-main-1"}) {
		t.Fatalf("after an update, read as the release before reads it: %+v", in)
	}
	if _, err := r.m.Start(context.Background(), "alpha", true); err != nil {
		t.Fatal(err)
	}
	r.readsLikeTheReleaseBefore("alpha")

	// An automatic update of another instance.
	r.m.opts.SteamTarget = func(src string) (string, time.Time, bool) { return "steam-main-2", time.Now(), src == "main" }
	if _, err := r.m.Create(CreateRequest{Name: "beta", Build: "steam-main-1", Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(nil),
		AutoUpdate: ptr(AutoUpdateRestart), UpdateMaxWait: ptr(config.Duration(time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	r.m.autoTick(time.Now())
	r.waitAuto("beta")
	if in = r.readsLikeTheReleaseBefore("beta"); in.Build != "steam-main-2" {
		t.Fatalf("after an automatic update, read as the release before reads it: %+v", in)
	}

	// The folder: the files the release before knows, and the revision.
	known := []string{"instance.json", "revision", "secrets.json", "data", "run", "logs", "console.log"}
	entries, err := r.st.ReadDir(instDir("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, e := range entries {
		have = append(have, e.Name())
		if !slices.Contains(known, e.Name()) {
			t.Errorf("the folder of alpha has %s, which the release before has never had", e.Name())
		}
	}
	if !slices.Contains(have, "revision") {
		t.Errorf("the folder of alpha has %v, no revision file", have)
	}
	// The one new file is not in a file the release before reads whole.
	if b, _ := r.st.ReadFile(instFile("alpha")); bytes.Contains(b, []byte("revision")) {
		t.Errorf("instance.json mentions the revision: %s", b)
	}
}

// A change that names a revision is made only when the instance is at it.
// At another, nothing is done: the instance, its file, its secrets and its
// revision stay as they were, and the refusal is of the kind
// fault.Precondition with the code "changed", which says the revision the
// instance is at. No condition, and a condition that holds, are taken.
func TestChangeOfAnotherRevisionIsRefused(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha")}}); err != nil { // 2
		t.Fatal(err)
	}
	fileBefore, err := r.st.ReadFile(instFile("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	secretsBefore, _ := r.st.ReadFile(secretsFile("alpha"))
	revisionBefore, _ := r.st.ReadFile(revisionFile("alpha"))
	viewBefore, _ := json.Marshal(mustGet(t, r.m, "alpha"))

	for _, stale := range []int64{1, 3, 99} {
		_, _, err := r.m.PatchIf("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Hijacked"), Rules: fastRules(map[string]string{"X": "1"})}}, Condition{Revision: stale})
		checkChanged(t, fmt.Sprintf("a change for revision %d", stale), err, 2, stale)
		_, _, _, err = r.m.SetSecretsIf("alpha", SecretsRequest{ServerPassword: ptr("hijacked 1")}, Condition{Revision: stale})
		checkChanged(t, fmt.Sprintf("secrets for revision %d", stale), err, 2, stale)
	}
	// The refusal comes before the change is looked at: a change that is
	// invalid as well, made for an old revision, is "changed".
	_, _, err = r.m.PatchIf("alpha", PatchRequest{Settings: Settings{MaxPlayers: ptr(0)}}, Condition{Revision: 1})
	checkChanged(t, "an invalid change for an old revision", err, 2, 1)

	fileAfter, _ := r.st.ReadFile(instFile("alpha"))
	secretsAfter, _ := r.st.ReadFile(secretsFile("alpha"))
	revisionAfter, _ := r.st.ReadFile(revisionFile("alpha"))
	viewAfter, _ := json.Marshal(mustGet(t, r.m, "alpha"))
	if string(fileAfter) != string(fileBefore) || string(secretsAfter) != string(secretsBefore) || string(revisionAfter) != string(revisionBefore) ||
		string(viewAfter) != string(viewBefore) {
		t.Fatalf("a refused change changed something:\nfile %s\nthen %s\nrevision %q then %q\nview %s\nthen %s", fileBefore, fileAfter, revisionBefore, revisionAfter, viewBefore, viewAfter)
	}

	// The revision it is at: taken, and the next one is 3.
	v, _, err := r.m.PatchIf("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha Two")}}, Condition{Revision: 2})
	if err != nil || v.Revision != 3 || v.DisplayName != "Alpha Two" {
		t.Fatalf("a change for the current revision: %v, %+v", err, v.Instance)
	}
	_, rev, _, err := r.m.SetSecretsIf("alpha", SecretsRequest{ServerPassword: ptr("join me 7")}, Condition{Revision: 3})
	if err != nil || rev != 4 || mustGet(t, r.m, "alpha").Revision != 4 {
		t.Fatalf("secrets for the current revision: %v, revision %d", err, rev)
	}
	// The change that was made for 2 is refused now, though it was right a
	// moment ago.
	_, _, err = r.m.PatchIf("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Late")}}, Condition{Revision: 2})
	checkChanged(t, "a change for a revision that has passed", err, 4, 2)
	// No condition: always taken.
	if v, _, err = r.m.PatchIf("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Free")}}, Condition{}); err != nil || v.Revision != 5 {
		t.Fatalf("a change without a condition: %v, revision %d", err, v.Revision)
	}
	// An instance that is not there is not found, whatever the condition.
	if _, _, err := r.m.PatchIf("ghost", PatchRequest{}, Condition{Revision: 1}); !fault.Is(err, fault.NotFound) {
		t.Fatalf("an instance that does not exist: %v", err)
	}
}

func mustGet(t *testing.T, m *Manager, name string) View {
	t.Helper()
	v, err := m.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// checkChanged checks that err is the refusal of a change made for the
// revision asked while the instance is at current.
func checkChanged(t *testing.T, what string, err error, current, asked int64) {
	t.Helper()
	if !fault.Is(err, fault.Precondition) || fault.CodeOf(err) != "changed" {
		t.Errorf("%s: %v, want the refusal %q of kind Precondition", what, err, "changed")
		return
	}
	msg := err.Error()
	for _, part := range []string{"changed since they were read", fmt.Sprintf("revision %d now", current), fmt.Sprintf("for revision %d", asked)} {
		if !strings.Contains(msg, part) {
			t.Errorf("%s: the refusal %q does not say %q", what, msg, part)
		}
	}
}

// Of any number of changes made for the same revision at the same time,
// exactly one is made, the others are refused as changed, and the instance
// holds what the one that won set and has the next revision, however the
// changes are mixed: changes of settings and of secrets alike.
func TestOnlyOneOfConcurrentChangesForOneRevisionWins(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	const n = 24
	for round := 0; round < 5; round++ {
		start := mustGet(t, r.m, "alpha").Revision
		var wg sync.WaitGroup
		gate := make(chan struct{})
		results := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				cond := Condition{Revision: start}
				if i%4 == 3 {
					_, _, _, results[i] = r.m.SetSecretsIf("alpha", SecretsRequest{ServerPassword: ptr(fmt.Sprintf("secret %02d %d", i, round))}, cond)
					return
				}
				_, _, results[i] = r.m.PatchIf("alpha", PatchRequest{Settings: Settings{DisplayName: ptr(fmt.Sprintf("writer-%02d", i))}}, cond)
			}()
		}
		close(gate)
		wg.Wait()
		winners := -1
		for i, err := range results {
			switch {
			case err == nil && winners >= 0:
				t.Fatalf("round %d: changes %d and %d were both made for revision %d", round, winners, i, start)
			case err == nil:
				winners = i
			case !fault.Is(err, fault.Precondition) || fault.CodeOf(err) != "changed":
				t.Fatalf("round %d: change %d: %v, want the refusal \"changed\"", round, i, err)
			}
		}
		if winners < 0 {
			t.Fatalf("round %d: no change was made for revision %d", round, start)
		}
		v := mustGet(t, r.m, "alpha")
		if v.Revision != start+1 || r.storedRevision("alpha") != start+1 {
			t.Fatalf("round %d: revision %d (stored %d), want %d", round, v.Revision, r.storedRevision("alpha"), start+1)
		}
		if winners%4 != 3 && v.DisplayName != fmt.Sprintf("writer-%02d", winners) {
			t.Fatalf("round %d: the display name is %q, change %d won", round, v.DisplayName, winners)
		}
	}
}

// A change with no condition made while others race on a revision is never
// lost to them: it is made, and the revision of whoever raced is then
// stale.
func TestUnconditionalChangeMakesTheRevisionStale(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	read := mustGet(t, r.m, "alpha").Revision
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{Rules: fastRules(map[string]string{"Banner": "set meanwhile"})}}); err != nil {
		t.Fatal(err)
	}
	// A panel that read before, merged and writes the whole rules map back.
	merged := *fastRules(map[string]string{"HarvestRate": "2"})
	_, _, err := r.m.PatchIf("alpha", PatchRequest{Settings: Settings{Rules: &merged}}, Condition{Revision: read})
	checkChanged(t, "the panel's change", err, read+1, read)
	if got := mustGet(t, r.m, "alpha").Rules["Banner"]; got != "set meanwhile" {
		t.Fatalf("the change made meanwhile was lost: Banner is %q", got)
	}
}
