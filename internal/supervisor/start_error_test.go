package supervisor

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// A failed start of a server that has passwords on its command line never
// repeats one, in any form the tools a supervisor calls may print it: as
// given, with the doubled dollars older systemd versions need, or quoted,
// with or without its switch in front.
func TestStartErrorHidesPasswordsInEveryForm(t *testing.T) {
	const pw, admin = `p$ss w$rd &"';%,=<> 42`, "adm$n-$ecret"
	spec := Spec{Name: "alpha", Args: []string{"Default", "-Port=1", "-ServerPassword=" + pw, "-AdminPassword=" + admin, "-log"},
		SecretValues: []string{pw, admin}}
	var escaped []string
	for _, a := range spec.Args {
		escaped = append(escaped, dollars(a))
	}
	echo := "Failed to start transient service unit: " + strings.Join(escaped, " ") +
		"\ninvalid argument " + strconv.Quote(dollars(pw)) +
		"\nbad value " + dollars(pw) + "; bad value " + dollars(admin)
	msg := startError(spec, errors.New(echo)).Error()
	for _, piece := range []string{"p$ss", "p$$ss", "w$rd", "w$$rd", `&"';%,=<>`, `&\"';%,=<>`, "adm$n", "adm$$n", "$ecret", "$$ecret"} {
		if strings.Contains(msg, piece) {
			t.Errorf("start error shows %q:\n%s", piece, msg)
		}
	}
	if !strings.Contains(msg, "supervisor: start alpha: ") || !strings.Contains(msg, "-Port=1") || !strings.Contains(msg, "-log") {
		t.Fatalf("start error lost its detail:\n%s", msg)
	}
}

// dollars is how older systemd versions get an argument: every "$" doubled.
func dollars(a string) string { return strings.ReplaceAll(a, "$", "$$") }

// A tool that repeats the command line it was given does not get it into
// the start error: what follows the executable is cut, as given and with
// the doubled dollars, and the rest of the tool's message stays.
func TestStartErrorCutsARepeatedCommandLine(t *testing.T) {
	spec := Spec{Name: "alpha", Executable: "/srv/builds/1.0/bin/the $erver",
		Args: []string{"Default", "-Port=7777", "-ServerName=My $erver", "-SecretsFile=/srv/run/server-secrets.json", "-log"}}
	plain := spec.Executable + " " + strings.Join(spec.Args, " ")
	var quoted []string
	for _, a := range append([]string{spec.Executable}, spec.Args...) {
		quoted = append(quoted, `"`+a+`"`)
	}
	for what, echo := range map[string]string{
		"as given":           "Failed to start transient service unit: --user --unit=esm-alpha -- " + plain + "\nSee the logs for details.",
		"with doubled $":     "Failed to start transient service unit: -- " + dollars(plain) + "\nSee the logs for details.",
		"quoted":             "Failed to start transient service unit: -- " + strings.Join(quoted, " ") + "\nSee the logs for details.",
		"twice":              "Failed to start transient service unit: " + plain + "\nagain: " + plain + "\nSee the logs for details.",
		"at the end of text": "See the logs for details.\nFailed to start transient service unit: " + plain,
	} {
		msg := startError(spec, errors.New(echo)).Error()
		for _, part := range []string{"-Port=", "-ServerName=", "-SecretsFile", "server-secrets.json", "-log", "Default"} {
			if strings.Contains(msg, part) {
				t.Errorf("%s: the start error has %q of the command line:\n%s", what, part, msg)
			}
		}
		if !strings.Contains(msg, "Failed to start transient service unit: ") || !strings.Contains(msg, "See the logs for details.") ||
			!strings.Contains(msg, "<server arguments>") || !strings.Contains(msg, "bin/the ") {
			t.Errorf("%s: the start error lost the tool's own words or the executable:\n%s", what, msg)
		}
	}
	// Without a repeated command line nothing is cut, not even where the
	// executable or an argument is named alone.
	for _, text := range []string{
		"Failed to find executable " + spec.Executable + ": No such file or directory",
		"fork/exec " + spec.Executable + ": exec format error",
		"Unknown assignment: Default",
	} {
		if msg := startError(spec, errors.New(text)).Error(); msg != "supervisor: start alpha: "+text {
			t.Errorf("start error %q, want the tool's message %q unchanged", msg, text)
		}
	}
}
