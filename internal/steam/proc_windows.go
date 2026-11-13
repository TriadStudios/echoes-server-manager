package steam

import "os/exec"

// ownGroup leaves the command as it is on Windows: ending the run kills
// steamcmd.exe.
func ownGroup(cmd *exec.Cmd) {}
