package page_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The long calls of the page (the update of a server, a backup, a restore
// and the install of a build) are started as jobs with ?async=true, and the
// page says the step they are in. These checks need no node.
func TestLongCallsAreJobsInTheScript(t *testing.T) {
	b, err := os.ReadFile("static/esm.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, need := range []string{
		"await runJob(inst(b.dataset.name) + '/backup', null, 'Backing up ' + b.dataset.name);",
		"await runJob(inst(b.dataset.name) + '/restore', { backup: b.dataset.id }, 'Restoring ' + b.dataset.name);",
		"await runJob(inst(f.dataset.name) + '/update', { to: to }, 'Updating ' + f.dataset.name + ' to build ' + to);",
		"await runJob('/api/v1/builds', body, 'Installing ' + body.source);",
		"call('POST', path + '?async=true', body)",
		"call('GET', '/api/v1/jobs/' + enc(id))",
		"call('DELETE', '/api/v1/jobs/' + enc(following))",
		"window.addEventListener('pagehide', follow.stop);",
	} {
		if strings.Count(js, need) != 1 {
			t.Errorf("esm.js does not have %q once", need)
		}
	}
	// What is not a long call stays as it was: the call that waits.
	for _, need := range []string{
		"await call('POST', inst(b.dataset.name) + '/start');",
		"await call('POST', inst(b.dataset.name) + '/stop');",
		"await call('POST', inst(b.dataset.name) + '/restart');",
		"r = await call('POST', '/api/v1/box/self-update');",
	} {
		if strings.Count(js, need) != 1 {
			t.Errorf("esm.js does not have %q once", need)
		}
	}
	for _, bad := range []string{
		"await call('POST', inst(b.dataset.name) + '/backup');",
		"await call('POST', inst(b.dataset.name) + '/restore', ",
		"await call('POST', inst(f.dataset.name) + '/update', ",
		"await call('POST', '/api/v1/builds', body);",
	} {
		if strings.Contains(js, bad) {
			t.Errorf("esm.js still calls and waits: %q", bad)
		}
	}
}

// jobsDriver runs jobText and followJob with timers of its own, so that a
// job that takes a minute takes no time. A scenario says what each look at
// the job gets (the last goes on for ever) and when the page is left.
const jobsDriver = `
var clock = 0, seq = 0, timers = [];
function later(fn, ms) { var t = { id: ++seq, at: clock + ms, fn: fn }; timers.push(t); return t; }
function cancel(t) { timers = timers.filter(function (x) { return x !== t; }); }
function tick() { return new Promise(function (r) { setImmediate(r); }); }
async function fire() {
  timers.sort(function (a, b) { return a.at - b.at || a.id - b.id; });
  var t = timers.shift();
  clock = t.at;
  t.fn();
  await tick();
}
async function run(sc) {
  clock = 0; timers = [];
  var n = 0, outcome = null, shown = [], gets = [];
  var io = {
    text: function (j) { return jobText(j, 'Working'); },
    show: function (j, text) { shown.push(text); },
    get: function (id) {
      var s = sc.looks[Math.min(n, sc.looks.length - 1)];
      n++;
      gets.push(id + '@' + clock);
      if (s.fail) { return Promise.reject(Object.assign(new Error('no answer'), { status: s.status || 0 })); }
      return Promise.resolve(s.job);
    },
    later: later,
    cancel: cancel
  };
  var f = followJob(sc.first, io);
  f.done.then(function (j) { outcome = { ended: j.state, at: clock }; }, function (e) { outcome = { gaveUp: e.status || 0, at: clock }; });
  if (sc.stopAt) { later(function () { f.stop(); }, sc.stopAt); }
  var guard = 0;
  while (outcome === null && timers.length && guard++ < 1000) { await fire(); }
  var left = 0;
  while (timers.length && guard++ < 1000) { await fire(); left++; }
  return { outcome: outcome, looks: n, shown: shown, gets: gets, left: left };
}
(async function () {
  var texts = ARGS.texts.map(function (c) { return jobText(c.job, c.label); });
  var words = ARGS.words.map(function (n) { return stepWords(n); });
  var sizes = ARGS.sizes.map(function (n) { return sizeText(n); });
  var runs = [];
  for (var i = 0; i < ARGS.scenarios.length; i++) { runs.push(await run(ARGS.scenarios[i])); }
  process.stdout.write(JSON.stringify({ texts: texts, words: words, sizes: sizes, runs: runs }));
})();
`

type scriptJob struct {
	ID          string       `json:"id"`
	State       string       `json:"state"`
	Step        string       `json:"step"`
	Steps       []scriptStep `json:"steps"`
	Cancellable bool         `json:"cancellable"`
}

type scriptStep struct {
	Name    string   `json:"name"`
	Bytes   int64    `json:"bytes,omitempty"`
	Total   int64    `json:"total,omitempty"`
	Percent *float64 `json:"percent,omitempty"`
}

type jobLook struct {
	Job    *scriptJob `json:"job,omitempty"`
	Fail   bool       `json:"fail,omitempty"`
	Status int        `json:"status,omitempty"`
}

type jobScenario struct {
	Name   string    `json:"name"`
	First  scriptJob `json:"first"`
	Looks  []jobLook `json:"looks"`
	StopAt int       `json:"stopAt,omitempty"`
}

type jobOutcome struct {
	Ended  string `json:"ended,omitempty"`
	GaveUp *int   `json:"gaveUp,omitempty"`
	At     int    `json:"at"`
}

type jobRun struct {
	Outcome *jobOutcome `json:"outcome"`
	Looks   int         `json:"looks"`
	Shown   []string    `json:"shown"`
	Gets    []string    `json:"gets"`
	Left    int         `json:"left"`
}

func pct(p float64) *float64 { return &p }

// The page says what a job does and the step it is in, with how far a
// download has come; it looks at the job every second until it is not
// running, says a step once, gives up after five looks in a row that fail
// (a job ESM does not know, at once), ends at once when the page goes away,
// and never looks again after it ended. The script decides this in jobText
// and followJob, which use nothing of the page; this test runs them with
// node, in a clock of its own. Without node it is skipped, except on the CI
// machines (GITHUB_ACTIONS=true), which have node: there it fails.
func TestJobsInTheScript(t *testing.T) {
	prog := scriptBlock(t, "jobs")
	node := nodeOrSkip(t, "jobs")
	running := func(steps ...scriptStep) scriptJob {
		j := scriptJob{ID: "j1", State: "running", Steps: steps}
		if len(steps) > 0 {
			j.Step = steps[len(steps)-1].Name
		}
		return j
	}
	ended := func(state string, step string) scriptJob { return scriptJob{ID: "j1", State: state, Step: step} }
	texts := []struct {
		job   scriptJob
		label string
		want  string
	}{
		{running(), "Updating alpha", "Updating alpha..."},
		{running(scriptStep{Name: "backup"}), "Updating alpha", "Updating alpha: backing up the world..."},
		{running(scriptStep{Name: "backup"}, scriptStep{Name: "wait_ready"}), "Updating alpha", "Updating alpha: waiting until the server is ready..."},
		{running(scriptStep{Name: "download", Bytes: 1 << 20, Total: 4 << 20, Percent: pct(25.04)}), "Installing steam:main", "Installing steam:main: downloading (25%, 1.0 MiB of 4.0 MiB)..."},
		{running(scriptStep{Name: "download", Bytes: 3 << 30, Total: 4 << 30, Percent: pct(75)}), "Installing x", "Installing x: downloading (75%, 3.0 GiB of 4.0 GiB)..."},
		{running(scriptStep{Name: "download", Bytes: 2048}), "Installing x", "Installing x: downloading (2.0 KiB)..."},
		{running(scriptStep{Name: "download", Percent: pct(61.9)}), "Installing x", "Installing x: downloading (61%)..."},
		{running(scriptStep{Name: "download", Bytes: 0, Total: 0}), "Installing x", "Installing x: downloading..."},
		{running(scriptStep{Name: "place"}), "Installing x", "Installing x: putting the build in place..."},
		// A step the page has no words for is said as its name.
		{running(scriptStep{Name: "some_new_step"}), "Working", "Working: some new step..."},
		{running(scriptStep{Name: "constructor"}), "Working", "Working: constructor..."},
	}
	words := []string{"backup", "stop", "switch_build", "start", "wait_ready", "rollback", "wait_save", "copy", "prune", "backup_current", "put_in_place",
		"download", "unpack", "verify", "place", "verify_signature", "place_binary", "restart", "toString"}
	wantWords := []string{"backing up the world", "stopping the server", "switching to the new build", "starting the server", "waiting until the server is ready",
		"putting the old build back", "waiting for the next world save", "copying the save files", "removing the oldest backups", "keeping the current world as a backup",
		"putting the backup in place", "downloading", "unpacking", "checking every file", "putting the build in place", "verify signature", "place binary", "restart", "toString"}
	sizes := []int64{0, 1, 1023, 1024, 1536, 1 << 20, 5 << 20, 1 << 30, 3 << 30}
	wantSizes := []string{"0 bytes", "1 bytes", "1023 bytes", "1.0 KiB", "1.5 KiB", "1.0 MiB", "5.0 MiB", "1.0 GiB", "3.0 GiB"}

	looks := func(js ...scriptJob) []jobLook {
		var out []jobLook
		for i := range js {
			out = append(out, jobLook{Job: &js[i]})
		}
		return out
	}
	step := func(name string) scriptJob { return running(scriptStep{Name: name}) }
	fail := jobLook{Fail: true}
	zero, notFound := 0, 404
	scenarios := []struct {
		s    jobScenario
		want jobRun
	}{
		{jobScenario{Name: "a job that runs to its end", First: running(), Looks: looks(step("backup"), step("backup"), step("stop"), ended("done", "stop"))},
			jobRun{Outcome: &jobOutcome{Ended: "done", At: 4000}, Looks: 4,
				Shown: []string{"Working...", "Working: backing up the world...", "Working: stopping the server..."},
				Gets:  []string{"j1@1000", "j1@2000", "j1@3000", "j1@4000"}}},
		{jobScenario{Name: "a job that failed", First: running(), Looks: looks(step("wait_ready"), ended("failed", "rollback"))},
			jobRun{Outcome: &jobOutcome{Ended: "failed", At: 2000}, Looks: 2, Shown: []string{"Working...", "Working: waiting until the server is ready..."},
				Gets: []string{"j1@1000", "j1@2000"}}},
		{jobScenario{Name: "a job that was cancelled", First: step("download"), Looks: looks(ended("cancelled", "download"))},
			jobRun{Outcome: &jobOutcome{Ended: "cancelled", At: 1000}, Looks: 1, Shown: []string{"Working: downloading..."}, Gets: []string{"j1@1000"}}},
		{jobScenario{Name: "a job that is over at the first look", First: running(), Looks: looks(ended("done", "copy"))},
			jobRun{Outcome: &jobOutcome{Ended: "done", At: 1000}, Looks: 1, Shown: []string{"Working..."}, Gets: []string{"j1@1000"}}},
		{jobScenario{Name: "two looks that fail, then it goes on", First: running(), Looks: append([]jobLook{fail, fail}, looks(step("copy"), ended("done", "prune"))...)},
			jobRun{Outcome: &jobOutcome{Ended: "done", At: 4000}, Looks: 4, Shown: []string{"Working...", "Working: copying the save files..."},
				Gets: []string{"j1@1000", "j1@2000", "j1@3000", "j1@4000"}}},
		{jobScenario{Name: "looks that fail four times in a row do not add up", First: running(),
			Looks: append(append([]jobLook{fail, fail, fail, fail}, looks(step("copy"))...), append([]jobLook{fail, fail, fail, fail}, looks(ended("done", "prune"))...)...)},
			jobRun{Outcome: &jobOutcome{Ended: "done", At: 10000}, Looks: 10, Shown: []string{"Working...", "Working: copying the save files..."},
				Gets: []string{"j1@1000", "j1@2000", "j1@3000", "j1@4000", "j1@5000", "j1@6000", "j1@7000", "j1@8000", "j1@9000", "j1@10000"}}},
		{jobScenario{Name: "five looks in a row that fail", First: running(), Looks: []jobLook{fail}},
			jobRun{Outcome: &jobOutcome{GaveUp: &zero, At: 5000}, Looks: 5, Shown: []string{"Working..."},
				Gets: []string{"j1@1000", "j1@2000", "j1@3000", "j1@4000", "j1@5000"}}},
		{jobScenario{Name: "a job ESM does not know", First: running(), Looks: []jobLook{{Fail: true, Status: 404}}},
			jobRun{Outcome: &jobOutcome{GaveUp: &notFound, At: 1000}, Looks: 1, Shown: []string{"Working..."}, Gets: []string{"j1@1000"}}},
		{jobScenario{Name: "the page is left", First: running(), Looks: looks(step("copy")), StopAt: 2500},
			jobRun{Outcome: nil, Looks: 2, Shown: []string{"Working...", "Working: copying the save files..."}, Gets: []string{"j1@1000", "j1@2000"}}},
	}
	args := struct {
		Texts []struct {
			Job   scriptJob `json:"job"`
			Label string    `json:"label"`
		} `json:"texts"`
		Words     []string      `json:"words"`
		Sizes     []int64       `json:"sizes"`
		Scenarios []jobScenario `json:"scenarios"`
	}{Words: words, Sizes: sizes}
	for _, c := range texts {
		args.Texts = append(args.Texts, struct {
			Job   scriptJob `json:"job"`
			Label string    `json:"label"`
		}{c.job, c.label})
	}
	for _, c := range scenarios {
		args.Scenarios = append(args.Scenarios, c.s)
	}
	arg, _ := json.Marshal(args)
	prog += "\nvar ARGS = " + string(arg) + ";\n" + jobsDriver
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(prog)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v\n%s", err, errOut.String())
	}
	var got struct {
		Texts []string `json:"texts"`
		Words []string `json:"words"`
		Sizes []string `json:"sizes"`
		Runs  []jobRun `json:"runs"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Texts) != len(texts) || len(got.Words) != len(words) || len(got.Sizes) != len(sizes) || len(got.Runs) != len(scenarios) {
		t.Fatalf("node printed %q: %v", out.String(), err)
	}
	for n, c := range texts {
		if got.Texts[n] != c.want {
			t.Errorf("jobText(%+v, %q) = %q, want %q", c.job, c.label, got.Texts[n], c.want)
		}
	}
	for n, w := range words {
		if got.Words[n] != wantWords[n] {
			t.Errorf("stepWords(%q) = %q, want %q", w, got.Words[n], wantWords[n])
		}
	}
	for n, s := range sizes {
		if got.Sizes[n] != wantSizes[n] {
			t.Errorf("sizeText(%d) = %q, want %q", s, got.Sizes[n], wantSizes[n])
		}
	}
	for n, c := range scenarios {
		g := got.Runs[n]
		a, _ := json.Marshal(g)
		w, _ := json.Marshal(c.want)
		if string(a) != string(w) {
			t.Errorf("%s:\n got  %s\n want %s", c.s.Name, a, w)
		}
	}
}
