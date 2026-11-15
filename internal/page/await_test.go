package page_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// scriptBlock is the part of the script between "// name: start" and
// "// name: end", which uses nothing of the page, so node can run it alone.
func scriptBlock(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("static/esm.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	i, j := strings.Index(script, "// "+name+": start"), strings.Index(script, "// "+name+": end")
	if i < 0 || j < i {
		t.Fatalf("the script has no %s between its markers", name)
	}
	return script[i:j]
}

// nodeOrSkip finds node: without it the test is skipped, except on the CI
// machines (GITHUB_ACTIONS=true), which have node: there it fails.
func nodeOrSkip(t *testing.T, what string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("node is not installed on this CI machine, so the script's %s cannot be run", what)
		}
		t.Skipf("node is not installed, so the script's %s is not run here", what)
	}
	return node
}

// The Box screen's update button waits for ESM to come back instead of
// asking for a reload: it looks at GET /api/v1/box every 2 seconds for
// 60 seconds, from a timer that ends with the page, and then reloads the
// Box screen. These checks need no node.
func TestSelfUpdateWaitsInTheScript(t *testing.T) {
	b, err := os.ReadFile("static/esm.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, need := range []string{
		"await waitForRestart(r.to);",
		"var WAIT_EVERY = 2000;",
		"var WAIT_FOR = 60000;",
		"fetch('/api/v1/box', {",
		"window.addEventListener('pagehide', w.stop);",
		"location.replace('/box#esm-h');",
		"waiting for ESM to restart...",
		"(esm serve); then reload this page.",
	} {
		if strings.Count(js, need) != 1 {
			t.Errorf("esm.js does not have %q once", need)
		}
	}
	for _, bad := range []string{"Reload this page in a few seconds", "setInterval("} {
		if strings.Contains(js, bad) {
			t.Errorf("esm.js has %q", bad)
		}
	}
	// The wait is only for an ESM that restarts: one that ends for good
	// still says to start it by hand.
	if !strings.Contains(js, "ESM ends now. Start it again on the box (esm serve), then reload this page.") {
		t.Error("esm.js lost what it says when nothing restarts ESM")
	}
}

// awaitDriver runs awaitEsm and updateStep with a clock and timers of its
// own, so that 60 seconds take no time. A scenario says what each look at
// /api/v1/box gets (the last step goes on for ever; took is how long the
// answer takes) and when the page is left (stopAt).
const awaitDriver = `
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
  var n = 0, how = null, at = -1;
  var io = {
    now: function () { return clock; },
    later: later,
    cancel: cancel,
    look: function () {
      var s = sc.steps[Math.min(n, sc.steps.length - 1)];
      n++;
      return new Promise(function (resolve, reject) {
        function answer() {
          if (s.fail) { reject(new Error('refused')); return; }
          resolve({ status: s.status, box: s.version ? { version: s.version } : null });
        }
        if (s.took) { later(answer, s.took); } else { answer(); }
      });
    }
  };
  var w = awaitEsm(sc.want, io);
  w.done.then(function (h) { how = h; at = clock; });
  if (sc.stopAt) { later(function () { w.stop(); }, sc.stopAt); }
  var guard = 0;
  while (how === null && timers.length && guard++ < 1000) { await fire(); }
  // What is still due after the end must not look again.
  while (timers.length && guard++ < 1000) { await fire(); }
  return { how: how, looks: n, at: at, left: timers.length };
}
(async function () {
  var steps = ARGS.steps.map(function (s) { return updateStep(s.status, s.box, s.want); });
  var runs = [];
  for (var i = 0; i < ARGS.scenarios.length; i++) { runs.push(await run(ARGS.scenarios[i])); }
  process.stdout.write(JSON.stringify({ steps: steps, runs: runs }));
})();
`

type lookStep struct {
	Status  int    `json:"status,omitempty"`
	Version string `json:"version,omitempty"`
	Fail    bool   `json:"fail,omitempty"`
	Took    int    `json:"took,omitempty"`
}

type awaitScenario struct {
	Name   string     `json:"name"`
	Want   string     `json:"want"`
	Steps  []lookStep `json:"steps"`
	StopAt int        `json:"stopAt,omitempty"`
}

type awaitRun struct {
	How   string `json:"how"`
	Looks int    `json:"looks"`
	At    int    `json:"at"`
	Left  int    `json:"left"`
}

