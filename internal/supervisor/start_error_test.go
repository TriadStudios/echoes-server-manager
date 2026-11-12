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
