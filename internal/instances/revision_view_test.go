package instances

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// raceRig makes the rig for the tests that read an instance while it is
// changed: two builds and the instance alpha at revision 1 on the first.
func raceRig(t *testing.T) (*rig, *rt) {
	t.Helper()
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2"})
	if _, err := r.m.Create(CreateRequest{Name: "alpha", Build: "steam-main-1", Settings: Settings{
		BindIP: ptr("127.0.0.1"), Rules: fastRules(nil), DisplayName: ptr("name-1")}}); err != nil {
		t.Fatal(err)
	}
	in, err := r.m.get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	return r, in
}

// whileChanging runs write once, in this goroutine, with readers that call
// read in a loop until it ends. Each reader is a goroutine of its own, and
// read is told which (0 to 8), so that one that waits for a lock of the
// manager does not keep the others from reading. read returns "" when what
// it saw is right, or what is wrong; the first few wrongs fail the test,
// with how many reads there were.
func whileChanging(t *testing.T, write func(), read func(worker int) string) {
	t.Helper()
	const readers = 9
	var (
		stop   atomic.Bool
		reads  atomic.Int64
		mu     sync.Mutex
		wrongs []string
		wrong  atomic.Int64
		wg     sync.WaitGroup
	)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				reads.Add(1)
				if msg := read(i); msg != "" {
					wrong.Add(1)
					mu.Lock()
					if len(wrongs) < 3 {
						wrongs = append(wrongs, msg)
					}
					mu.Unlock()
				}
			}
		}()
	}
	write()
	stop.Store(true)
	wg.Wait()
	if wrong.Load() > 0 {
		t.Fatalf("%d of %d reads were wrong, for example:\n%s", wrong.Load(), reads.Load(), strings.Join(wrongs, "\n"))
	}
	if reads.Load() < 100 {
		t.Fatalf("only %d reads were made while the instance was changed: the test saw nothing", reads.Load())
	}
}

// What an instance shows is always one state: the revision a view, a
// reading or a list gives is the revision of the settings it gives, never
// ahead of them. A caller that reads a revision and the settings that go
// with it, merges its change into them and sends the revision as If-Match,
// would otherwise be let through over the change that revision stands for
// (the lost update the revision is there to stop). Each change here sets
// the display name to the revision it makes (or the build, to the one that
// goes with its parity), so any view that shows a revision with the
// settings of another is found. The views are read as fast as they can
// meanwhile, from several goroutines.
func TestAViewShowsTheRevisionOfItsSettings(t *testing.T) {
	t.Run("change", func(t *testing.T) {
		r, in := raceRig(t)
		var failed string
		whileChanging(t, func() {
			cur := int64(1)
			for i := 0; i < 300 && failed == ""; i++ {
				v, _, err := r.m.PatchIf("alpha", PatchRequest{Settings: Settings{DisplayName: ptr(fmt.Sprintf("name-%d", cur+1))}}, Condition{Revision: cur})
				if err != nil || v.Revision != cur+1 || v.DisplayName != fmt.Sprintf("name-%d", cur+1) {
					failed = fmt.Sprintf("change %d at revision %d: %v, the view it returns is at revision %d (%q)", i+1, cur, err, v.Revision, v.DisplayName)
					return
				}
				cur = v.Revision
			}
		}, func(worker int) string {
			// The three ways to read an instance. Get and List take the
			// manager's lock, which a change holds while it is made.
			var v View
			switch worker % 3 {
			case 0:
				v = r.m.view(in)
			case 1:
				v, _ = r.m.Get("alpha")
			default:
				list := r.m.List()
				if len(list) != 1 {
					return fmt.Sprintf("the list has %d instances", len(list))
				}
				v = list[0]
			}
			if want := fmt.Sprintf("name-%d", v.Revision); v.DisplayName != want {
				return fmt.Sprintf("a view at revision %d shows the display name %q, which is that of revision %s", v.Revision, v.DisplayName, strings.TrimPrefix(v.DisplayName, "name-"))
			}
			return ""
		})
		if failed != "" {
			t.Fatal(failed)
		}
	})

	// A switch of the build is stored with its revision in one step.
	t.Run("build", func(t *testing.T) {
		r, in := raceRig(t)
		// Revision 2 is on steam-main-2, 3 on steam-main-1, and so on; 1 is on steam-main-1.
		builds := []string{"steam-main-2", "steam-main-1"}
		whileChanging(t, func() {
			for i := 0; i < 300; i++ {
				in.op.Lock()
				err := r.m.setBuild(in, builds[i%2], nil)
				in.op.Unlock()
				if err != nil {
					t.Error(err)
					return
				}
			}
		}, func(worker int) string {
			v := r.m.view(in)
			if want := builds[v.Revision%2]; v.Build != want {
				return fmt.Sprintf("a view at revision %d shows the build %s, want %s", v.Revision, v.Build, want)
			}
			return ""
		})
	})

	// The secrets are read from their file when a view is made, after the
	// revision, so the file has to be written before the revision is shown:
	// a reader that takes the revision and then reads the file, as a view
	// does, finds the file as new as the revision, or newer. Each change
	// sets the server password to a number, the one of the revision it makes.
	t.Run("secrets", func(t *testing.T) {
		r, in := raceRig(t)
		var failed string
		whileChanging(t, func() {
			cur := int64(1)
			for i := 0; i < 300 && failed == ""; i++ {
				_, rev, _, err := r.m.SetSecretsIf("alpha", SecretsRequest{ServerPassword: ptr(fmt.Sprintf("password-%d", cur+1))}, Condition{Revision: cur})
				if err != nil || rev != cur+1 {
					failed = fmt.Sprintf("change %d at revision %d: %v, the revision it returns is %d", i+1, cur, err, rev)
					return
				}
				cur = rev
			}
		}, func(worker int) string {
			in.mu.Lock()
			rev := in.revision
			in.mu.Unlock()
			if rev == 1 {
				return ""
			}
			b, err := r.st.ReadFile(secretsFile("alpha"))
			if err != nil {
				return fmt.Sprintf("at revision %d there is no secrets file: %v", rev, err)
			}
			var s Secrets
			if err := json.Unmarshal(b, &s); err != nil {
				return fmt.Sprintf("the secrets file is not JSON: %v", err)
			}
			got, err := strconv.ParseInt(strings.TrimPrefix(s.ServerPassword, "password-"), 10, 64)
			if err != nil {
				return fmt.Sprintf("at revision %d the secrets file holds the password %q", rev, s.ServerPassword)
			}
			if got < rev {
				return fmt.Sprintf("the instance is at revision %d, and its secrets file is that of revision %d", rev, got)
			}
			// The views are made the same way, whole.
			if v := r.m.view(in); v.Revision > 1 && !v.Secrets.ServerPasswordSet {
				return fmt.Sprintf("a view at revision %d says no server password is set", v.Revision)
			}
			return ""
		})
		if failed != "" {
			t.Fatal(failed)
		}
	})
}