// After a self-update the page waits until ESM answers with the new
// version, or without the login (a restarted ESM keeps no sessions), looks
// every 2 seconds, gives up after 60, ends at once when the page goes
// away, and never looks again after it ended. The script decides this in
// updateStep and awaitEsm alone, which use nothing of the page; this test
// runs them with node, in a clock of its own. Without node it is skipped,
// except on the CI machines (GITHUB_ACTIONS=true), which have node: there
// it fails.
func TestAwaitEsmInTheScript(t *testing.T) {
	prog := scriptBlock(t, "sessionEnded") + scriptBlock(t, "awaitEsm")
	node := nodeOrSkip(t, "awaitEsm")
	const want = "v0.2.0"
	type stepCase struct {
		Status int            `json:"status"`
		Box    map[string]any `json:"box"`
		Want   string         `json:"want"`
	}
	stepCases := []struct {
		in   stepCase
		step string
	}{
		{stepCase{200, map[string]any{"version": want}, want}, "back"},
		{stepCase{200, map[string]any{"version": "v0.1.0"}, want}, "wait"},
		{stepCase{200, map[string]any{"version": ""}, want}, "wait"},
		{stepCase{200, nil, want}, "wait"},
		// The login is gone: the restarted ESM keeps no sessions.
		{stepCase{401, nil, want}, "back"},
		{stepCase{429, nil, want}, "back"},
		// A proxy in front of ESM, or ESM itself, not ready.
		{stepCase{502, nil, want}, "wait"},
		{stepCase{503, nil, want}, "wait"},
		{stepCase{504, nil, want}, "wait"},
		{stepCase{500, nil, want}, "wait"},
		{stepCase{403, nil, want}, "wait"},
		{stepCase{404, nil, want}, "wait"},
	}
	old, fresh := lookStep{Status: 200, Version: "v0.1.0"}, lookStep{Status: 200, Version: want}
	refused := lookStep{Fail: true}
	scenarios := []struct {
		s    awaitScenario
		want awaitRun
	}{
		{awaitScenario{Name: "the old ESM ends, the new one has no login", Steps: []lookStep{old, refused, refused, {Status: 502}, {Status: 401}}},
			awaitRun{How: "back", Looks: 5, At: 10000}},
		{awaitScenario{Name: "the new version answers with the login", Steps: []lookStep{old, old, fresh}},
			awaitRun{How: "back", Looks: 3, At: 6000}},
		{awaitScenario{Name: "errors of a proxy first", Steps: []lookStep{{Status: 502}, {Status: 504}, refused, fresh}},
			awaitRun{How: "back", Looks: 4, At: 8000}},
		{awaitScenario{Name: "the address is limited", Steps: []lookStep{refused, {Status: 429}}},
			awaitRun{How: "back", Looks: 2, At: 4000}},
		{awaitScenario{Name: "nothing answers", Steps: []lookStep{refused}},
			awaitRun{How: "late", Looks: 30, At: 60000}},
		{awaitScenario{Name: "the old version for ever", Steps: []lookStep{old}},
			awaitRun{How: "late", Looks: 30, At: 60000}},
		{awaitScenario{Name: "another version than the one waited for", Steps: []lookStep{{Status: 200, Version: "v0.3.0"}}},
			awaitRun{How: "late", Looks: 30, At: 60000}},
		{awaitScenario{Name: "slow answers", Steps: []lookStep{{Status: 502, Took: 1500}}},
			awaitRun{How: "late", Looks: 18, At: 63000}},
		{awaitScenario{Name: "the page is left", Steps: []lookStep{refused}, StopAt: 5000},
			awaitRun{How: "stopped", Looks: 2, At: 5000}},
		{awaitScenario{Name: "the page is left during a look", Steps: []lookStep{{Status: 502, Took: 3000}}, StopAt: 3000},
			awaitRun{How: "stopped", Looks: 1, At: 3000}},
	}
	args := struct {
		Steps     []stepCase      `json:"steps"`
		Scenarios []awaitScenario `json:"scenarios"`
	}{}
	for _, c := range stepCases {
		args.Steps = append(args.Steps, c.in)
	}
	for _, c := range scenarios {
		c.s.Want = want
		args.Scenarios = append(args.Scenarios, c.s)
	}
	arg, _ := json.Marshal(args)
	prog += "\nvar ARGS = " + string(arg) + ";\n" + awaitDriver
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(prog)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v\n%s", err, errOut.String())
	}
	var got struct {
		Steps []string   `json:"steps"`
		Runs  []awaitRun `json:"runs"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Steps) != len(stepCases) || len(got.Runs) != len(scenarios) {
		t.Fatalf("node printed %q: %v", out.String(), err)
	}
	for n, c := range stepCases {
		if got.Steps[n] != c.step {
			t.Errorf("updateStep(%d, %v, %q) = %q, want %q", c.in.Status, c.in.Box, c.in.Want, got.Steps[n], c.step)
		}
	}
	for n, c := range scenarios {
		if got.Runs[n] != c.want {
			t.Errorf("%s: %+v, want %+v", c.s.Name, got.Runs[n], c.want)
		}
	}
}
