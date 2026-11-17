package builds_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

func stepsOf(j jobs.Job) []string {
	out := []string{}
	for _, s := range j.Steps {
		out = append(out, s.Name)
	}
	return out
}

func endOf(t *testing.T, run *jobs.Runner, id string) jobs.Job {
	t.Helper()
	j, ok := run.Wait(id, 20*time.Second)
	if !ok || j.State == jobs.StateRunning {
		t.Fatalf("job did not end: %+v", j)
	}
	return j
}

// An install that runs as a job tells its steps; the same call outside a job
// tells nothing and runs as ever.
func TestInstallTellsItsSteps(t *testing.T) {
	r := newRig(t, builds.Options{})
	run := jobs.New(jobs.Options{})
	folder := testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "1.0.0"})
	j, err := run.Start(jobs.Spec{Kind: jobs.KindInstall, Target: folder}, func(ctx context.Context) (any, error) {
		return r.m.Install(ctx, folder, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	got := endOf(t, run, j.ID)
	if got.State != jobs.StateDone || !slices.Equal(stepsOf(got), []string{"unpack", "verify", "place"}) || got.Cancellable {
		t.Fatalf("job: %+v %+v", got, got.Error)
	}
	if _, err := r.m.Get("1.0.0"); err != nil {
		t.Fatal(err)
	}
	// Outside a job: as before.
	folder2 := testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "1.1.0"})
	if _, err := r.m.Install(context.Background(), folder2, ""); err != nil {
		t.Fatal(err)
	}
	r.noLeftovers(t)
}

// A job that is cancelled before its build is placed places nothing and
// leaves nothing; a request whose context ended is not stopped at the same
// place, as it never was.
func TestACancelledInstallPlacesNothing(t *testing.T) {
	r := newRig(t, builds.Options{})
	run := jobs.New(jobs.Options{})
	folder := testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "2.0.0"})
	atVerify, goOn := make(chan struct{}), make(chan struct{})
	j, _ := run.Start(jobs.Spec{Kind: jobs.KindInstall, Target: folder}, func(ctx context.Context) (any, error) {
		// What Install does, with a pause before its verify step.
		jobs.Step(ctx, "unpack")
		close(atVerify)
		<-goOn
		return r.m.Install(ctx, folder, "")
	})
	<-atVerify
	if _, err := run.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	close(goOn)
	got := endOf(t, run, j.ID)
	if got.State != jobs.StateCancelled {
		t.Fatalf("job: %+v %+v", got, got.Error)
	}
	if _, err := r.m.Get("2.0.0"); err == nil {
		t.Fatal("a build was placed after the cancel")
	}
	r.noLeftovers(t)

	// A cancelled context outside a job (a client that left) does not stop
	// the verify and place steps: only a job's does.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	folder3 := testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "2.1.0"})
	if _, err := r.m.Install(ctx, folder3, ""); err != nil {
		t.Fatalf("install with an ended request context: %v", err)
	}
}

// CheckInstall refuses what Install refuses before it reads or downloads a
// byte, with the same code, and accepts what it accepts.
func TestCheckInstallAnswersAsInstallDoes(t *testing.T) {
	r := newRig(t, builds.Options{})
	folder := testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "1.0.0"})
	for _, c := range []struct {
		name, source, sha, code string
	}{
		{"no source", "", "", "invalid"},
		{"a sha256 that is none", folder, "xyz", "invalid"},
		{"plain http", "http://example.invalid/a.zip", strings.Repeat("0", 64), "invalid"},
		{"an address without sha256", "https://example.invalid/a.zip", "", "invalid"},
		{"an address with user information", "https://u:p@example.invalid/a.zip", strings.Repeat("0", 64), "invalid"},
		{"an address of no archive", "https://example.invalid/a.exe", strings.Repeat("0", 64), "invalid"},
		{"outside the import folder", "/", "", "source_not_allowed"},
		{"a folder that is not there", "nothing-here", "", "invalid"},
		{"a sha256 for a folder", folder, strings.Repeat("0", 64), "invalid"},
		{"a file that is no archive", folder + "/manifest.json", "", "invalid"},
	} {
		check := r.m.CheckInstall(c.source, c.sha)
		_, install := r.m.Install(context.Background(), c.source, c.sha)
		if check == nil || install == nil || code(check) != c.code || code(install) != c.code {
			t.Errorf("%s: check %v, install %v, want %s", c.name, check, install, c.code)
		}
	}
	// What is fine for the request is fine for the check, with no sha256 on a
	// folder, and with one on an address.
	for _, c := range []struct{ source, sha string }{
		{folder, ""},
		{"https://example.invalid/a.tar.gz", strings.Repeat("0", 64)},
		{"HTTPS://example.invalid/a.TGZ", strings.ToUpper(strings.Repeat("a", 64))},
	} {
		if err := r.m.CheckInstall(c.source, c.sha); err != nil {
			t.Errorf("CheckInstall(%q): %v", c.source, err)
		}
	}
	r.noLeftovers(t)
}

// A download tells the bytes it has, and a job that is cancelled during it
// ends it.
func TestADownloadTellsItsBytesAndStopsWhenCancelled(t *testing.T) {
	dir := testkit.MakeBuild(t, t.TempDir(), testkit.BuildOptions{Version: "3.0.0"})
	archive := t.TempDir() + "/b.tar.gz"
	testkit.MakeTarGz(t, dir, archive)
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	gate := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-req.Context().Done():
			return
		}
		w.Write(data[len(data)/2:])
	}))
	t.Cleanup(func() { once.Do(func() { close(gate) }); srv.Close() })
	r := newRig(t, builds.Options{HTTPClient: srv.Client()})
	run := jobs.New(jobs.Options{})
	digest := sha256.Sum256(data)
	sum := hex.EncodeToString(digest[:])
	j, _ := run.Start(jobs.Spec{Kind: jobs.KindInstall, Target: srv.URL}, func(ctx context.Context) (any, error) {
		return r.m.Install(ctx, srv.URL+"/b.tar.gz", sum)
	})
	var cur jobs.Job
	testkit.Eventually(t, 10*time.Second, func() bool {
		cur, _ = run.Get(j.ID)
		// The server holds the second half back, so the bytes come in up to
		// the half and stay there.
		return len(cur.Steps) == 1 && cur.Steps[0].Bytes == int64(len(data)/2)
	}, "the download is not at its half: %+v", &cur)
	s := cur.Steps[0]
	if s.Name != "download" || s.Total != int64(len(data)) || s.Bytes != int64(len(data)/2) || s.Percent == nil || !cur.Cancellable {
		t.Fatalf("download step: %+v cancellable %v", s, cur.Cancellable)
	}
	if _, err := run.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	got := endOf(t, run, j.ID)
	if got.State != jobs.StateCancelled {
		t.Fatalf("job: %+v %+v", got, got.Error)
	}
	r.noLeftovers(t)
	// Let go, the same install runs through all its steps.
	once.Do(func() { close(gate) })
	j2, _ := run.Start(jobs.Spec{Kind: jobs.KindInstall, Target: srv.URL}, func(ctx context.Context) (any, error) {
		return r.m.Install(ctx, srv.URL+"/b.tar.gz", sum)
	})
	done := endOf(t, run, j2.ID)
	if done.State != jobs.StateDone || !slices.Equal(stepsOf(done), []string{"download", "unpack", "verify", "place"}) {
		t.Fatalf("job: %+v %+v", done, done.Error)
	}
	if done.Steps[0].Bytes != int64(len(data)) {
		t.Fatalf("bytes of a finished download: %+v", done.Steps[0])
	}
}
