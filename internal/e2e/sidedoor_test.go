package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// Passwords have one way in: PUT .../secrets. A rule key or an extra
// argument that looks like a secret is refused on create and on change, and
// its value shows up nowhere: not in the answer, the audit log, the service
// log or the instance.
func TestSecretLikeSettingsAreRefused(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	const value = "Zq7-hunter-value"
	bodies := map[string]map[string]any{
		"rule JoinPassword":   {"rules": map[string]string{"JoinPassword": value}},
		"rule joinpassword":   {"rules": map[string]string{"Difficulty": "hard", "joinpassword": value}},
		"rule ServerPassword": {"rules": map[string]string{"ServerPassword": value}},
		"rule Rcon_Pwd":       {"rules": map[string]string{"Rcon_Pwd": value}},
		"rule RconKey":        {"rules": map[string]string{"RconKey": value}},
		"rule ApiKey":         {"rules": map[string]string{"ApiKey": value}},
		"rule AuthToken":      {"rules": map[string]string{"AuthToken": value}},
		"rule ClientSecret":   {"rules": map[string]string{"ClientSecret": value}},
		"arg RconPassword":    {"extra_args": []string{"-RconPassword=" + value}},
		"arg rcon-password":   {"extra_args": []string{"-NoSteam", "--rcon-password=" + value}},
		"arg with a colon":    {"extra_args": []string{"-RconPassword:" + value}},
		"arg with a space":    {"extra_args": []string{"-AdminKey " + value}},
		"arg AdminKey":        {"extra_args": []string{"-AdminKey=" + value}},
		"arg in a section":    {"extra_args": []string{"-Section:Game:JoinPassword=" + value}},
	}
	refused := func(what string, status int, raw json.RawMessage) {
		t.Helper()
		var eb api.ErrorBody
		json.Unmarshal(raw, &eb)
		if status != http.StatusUnprocessableEntity || eb.Error.Code != "looks_like_secret" {
			t.Errorf("%s: %d %s, want 422 looks_like_secret", what, status, raw)
		}
		if strings.Contains(string(raw), value) || strings.Contains(string(raw), "hunter") {
			t.Errorf("%s: the answer repeats the value: %s", what, raw)
		}
		if !strings.Contains(eb.Error.Message, "/secrets") {
			t.Errorf("%s: the answer does not point to the secrets call: %s", what, raw)
		}
	}
	for what, body := range bodies {
		var raw json.RawMessage
		st := h.call("PATCH", "/api/v1/instances/alpha", h.admin, body, &raw)
		refused("change with "+what, st, raw)
		create := map[string]any{"name": "beta", "bind_ip": "127.0.0.1"}
		for k, v := range body {
			create[k] = v
		}
		raw = nil
		st = h.call("POST", "/api/v1/instances", h.admin, create, &raw)
		refused("create with "+what, st, raw)
	}
	var eb api.ErrorBody
	if st := h.call("GET", "/api/v1/instances/beta", h.admin, nil, &eb); st != http.StatusNotFound {
		t.Fatalf("a refused create left an instance: %d", st)
	}
	// The command line gets the same answer.
	code, out, errOut := h.runCLI("create", "gamma", "--bind", "127.0.0.1", "--set", "JoinPassword="+value)
	if code != 1 || !strings.Contains(errOut, "looks like a secret") || strings.Contains(out+errOut, value) {
		t.Fatalf("create on the command line: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("create", "gamma", "--bind", "127.0.0.1", "--arg", "-RconPassword="+value)
	if code != 1 || !strings.Contains(errOut, "looks like a secret") || strings.Contains(out+errOut, value) {
		t.Fatalf("create on the command line: %d %q %q", code, out, errOut)
	}

	// Ordinary names pass, also ones that only resemble the words.
	var ch api.InstanceChange
	h.must(http.StatusOK, "PATCH", "/api/v1/instances/alpha", map[string]any{
		"rules":      fastRules(map[string]string{"Difficulty": "2", "Keyboard": "qwerty", "KeepInventory": "true", "Compass": "on"}),
		"extra_args": []string{"-NoSteam", "-KeyBindings=default"},
	}, &ch)
	if ch.Rules["Keyboard"] != "qwerty" || len(ch.ExtraArgs) != 2 {
		t.Fatalf("ordinary settings: %+v", ch.Instance)
	}

	// Nothing kept the value.
	var tok api.NewToken
	h.must(http.StatusCreated, "POST", "/api/v1/tokens", api.TokenRequest{Name: "viewer", Role: "read"}, &tok)
	var raw json.RawMessage
	if st := h.call("GET", "/api/v1/instances", tok.Token, nil, &raw); st != http.StatusOK {
		t.Fatalf("list with a read token: %d", st)
	}
	var list struct {
		Instances []instances.View `json:"instances"`
	}
	json.Unmarshal(raw, &list)
	if len(list.Instances) != 1 || len(list.Instances[0].Rules) != 6 {
		t.Fatalf("instances after the refusals: %s", raw)
	}
	auditLog, err := os.ReadFile(filepath.Join(h.root, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(auditLog), "looks like a secret"); n != 2*len(bodies)+2 {
		t.Errorf("the audit log has %d refusals, want %d", n, 2*len(bodies)+2)
	}
	instanceFile, _ := os.ReadFile(filepath.Join(h.root, "instances", "alpha", "instance.json"))
	for what, text := range map[string]string{
		"list for a read token": string(raw), "audit log": string(auditLog),
		"service log": h.stderr.String(), "instance.json": string(instanceFile),
	} {
		if strings.Contains(text, value) || strings.Contains(text, "hunter") {
			t.Errorf("the %s has the value of a refused setting:\n%s", what, text)
		}
	}
}
