package cli

import "syscall"

var procSetConsoleCtrlHandler = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")

// takeCtrlC makes this process take Ctrl+C again. On Windows a process can
// start with Ctrl+C turned off: every process started in a new process
// group or with start /b, and every one started by a process that has it
// turned off (a program run in the background from Git Bash is one). Such
// a process never sees a Ctrl+C, so a Ctrl+C does not end it; Ctrl+Break
// still would. Turning it back on is what signal.Notify does on Linux for
// a SIGINT the process was started with ignored, and does not do here.
//
// Only this process and what it starts later change. Game servers start in
// a process group of their own, without a console (see the supervisor), so
// a Ctrl+C never reaches them either way.
func takeCtrlC() {
	// A nil handler and FALSE: handle Ctrl+C again.
	procSetConsoleCtrlHandler.Call(0, 0)
}
