package page_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The script's sessionEnded says when an API answer means the login is
// gone: a 401 except from the login and the setup, and a 429 from
// /api/v1/session (the limit on failed authentications of an address); no
// other 429. This test runs it with node, like TestSaveRulesInTheScript:
// skipped without node, except on the CI machines.
func TestSessionEndedInTheScript(t *testing.T) {
	b, err := os.ReadFile("static/esm.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	const start, end = "// sessionEnded: start", "// sessionEnded: end"
	i, j := strings.Index(script, start), strings.Index(script, end)
	if i < 0 || j < i {
		t.Fatal("the script has no sessionEnded between its markers")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("node is not installed on this CI machine, so the script's sessionEnded cannot be run")
		}
		t.Skip("node is not installed, so the script's sessionEnded is not run here")
	}
	type answer struct {
		Path   string `json:"path"`
		Status int    `json:"status"`
	}
	cases := []struct {
		in   answer
		want bool
	}{
		{answer{"/api/v1/session", 401}, true},
		{answer{"/api/v1/session", 429}, true},
		{answer{"/api/v1/instances", 401}, true},
		{answer{"/api/v1/instances/alpha/start?wait=true", 401}, true},
		{answer{"/api/v1/login", 401}, false},
		{answer{"/api/v1/setup", 401}, false},
		{answer{"/api/v1/login", 429}, false},
		{answer{"/api/v1/session/password", 429}, false},
		{answer{"/api/v1/instances/alpha/logs/download", 429}, false},
		{answer{"/api/v1/session", 403}, false},
		{answer{"/api/v1/session", 200}, false},
		{answer{"/api/v1/session", 502}, false},
	}
	var in []answer
	for _, c := range cases {
		in = append(in, c.in)
	}
	arg, _ := json.Marshal(in)
	prog := script[i:j] + "\nprocess.stdout.write(JSON.stringify(" + string(arg) + ".map(function (a) { return sessionEnded(a.path, a.status); })));\n"
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(prog)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v\n%s", err, errOut.String())
	}
	var got []bool
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node printed %q: %v", out.String(), err)
	}
	for n, c := range cases {
		if got[n] != c.want {
			t.Errorf("sessionEnded(%q, %d) = %v, want %v", c.in.Path, c.in.Status, got[n], c.want)
		}
	}
}
