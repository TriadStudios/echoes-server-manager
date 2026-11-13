package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// A stop from outside ESM that the server outlasts: systemd must not send
// the kill signal again when some unit timeout runs out (it would, with a
// TimeoutStopSec on the unit, even with SendSIGKILL=no), and a stop by ESM
// while the unit is being stopped sends nothing either.
func TestSystemdOutsideStopPastTimeoutIsOneSignal(t *testing.T) {
	needSystemd(t)
	r := newRig(t)
	name := uniqueName("sdlong")
	cleanupUnit(t, name)
	s := newSystemd(t, r)
	if err := s.Start(context.Background(), r.spec(name, "-FakeIgnoreStop")); err != nil {
		t.Fatal(err)
	}
	data := r.dataDir(name)
	st := waitReady(t, data)
	if out, err := exec.Command("systemctl", "--user", "stop", "--no-block", unitName(name)).CombinedOutput(); err != nil {
		t.Fatalf("systemctl stop: %v\n%s", err, out)
	}
	// Longer than any stop timeout the tests have used for units.
	time.Sleep(6500 * time.Millisecond)
	if testkit.SecondSignal(data) {
		t.Fatal("the server got a second stop signal from systemd")
	}
	if pr, _ := s.Status(name); !pr.Running || pr.PID != st.PID {
		t.Fatalf("the server is gone: %+v", pr)
	}
	if err := s.Stop(context.Background(), name, 500*time.Millisecond); !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("ESM's stop of a unit being stopped: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	log, _ := os.ReadFile(r.logFile(name))
	if testkit.SecondSignal(data) || strings.Count(string(log), "Ignoring stop request") != 1 {
		t.Fatalf("second signal %v, stop requests seen %d, want 1:\n%s", testkit.SecondSignal(data),
			strings.Count(string(log), "Ignoring stop request"), log)
	}
}

// Stops that run at the same time, from two supervisors on the same root,
// send the unit's server one signal between them.
func TestSystemdConcurrentStopsSendOneSignal(t *testing.T) {
	needSystemd(t)
	r := newRig(t)
	name := uniqueName("sdconc")
	cleanupUnit(t, name)
	s := newSystemd(t, r)
	if err := s.Start(context.Background(), r.spec(name, "-FakeIgnoreStop")); err != nil {
		t.Fatal(err)
	}
	data := r.dataDir(name)
	waitReady(t, data)
	s2 := newSystemd(t, r)
	slowGuard(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := range 4 {
		sup := s
		if i%2 == 1 {
			sup = s2
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- sup.Stop(context.Background(), name, 700*time.Millisecond)
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
	log, _ := os.ReadFile(r.logFile(name))
	if testkit.SecondSignal(data) || strings.Count(string(log), "Ignoring stop request") != 1 {
		t.Fatalf("second signal %v, stop requests seen %d, want 1:\n%s", testkit.SecondSignal(data),
			strings.Count(string(log), "Ignoring stop request"), log)
	}
}

// After ESM's stop has sent its signal and run out of time, a stop from
// outside ESM sends nothing more: ESM stops the unit the same way systemd
// does, so the unit knows it is being stopped.
func TestSystemdOutsideStopAfterESMStopIsOneSignal(t *testing.T) {
	needSystemd(t)
	r := newRig(t)
	name := uniqueName("sdafter")
	cleanupUnit(t, name)
	s := newSystemd(t, r)
	if err := s.Start(context.Background(), r.spec(name, "-FakeIgnoreStop")); err != nil {
		t.Fatal(err)
	}
	data := r.dataDir(name)
	st := waitReady(t, data)
	if err := s.Stop(context.Background(), name, 500*time.Millisecond); !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("ESM's stop of a server that ignores it: %v", err)
	}
	if out, err := exec.Command("systemctl", "--user", "stop", "--no-block", unitName(name)).CombinedOutput(); err != nil {
		t.Fatalf("systemctl stop: %v\n%s", err, out)
	}
	time.Sleep(time.Second)
	log, _ := os.ReadFile(r.logFile(name))
	if testkit.SecondSignal(data) || strings.Count(string(log), "Ignoring stop request") != 1 {
		t.Fatalf("second signal %v, stop requests seen %d, want 1:\n%s", testkit.SecondSignal(data),
			strings.Count(string(log), "Ignoring stop request"), log)
	}
	if pr, _ := s.Status(name); !pr.Running || pr.PID != st.PID {
		t.Fatalf("the server is gone: %+v", pr)
	}
}
