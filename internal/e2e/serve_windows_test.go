package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
)

const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
	detachedProcess       = 0x00000008
	ctrlCEvent            = 0
)

// ctrlCEnv makes this test binary press Ctrl+C on the console of the
// process it names, and do nothing else (see ctrlCHelper).
const ctrlCEnv = "ESM_TEST_CTRL_C_PID"

// serveAttr starts esm serve in a process group of its own, on a console
// of its own that has no window. Windows starts a process of a new group
// with Ctrl+C turned off.
func serveAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | createNoWindow}
}

// stopServe presses Ctrl+C on the console of esm serve: a copy of this test
// binary without a console attaches to that console and sends Ctrl+C to
// every process on it, as the key does. Nothing else is on that console.
func stopServe(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(exe, "-test.run=^$")
	c.Env = append(os.Environ(), ctrlCEnv+"="+strconv.Itoa(cmd.Process.Pid))
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("Ctrl+C on the console of esm serve: %v\n%s", err, out)
	}
}

// ctrlCHelper does the work of stopServe when this test binary was started
// for it, and reports the exit code.
func ctrlCHelper() (int, bool) {
	v := os.Getenv(ctrlCEnv)
	if v == "" {
		return 0, false
	}
	pid, err := strconv.Atoi(v)
	if err == nil {
		err = pressCtrlC(pid)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	return 0, true
}

// pressCtrlC sends Ctrl+C to every process on the console of pid. This
// process, attached to that console for the moment, ignores it.
func pressCtrlC(pid int) error {
	k := syscall.NewLazyDLL("kernel32.dll")
	free, attach := k.NewProc("FreeConsole"), k.NewProc("AttachConsole")
	handler, generate := k.NewProc("SetConsoleCtrlHandler"), k.NewProc("GenerateConsoleCtrlEvent")
	free.Call()
	if r, _, err := attach.Call(uintptr(pid)); r == 0 {
		return fmt.Errorf("attach to the console of process %d: %w", pid, err)
	}
	defer free.Call()
	if r, _, err := handler.Call(0, 1); r == 0 {
		return fmt.Errorf("ignore Ctrl+C: %w", err)
	}
	if r, _, err := generate.Call(ctrlCEvent, 0); r == 0 {
		return fmt.Errorf("send Ctrl+C: %w", err)
	}
	return nil
}
