package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// A failing verb always says why, on stderr (or as the JSON error), even
// when the error it got has no text.
func TestFailAlwaysSaysSomething(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		var out, errOut bytes.Buffer
		e := &env{g: globals{json: jsonOut}, out: &out, errOut: &errOut}
		if code := e.fail(errors.New("")); code != 1 {
			t.Fatalf("exit code %d", code)
		}
		got := errOut.String()
		if jsonOut {
			got = out.String()
		}
		if !strings.Contains(got, "failed (error)") {
			t.Fatalf("json %v: %q %q", jsonOut, out.String(), errOut.String())
		}
	}
}
