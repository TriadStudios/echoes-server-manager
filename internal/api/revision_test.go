package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
)

// If-Match of a change call is read as nothing (no header, or "*"), one
// revision (one strong tag: a positive whole number without leading zeros
// in double quotes), or 400 bad_if_match: a list, a weak tag, a number
// without quotes, an empty header, one sent twice, a revision that is none
// or too long.
func TestIfMatchIsReadAsARevision(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  int64 // the revision, 0 for no condition
		bad   bool
	}{
		{name: "no header"},
		{name: "any", lines: []string{"*"}},
		{name: "any with spaces", lines: []string{" * "}},
		{name: "one", lines: []string{`"1"`}, want: 1},
		{name: "seven", lines: []string{`"7"`}, want: 7},
		{name: "spaces around", lines: []string{`  "42" `}, want: 42},
		{name: "a large one", lines: []string{`"123456789012345678"`}, want: 123456789012345678},
		{name: "too long to be one", lines: []string{`"1234567890123456789"`}, bad: true},
		{name: "none", lines: []string{`"0"`}, bad: true},
		{name: "a leading zero", lines: []string{`"07"`}, bad: true},
		{name: "negative", lines: []string{`"-7"`}, bad: true},
		{name: "not a number", lines: []string{`"abc"`}, bad: true},
		{name: "a number with a space", lines: []string{`"7 "`}, bad: true},
		{name: "no quotes", lines: []string{`7`}, bad: true},
		{name: "one quote", lines: []string{`"7`}, bad: true},
		{name: "single quotes", lines: []string{`'7'`}, bad: true},
		{name: "weak", lines: []string{`W/"7"`}, bad: true},
		{name: "a list", lines: []string{`"7", "8"`}, bad: true},
		{name: "a list with any", lines: []string{`*, "7"`}, bad: true},
		{name: "two stars", lines: []string{`**`}, bad: true},
		{name: "empty", lines: []string{``}, bad: true},
		{name: "twice", lines: []string{`"7"`, `"7"`}, bad: true},
		{name: "any and one", lines: []string{`*`, `"7"`}, bad: true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPatch, "/api/v1/instances/alpha", nil)
		for _, l := range c.lines {
			r.Header.Add(IfMatchHeader, l)
		}
		cond, err := ifMatch(r)
		switch {
		case c.bad:
			if err == nil || !fault.Is(err, fault.BadRequest) || fault.CodeOf(err) != "bad_if_match" {
				t.Errorf("%s: %q: %+v, %v, want 400 bad_if_match", c.name, c.lines, cond, err)
			}
		case err != nil || cond.Revision != c.want:
			t.Errorf("%s: %q: revision %d, %v, want %d", c.name, c.lines, cond.Revision, err, c.want)
		}
	}
	if etag(7) != `"7"` || etag(1) != `"1"` {
		t.Errorf("etag(7) = %s", etag(7))
	}
	// What the API sends as an ETag is a tag it takes as If-Match.
	for _, rev := range []int64{1, 9, 10, 1000000} {
		r := httptest.NewRequest(http.MethodPatch, "/", nil)
		r.Header.Set(IfMatchHeader, etag(rev))
		if cond, err := ifMatch(r); err != nil || cond.Revision != rev {
			t.Errorf("revision %d sent as %s is read as %d, %v", rev, etag(rev), cond.Revision, err)
		}
	}
}

// A change refused because the revision moved is 412, with its own code.
func TestPreconditionIsStatus412(t *testing.T) {
	status, code, msg := classify(fault.New(fault.Precondition, "changed", "the settings changed"))
	if status != http.StatusPreconditionFailed || code != "changed" || msg != "the settings changed" {
		t.Fatalf("classify: %d %s %q", status, code, msg)
	}
	// Without a code of its own the error has the one of its kind.
	if status, code, _ := classify(fault.New(fault.Precondition, "", "x")); status != http.StatusPreconditionFailed || code != "precondition_failed" {
		t.Fatalf("classify without a code: %d %q", status, code)
	}
}
