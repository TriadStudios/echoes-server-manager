package e2e

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// esm jobs lists the jobs and esm job <id> shows one, with --json too; the
// verbs that start an update, a backup or a restore still wait for their
// own and make no job.
func TestCLIJobs(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "1.0", fastRules(nil))
	code, out, errOut := h.runCLI("jobs")
	if code != 0 || oneLine(t, "jobs", out) != "no jobs" || errOut != "" {
		t.Fatalf("no jobs: %d %q %q", code, out, errOut)
	}
	code, out, _ = h.runCLI("--json", "jobs")
	if code != 0 || strings.TrimSpace(out) != `{"jobs":[]}` {
		t.Fatalf("no jobs as JSON: %d %q", code, out)
	}

	j := h.jobEnds(h.startAsync(h.admin, "/api/v1/instances/alpha/backup", nil).ID)
	if j.State != jobs.StateDone {
		t.Fatalf("job: %+v", j)
	}
	bad := h.jobEnds(h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: filepath.Join(h.src, "1.0")}).ID)
	if bad.State != jobs.StateFailed {
		t.Fatalf("job: %+v", bad)
	}
	code, out, errOut = h.runCLI("jobs")
	line := oneLine(t, "jobs", out)
	if code != 0 || errOut != "" || !strings.HasPrefix(line, "2 jobs: "+bad.ID+" install ") || !strings.Contains(line, "; "+j.ID+" backup alpha: done in ") ||
		!strings.Contains(line, "failed in ") || !strings.Contains(line, "(build_exists)") {
		t.Fatalf("jobs: %d %q %q", code, out, errOut)
	}
	code, out, _ = h.runCLI("jobs", "--state", "done", "--target", "alpha")
	if line := oneLine(t, "jobs filtered", out); code != 0 || !strings.HasPrefix(line, "1 job: "+j.ID+" backup alpha") || strings.Contains(line, bad.ID) {
		t.Fatalf("jobs filtered: %d %q", code, out)
	}
	code, out, _ = h.runCLI("jobs", "--state", "running")
	if oneLine(t, "jobs running", out) != "no jobs" || code != 0 {
		t.Fatalf("running jobs: %d %q", code, out)
	}
	code, out, _ = h.runCLI("--json", "jobs", "--state", "failed")
	var list api.JobList
	if err := json.Unmarshal([]byte(out), &list); code != 0 || err != nil || len(list.Jobs) != 1 || list.Jobs[0].ID != bad.ID || list.Jobs[0].Error == nil || list.Jobs[0].Error.Code != "build_exists" {
		t.Fatalf("jobs as JSON: %d %q %v", code, out, err)
	}

	code, out, errOut = h.runCLI("job", j.ID)
	line = oneLine(t, "job", out)
	if code != 0 || errOut != "" || !strings.HasPrefix(line, j.ID+" backup alpha: done in ") || !strings.Contains(line, "; started by token ") ||
		!strings.Contains(line, "; steps: wait_save ") && !strings.Contains(line, "; steps: copy ") || !strings.Contains(line, "prune ") {
		t.Fatalf("job: %d %q %q", code, out, errOut)
	}
	code, out, _ = h.runCLI("--json", "job", j.ID)
	var one jobs.Job
	if err := json.Unmarshal([]byte(out), &one); code != 0 || err != nil || one.ID != j.ID || len(one.Steps) != len(j.Steps) || one.Result == nil {
		t.Fatalf("job as JSON: %d %q %v", code, out, err)
	}
	code, out, errOut = h.runCLI("job", "nope")
	if code != 1 || out != "" || !strings.Contains(errOut, `job "nope" not found`) {
		t.Fatalf("job that is not there: %d %q %q", code, out, errOut)
	}
	code, out, _ = h.runCLI("--json", "job", "nope")
	if code != 1 || !strings.Contains(out, `"code":"not_found"`) {
		t.Fatalf("job that is not there as JSON: %d %q", code, out)
	}
	for _, args := range [][]string{{"job"}, {"job", "a", "b"}, {"jobs", "extra"}, {"jobs", "--state", "paused"}} {
		code, out, errOut = h.runCLI(args...)
		if code != 1 || out != "" || errOut == "" {
			t.Errorf("esm %v: %d %q %q", args, code, out, errOut)
		}
	}

	// The verbs wait, and make no job.
	code, out, errOut = h.runCLI("backup", "alpha")
	if code != 0 || !strings.HasPrefix(oneLine(t, "backup", out), "backup ") || errOut != "" {
		t.Fatalf("backup: %d %q %q", code, out, errOut)
	}
	if got := jobList(t, h, h.admin, ""); len(got) != 2 {
		t.Fatalf("the verb made a job: %+v", got)
	}
}
