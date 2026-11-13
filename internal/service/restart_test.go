package service

import (
	"runtime"
	"testing"
)

// systemd sets INVOCATION_ID for every service it starts; on Linux that is
// what tells ESM that a service manager starts it again. Elsewhere nothing
// does.
func TestRestartedBy(t *testing.T) {
	t.Setenv("INVOCATION_ID", "0123456789abcdef0123456789abcdef")
	want := ""
	if runtime.GOOS == "linux" {
		want = "systemd"
	}
	if got := restartedBy(); got != want {
		t.Errorf("with INVOCATION_ID: %q, want %q", got, want)
	}
	t.Setenv("INVOCATION_ID", "")
	if got := restartedBy(); got != "" {
		t.Errorf("without INVOCATION_ID: %q", got)
	}
}
