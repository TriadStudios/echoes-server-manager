package supervisor

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// slowGuard makes the window between the "was it sent?" check and the
// record of the signal wide, for the time of a test.
func slowGuard(t *testing.T) {
	testHookBeforeStopSignal = func(string) { time.Sleep(150 * time.Millisecond) }
	t.Cleanup(func() { testHookBeforeStopSignal = nil })
}

// Stops that run at the same time, from one supervisor or two on the same
// root, still send the server exactly one signal.
func TestProcessConcurrentStopsSendOneSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no signals; the stop request file is used there")
	}
	r := newRig(t)
	p := newProc(t, r.st)
	defer p.Close()
	r.cleanupStop("stubborn")
	if err := p.Start(context.Background(), r.spec("stubborn", "-FakeIgnoreStop")); err != nil {
		t.Fatal(err)
	}
	data := r.dataDir("stubborn")
	waitReady(t, data)
	p2 := newProc(t, r.st)
	defer p2.Close()
	p2.Adopt()
	slowGuard(t)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := range 6 {
		sup := p
		if i%2 == 1 {
			sup = p2
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- sup.Stop(context.Background(), "stubborn", 700*time.Millisecond)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrStopTimeout) {
			t.Errorf("a stop: %v, want ErrStopTimeout", err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	log, _ := os.ReadFile(r.logFile("stubborn"))
	if testkit.SecondSignal(data) || strings.Count(string(log), "Ignoring stop request") != 1 {
		t.Fatalf("second signal %v, stop requests seen %d, want 1:\n%s", testkit.SecondSignal(data),
			strings.Count(string(log), "Ignoring stop request"), log)
	}
}
