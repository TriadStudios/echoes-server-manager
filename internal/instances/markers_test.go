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

// bothDeliveries runs a test once for each way a server gets its passwords.
func bothDeliveries(t *testing.T, run func(t *testing.T, delivery string)) {
	for _, delivery := range []string{gameserver.DeliveryFile, gameserver.DeliveryCommandLine} {
		t.Run(delivery, func(t *testing.T) { run(t, delivery) })
	}
}

// createWithMarker makes the instance "alpha" with the text of marker
// wherever a user can put text that the server may repeat in its log: the
// server name, a rule value, extra arguments, and both passwords (which a
// server with command-line delivery repeats too). extra adds rules for the
// fake.
func (r *rig) createWithMarker(marker, delivery string, extra map[string]string) {
	r.t.Helper()
	rules := map[string]string{"Motd": marker}
	for k, v := range extra {
		rules[k] = v
	}
	if _, err := r.m.Create(CreateRequest{Name: "alpha", Settings: Settings{
		BindIP:         ptr("127.0.0.1"),
		DisplayName:    ptr(marker),
		Rules:          fastRules(rules),
		ExtraArgs:      &[]string{"-Note=" + marker, "-Greeting=x " + marker + " y"},
		SecretDelivery: ptr(delivery),
	}}); err != nil {
		r.t.Fatal(err)
	}
	// The passwords hold the marker too; they may not occur in a setting,
	// so they carry one more character.
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr(marker + "!"), AdminPassword: ptr(marker + "!")}); err != nil {
		r.t.Fatal(err)
	}
}

// waitCommandLine waits until the server has printed its command line, and
// returns that line. It must repeat marker: four times, and twice more with
// command-line delivery, for the two passwords.
func (r *rig) waitCommandLine(name, marker, delivery string) string {
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
	want := 4
	if delivery == gameserver.DeliveryCommandLine {
		want = 6
	}
	if got := strings.Count(line, marker); got != want {
		r.t.Fatalf("the command line repeats the marker text %d times, want %d: %s", got, want, line)
	}
	return line
}

// A server counts as ready through its log only when it writes the ready
// line itself. The same words in its server name, in a rule value, in extra
// arguments and in its passwords, which it repeats with its command line,
// change nothing.
func TestReadyOnlyFromTheRealLine(t *testing.T) {
	bothDeliveries(t, func(t *testing.T, delivery string) {
		r := newRig(t, nil)
		r.install(testkit.BuildOptions{Version: "1.0"})
		// No status file and no query answers: only the log can show ready,
		// and the server writes the ready line only when told to.
		r.createWithMarker(gameserver.ReadyMarker, delivery,
			map[string]string{"FakeNoStatusFile": "true", "FakeNoQuery": "true", "FakeReadyDelay": "1h"})
		if _, err := r.m.Start(context.Background(), "alpha", false); err != nil {
			t.Fatal(err)
		}
		r.waitCommandLine("alpha", gameserver.ReadyMarker, delivery)
		time.Sleep(500 * time.Millisecond) // ten checks of the log
		if st, _ := r.m.Status("alpha"); st.State != Starting {
			t.Fatalf("ready from text the server only repeated: %+v", st)
		}
		data, _ := r.st.Abs(dataDir("alpha"))
		if err := testkit.WriteControl(data, testkit.ControlReady); err != nil {
			t.Fatal(err)
		}
		st := r.waitState("alpha", Running, 10*time.Second)
		if st.ReadySource != ReadyLogLine || st.Reason != "ready (log_line)" {
			t.Fatalf("ready source %q, reason %q; want the log line", st.ReadySource, st.Reason)
		}
	})
}

// Without a status file, a ready line or a query answer a server never
// becomes ready, whatever its command line says.
func TestRepeatedMarkerNeverMakesReady(t *testing.T) {
	bothDeliveries(t, func(t *testing.T, delivery string) {
		r := newRig(t, func(c *config.Config) { c.ReadyTimeout = config.Duration(1500 * time.Millisecond) })
		r.install(testkit.BuildOptions{Version: "1.0"})
		r.createWithMarker(gameserver.ReadyMarker, delivery, map[string]string{"FakeNoStatusFile": "true", "FakeNoReady": "true"})
		_, err := r.m.Start(context.Background(), "alpha", true)
		if fault.CodeOf(err) != "not_ready" {
			t.Fatalf("start: %v", err)
		}
		r.waitCommandLine("alpha", gameserver.ReadyMarker, delivery)
		if st, _ := r.m.Status("alpha"); st.State != Failed || st.ReadySource != "" || !strings.HasPrefix(st.Reason, "not ready within") {
			t.Fatalf("after the ready timeout: %+v", st)
		}
	})
}

// A backup waits for the server's own world-saved line. The same words in
// the command line, printed again while the backup waits, do not count.
func TestWorldSavedOnlyFromTheRealLine(t *testing.T) {
	bothDeliveries(t, func(t *testing.T, delivery string) {
		r := newRig(t, func(c *config.Config) { c.BackupSaveWait = config.Duration(1500 * time.Millisecond) })
		r.install(testkit.BuildOptions{Version: "1.0"})
		r.createWithMarker(gameserver.WorldSavedMarker, delivery, map[string]string{"FakeSaveInterval": "1h"})
		ctx := context.Background()
		if _, err := r.m.Start(ctx, "alpha", true); err != nil {
			t.Fatal(err)
		}
		r.waitCommandLine("alpha", gameserver.WorldSavedMarker, delivery)
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
	})
}
