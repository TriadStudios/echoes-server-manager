package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// storedRevision is the revision instance.json holds, 0 when the file has
// none.
func (r *rig) storedRevision(name string) int64 {
	r.t.Helper()
	b, err := r.st.ReadFile(instFile(name))
	if err != nil {
		r.t.Fatal(err)
	}
	var in struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		r.t.Fatal(err)
	}
	return in.Revision
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
// secrets, and the switch of its build by an update. The revision is in
// instance.json, so it is the same after ESM starts again; a request that
// is refused, or that only reads, leaves it as it is.
func TestRevisionGrowsWithEveryStoredChange(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2"})
	v, err := r.m.Create(CreateRequest{Name: "alpha", Build: "steam-main-1", Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(nil)}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != 1 || r.storedRevision("alpha") != 1 {
		t.Fatalf("a new instance is at revision %d (stored %d), want 1", v.Revision, r.storedRevision("alpha"))
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

// An instance stored before there were revisions has none, and starts at 1
// when it is read; its first change makes it 2, and from then on the file
// holds it.
func TestRevisionOfAnInstanceStoredBefore(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	b, err := r.st.ReadFile(instFile("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["revision"]; !ok {
		t.Fatalf("instance.json has no revision: %s", b)
	}
	delete(raw, "revision")
	if err := r.st.WriteJSON(instFile("alpha"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if r.storedRevision("alpha") != 0 {
		t.Fatal("the test file still has a revision")
	}

	m := r.newManager(&recordSup{})
	defer m.Close()
	v, err := m.Get("alpha")
	if err != nil || v.Revision != 1 {
		t.Fatalf("an instance stored without a revision: %v, revision %d, want 1", err, v.Revision)
	}
	// Read as 1, a change that names 1 is taken, and makes 2.
	v, _, err = m.PatchIf("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha")}}, Condition{Revision: 1})
	if err != nil || v.Revision != 2 || r.storedRevision("alpha") != 2 {
		t.Fatalf("the first change: %v, revision %d (stored %d), want 2", err, v.Revision, r.storedRevision("alpha"))
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
	viewAfter, _ := json.Marshal(mustGet(t, r.m, "alpha"))
	if string(fileAfter) != string(fileBefore) || string(secretsAfter) != string(secretsBefore) || string(viewAfter) != string(viewBefore) {
		t.Fatalf("a refused change changed something:\nfile %s\nthen %s\nview %s\nthen %s", fileBefore, fileAfter, viewBefore, viewAfter)
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
