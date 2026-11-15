//go:build !windows

package e2e

import (
	"os"
	"os/exec"
	"testing"
)

// serveAttr leaves esm serve as it is.
func serveAttr(*exec.Cmd) {}

// stopServe sends esm serve its SIGINT.
func stopServe(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
}

// ctrlCHelper is only needed on Windows.
func ctrlCHelper() (int, bool) { return 0, false }
