package page_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// saveField is a rule field of the settings form as the script's
// saveRules takes it.
type saveField struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Game    bool   `json:"game"`
	Dropped bool   `json:"dropped"`
}

// The rules a save of the settings screen sends (the API replaces the whole
// map): a game setting left empty is not set and left out; every other
// rule goes as it is unless it is removed, also one stored empty. The
// script decides this in saveRules alone, which uses nothing of the page;
// this test runs it with node. Without node it is skipped, except on the
// CI machines (GITHUB_ACTIONS=true), which have node: there it fails.
func TestSaveRulesInTheScript(t *testing.T) {
	b, err := os.ReadFile("static/esm.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	const start, end = "// saveRules: start", "// saveRules: end"
	i, j := strings.Index(script, start), strings.Index(script, end)
	if i < 0 || j < i {
		t.Fatal("the script has no saveRules between its markers")
	}
	if strings.Count(script, "b.rules = saveRules(") != 1 || strings.Contains(script, "rules[key] = ") {
		t.Fatal("the settings form does not leave the rules it sends to saveRules")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("node is not installed on this CI machine, so the script's saveRules cannot be run")
		}
		t.Skip("node is not installed, so the script's saveRules is not run here")
	}
	cases := []struct {
		fields []saveField
		want   map[string]string
	}{
		{[]saveField{{"HarvestRate", "3", true, false}, {"PvP", "", true, false}, {"Difficulty", "2", true, false}},
			map[string]string{"HarvestRate": "3", "Difficulty": "2"}},
		{[]saveField{{"Banner", "free", false, false}, {"Old", "x", false, true}, {"Flag", "", false, false}},
			map[string]string{"Banner": "free", "Flag": ""}},
		{[]saveField{{"MOTD", "", true, false}, {"ChatFilter", "", true, false}}, map[string]string{}},
		{[]saveField{{"MOTD", "Hello, traveller", true, false}, {"Gone", "", false, true}}, map[string]string{"MOTD": "Hello, traveller"}},
	}
	var in [][]saveField
	for _, c := range cases {
		in = append(in, c.fields)
	}
	arg, _ := json.Marshal(in)
	prog := script[i:j] + "\nprocess.stdout.write(JSON.stringify(" + string(arg) + ".map(saveRules)));\n"
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(prog)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v\n%s", err, errOut.String())
	}
	var got []map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node printed %q: %v", out.String(), err)
	}
	for n, c := range cases {
		if !reflect.DeepEqual(got[n], c.want) {
			t.Errorf("case %d: sends %v, want %v", n+1, got[n], c.want)
		}
	}
}
