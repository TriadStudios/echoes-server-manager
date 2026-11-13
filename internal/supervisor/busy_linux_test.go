package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// A server binary that is still open for writing somewhere (a build being
// copied, or a descriptor a forked child holds for a moment) cannot be run
// on Linux: "text file busy". A start waits that out instead of failing.
func TestStartWaitsOutABusyExecutable(t *testing.T) {
	r := newRig(t)
	p := newProc(t, r.st)
	defer p.Close()
	r.cleanupStop("busy")
	exe := filepath.Join(t.TempDir(), "esm-fakeserver")
	b, err := os.ReadFile(testkit.FakeServer(t))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(exe, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.Close()
	}()
	spec := r.spec("busy")
	spec.Executable = exe
	if err := p.Start(context.Background(), spec); err != nil {
		t.Fatalf("start of a binary busy for a moment: %v", err)
	}
	waitReady(t, r.dataDir("busy"))
}
