//go:build linux

package cli

import (
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A signal that would end the command while a password is asked for, Ctrl-C,
// Ctrl-\ or a hang-up of the terminal among them, turns the echo back on:
// also one that arrives just as the echo goes off. (The echo is not really
// switched here, and the signal is sent to the test itself: were it not
// caught by then, it would end the test.)
func TestHiddenPromptTurnsTheEchoBackOn(t *testing.T) {
	defer func(old func(*os.File) (func(), error)) { hideInput = old }(hideInput)
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		restored := make(chan struct{}, 1)
		hideInput = func(*os.File) (func(), error) {
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			return func() { restored <- struct{}{} }, nil
		}
		e := &env{errOut: io.Discard}
		done := make(chan error, 1)
		go func() {
			_, err := e.askHidden(r, "New password: ")
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "interrupted") {
				t.Errorf("%v: %v", sig, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%v: the prompt went on", sig)
		}
		select {
		case <-restored:
		default:
			t.Errorf("%v: the echo was left off", sig)
		}
		w.Close()
		r.Close()
	}
}
