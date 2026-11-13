package instances

import (
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// The Steam server id of a run is the first one its log names: a later id
// line (another run appended, or text made to look like one) never
// replaces it, whether the log is followed or read back after an ESM
// restart.
func TestServerIDIsTheFirstOfTheRun(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	stamp := "[2026.01.02-03.04.05:678][  0]"
	log := stamp + "LogExample: starting\n" +
		stamp + "LogOnline: " + gameserver.ServerIDLine("90000000000000001") + "\n" +
		stamp + "LogOnline: " + gameserver.ServerIDLine("90000000000000002") + "\n"
	if err := r.st.WriteFile(serverLog("alpha"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := r.m.findServerID("alpha"); got != "90000000000000001" {
		t.Fatalf("read back: %q", got)
	}
	in, err := r.m.get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	r.m.followLogs(in, &watchState{server: tail{rel: serverLog("alpha")}, console: tail{rel: consoleFile("alpha")}})
	in.mu.Lock()
	got := in.serverID
	in.mu.Unlock()
	if got != "90000000000000001" {
		t.Fatalf("followed: %q", got)
	}
}
