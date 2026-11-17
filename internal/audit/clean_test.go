package audit

import (
	"strings"
	"testing"
)

// What a caller wrote is cut and scrubbed before it is recorded; Clean and
// CleanActor are how an entry's fields are made, and what anything else that
// shows an actor (a job) uses.
func TestCleanActorAsAnEntryRecordsIt(t *testing.T) {
	const secret = "esm_abcdefghijklmnopqrstuv"
	a := CleanActor(Actor{Kind: "token", ID: "tok_1", Role: "site", ActingUser: "jane " + secret + " -ServerPassword=hunter2"})
	if a.Kind != "token" || a.ID != "tok_1" || a.Role != "site" || a.ActingUser != "jane esm_******** -ServerPassword=********" {
		t.Fatalf("cleaned actor: %+v", a)
	}
	long := CleanActor(Actor{Kind: strings.Repeat("k", 400), ID: strings.Repeat("i", 400), Role: strings.Repeat("r", 400), ActingUser: strings.Repeat("u", 400)})
	for what, v := range map[string]string{"kind": long.Kind, "id": long.ID, "role": long.Role, "acting user": long.ActingUser} {
		if len(v) > MaxField {
			t.Errorf("the %s has %d bytes, at most %d", what, len(v), MaxField)
		}
	}
	if got := Clean("steam:main"); got != "steam:main" {
		t.Errorf("Clean of a plain text: %q", got)
	}
	if got := Clean(strings.Repeat("x", 300) + secret); len(got) > MaxField || strings.Contains(got, "esm_a") {
		t.Errorf("Clean of a long text with a token at its end: %q", got)
	}
	// An entry is written as it is cleaned.
	if Clean("a "+secret) != "a esm_********" {
		t.Errorf("Clean of a token: %q", Clean("a "+secret))
	}
}
