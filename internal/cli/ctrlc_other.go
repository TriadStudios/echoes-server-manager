//go:build !windows

package cli

// takeCtrlC has nothing to do here: signal.Notify itself takes back a
// SIGINT that the process was started with ignored (see os/signal).
func takeCtrlC() {}
