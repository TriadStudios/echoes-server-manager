package instances

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// markerSettings puts the text of marker wherever a user can put text that
// the server repeats in its log: the server name, a rule value and an
// extra argument. extra adds rules for the fake.
func markerSettings(marker string, extra map[string]string) Settings {
	rules := map[string]string{"Motd": marker}
	for k, v := range extra {
		rules[k] = v
	}
	return Settings{
		BindIP:      ptr("127.0.0.1"),
		DisplayName: ptr(marker),
		Rules:       fastRules(rules),
		ExtraArgs:   &[]string{"-Note=" + marker, "-Greeting=x " + marker + " y"},
	}
}

// waitCommandLine waits until the server has printed its command line, and
// returns that line.
func (r *rig) waitCommandLine(name string) string {
	r.t.Helper()
	var line string
	testkit.Eventually(r.t, 10*time.Second, func() bool {
		b, _ := r.st.ReadFile(serverLog(name))
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, "Command line: ") {
				line = l
			}
		}
		return line != ""
	}, "the server did not print its command line")
	return line
}

// A server counts as ready through its log only when it writes the ready
// line itself. The same words in its server name, in a rule value and in
// extra arguments, which it repeats with its command line, change nothing.
func TestReadyOnlyFromTheRealLine(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	// No status file and no query answers: only the log can show ready, and
	// the server writes the ready line only when told to.
	only := map[string]string{"FakeNoStatusFile": "true", "FakeNoQuery": "true", "FakeReadyDelay": "1h"}
	if _, err := r.m.Create(CreateRequest{Name: "alpha", Settings: markerSettings(gameserver.ReadyMarker, only)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Start(context.Background(), "alpha", false); err != nil {
		t.Fatal(err)
	}
	if line := r.waitCommandLine("alpha"); strings.Count(line, gameserver.ReadyMarker) < 4 {
		t.Fatalf("the command line does not repeat the marker text four times: %s", line)
	}
	time.Sleep(500 * time.Millisecond) // ten checks of the log
	if st, _ := r.m.Status("alpha"); st.State != Starting {
		t.Fatalf("ready from text the server only repeated: %+v", st)
	}
	data, _ := r.st.Abs(dataDir("alpha"))
	if err := testkit.WriteControl(data, testkit.ControlReady); err != nil {
		t.Fatal(err)
	}
	st := r.waitState("alpha", Running, 10*time.Second)
	if st.ReadySource != ReadyLogLine {
		t.Fatalf("ready source %q, want the log line", st.ReadySource)
	}
}

// Without a status file, a ready line or a query answer a server never
// becomes ready, whatever its command line says.
func TestRepeatedMarkerNeverMakesReady(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.ReadyTimeout = config.Duration(1500 * time.Millisecond) })
	r.install(testkit.BuildOptions{Version: "1.0"})
	never := map[string]string{"FakeNoStatusFile": "true", "FakeNoReady": "true"}
	if _, err := r.m.Create(CreateRequest{Name: "alpha", Settings: markerSettings(gameserver.ReadyMarker, never)}); err != nil {
		t.Fatal(err)
	}
	_, err := r.m.Start(context.Background(), "alpha", true)
	if fault.CodeOf(err) != "not_ready" {
		t.Fatalf("start: %v", err)
	}
	if line := r.waitCommandLine("alpha"); !strings.Contains(line, gameserver.ReadyMarker) {
		t.Fatalf("the command line does not repeat the marker text: %s", line)
	}
	if st, _ := r.m.Status("alpha"); st.State != Failed || st.ReadySource != "" {
		t.Fatalf("after the ready timeout: %+v", st)
	}
}

// A backup waits for the server's own world-saved line. The same words in
// the command line, printed again while the backup waits, do not count.
func TestWorldSavedOnlyFromTheRealLine(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.BackupSaveWait = config.Duration(1500 * time.Millisecond) })
	r.install(testkit.BuildOptions{Version: "1.0"})
	noSaves := map[string]string{"FakeSaveInterval": "1h"}
	if _, err := r.m.Create(CreateRequest{Name: "alpha", Settings: markerSettings(gameserver.WorldSavedMarker, noSaves)}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := r.m.Start(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	r.waitCommandLine("alpha")
	data, _ := r.st.Abs(dataDir("alpha"))
	done := make(chan Backup, 1)
	go func() {
		b, err := r.m.Backup(ctx, "alpha", ReasonManual)
		if err != nil {
			t.Error(err)
		}
		done <- b
	}()
	time.Sleep(300 * time.Millisecond) // the backup is waiting now
	if err := testkit.WriteControl(data, testkit.ControlCommandLine); err != nil {
		t.Fatal(err)
	}
	b := <-done
	if b.SaveConfirmed {
		t.Fatal("the backup took the repeated command line for a world save")
	}
	log, _ := r.st.ReadFile(serverLog("alpha"))
	if strings.Count(string(log), "Command line: ") != 2 {
		t.Fatalf("the command line was not printed again while the backup waited:\n%s", log)
	}
}
