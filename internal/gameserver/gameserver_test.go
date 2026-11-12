package gameserver

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func goldenLaunch() Launch {
	return Launch{
		Map:        "TestMap",
		GamePort:   7777,
		QueryPort:  27015,
		BindIP:     "192.0.2.10",
		MaxPlayers: 40,
		ServerName: "Echoes Test Server",
		WorldName:  "main",
		Rules: map[string]string{
			"PvP":            "true",
			"DayLength":      "3600",
			"BuildingDecay":  "false",
			"AnotherSetting": "a b c",
		},
		DataDir:     "/srv/durinn/instances/alpha/data",
		SecretsFile: "/srv/durinn/instances/alpha/run/server-secrets.json",
		Secrets:     Secrets{ServerPassword: "join-secret", AdminPassword: "admin-secret"},
		ExtraArgs:   []string{"-NoSteam", "-Extra=1 2"},
	}
}

// By default the passwords are not on the command line at all: the server
// is pointed at the secrets file with one switch.
func TestLaunchArgsGolden(t *testing.T) {
	want := []string{
		"TestMap",
		"-Port=7777",
		"-QueryPort=27015",
		"-MULTIHOME=192.0.2.10",
		"-MaxPlayers=40",
		"-ServerName=Echoes Test Server",
		"-WorldName=main",
		"-AnotherSetting=a b c",
		"-BuildingDecay=false",
		"-DayLength=3600",
		"-PvP=true",
		"-SecretsFile=/srv/durinn/instances/alpha/run/server-secrets.json",
		"-log",
		"-SavedDir=/srv/durinn/instances/alpha/data",
		"-NoSteam",
		"-Extra=1 2",
	}
	for _, delivery := range []string{"", DeliveryFile} {
		l := goldenLaunch()
		l.Delivery = delivery
		argv := l.Args()
		if !slices.Equal(argv, want) {
			t.Errorf("delivery %q: argv mismatch\n got: %q\nwant: %q", delivery, argv, want)
		}
		for _, a := range argv {
			if strings.Contains(strings.ToLower(a), "password") || strings.Contains(a, "join-secret") || strings.Contains(a, "admin-secret") {
				t.Errorf("delivery %q: a password or a password switch is on the command line: %q", delivery, a)
			}
		}
	}
	if SecretDelivery != DeliveryFile {
		t.Fatalf("the default delivery is %q", SecretDelivery)
	}
}

// With command-line delivery the passwords are switches, and the secrets
// file switch is not given.
func TestLaunchArgsCommandLine(t *testing.T) {
	l := goldenLaunch()
	l.Delivery = DeliveryCommandLine
	want := []string{
		"TestMap",
		"-Port=7777",
		"-QueryPort=27015",
		"-MULTIHOME=192.0.2.10",
		"-MaxPlayers=40",
		"-ServerName=Echoes Test Server",
		"-WorldName=main",
		"-AnotherSetting=a b c",
		"-BuildingDecay=false",
		"-DayLength=3600",
		"-PvP=true",
		"-ServerPassword=join-secret",
		"-AdminPassword=admin-secret",
		"-log",
		"-SavedDir=/srv/durinn/instances/alpha/data",
		"-NoSteam",
		"-Extra=1 2",
	}
	if argv := l.Args(); !slices.Equal(argv, want) {
		t.Errorf("argv mismatch\n got: %q\nwant: %q", argv, want)
	}
	l.Secrets = Secrets{AdminPassword: "only-admin"}
	if argv := l.Args(); !slices.Contains(argv, "-AdminPassword=only-admin") || strings.Contains(strings.Join(argv, " "), "-ServerPassword") ||
		strings.Contains(strings.Join(argv, " "), SecretsFileSwitch) {
		t.Errorf("argv with only an admin password: %q", argv)
	}
}

func TestLaunchArgsMinimal(t *testing.T) {
	argv := Launch{GamePort: 7778, QueryPort: 27016, MaxPlayers: 10, ServerName: "s", WorldName: "w", DataDir: "D"}.Args()
	want := []string{"Default", "-Port=7778", "-QueryPort=27016", "-MaxPlayers=10", "-ServerName=s", "-WorldName=w", "-log", "-SavedDir=D"}
	if !slices.Equal(argv, want) {
		t.Fatalf("got %q", argv)
	}
}

func TestReservedSwitches(t *testing.T) {
	for _, k := range []string{"Port", "-QUERYPORT", "MultiHome", "log", "SavedDir", "ServerPassword", "adminpassword", "-SecretsFile", "secretsfile"} {
		if !IsReservedSwitch(k) {
			t.Errorf("%q not reserved", k)
		}
	}
	for _, k := range []string{"PvP", "DayLength", "Ports"} {
		if IsReservedSwitch(k) {
			t.Errorf("%q reserved", k)
		}
	}
}

// The ready and world-saved lines count only when the marker starts the
// line, after nothing but the server's own time prefix. The same words
// anywhere else in a line (a repeated command line, a name, a chat message)
// never count.
func TestMarkerLinesAreAnchored(t *testing.T) {
	for _, marker := range []struct {
		name  string
		text  string
		match func(string) bool
	}{
		{"ready", ReadyMarker, IsReadyLine},
		{"world saved", WorldSavedMarker, IsWorldSavedLine},
	} {
		yes := []string{
			marker.text,
			marker.text + " (players 0/40, map M, world w)",
			marker.text + ".",
			marker.text + "\r",
			"[2026-01-01T00:00:00Z] " + marker.text,
			"[2026-01-01T00:00:00.5Z] " + marker.text + " (3)",
			"[2026-11-12T06:05:56.278390193Z] " + marker.text + " (players 0/10)",
		}
		no := []string{
			"",
			marker.text + "ing",
			marker.text + "_now",
			" " + marker.text,
			"\t" + marker.text,
			"x" + marker.text,
			"[2026-01-01T00:00:00Z]  " + marker.text,
			"[2026-01-01T00:00:00Z]" + marker.text,
			"[2026-01-01 00:00:00] " + marker.text,
			"[2026-01-01T00:00:00+01:00] " + marker.text,
			"[x] " + marker.text,
			"[] " + marker.text,
			"[2026-01-01T00:00:00Z] [2026-01-01T00:00:00Z] " + marker.text,
			"[2026-01-01T00:00:00Z] Command line: Default -ServerName=" + marker.text + " -log",
			"[2026-01-01T00:00:00Z] Command line: Default -Motd=" + marker.text,
			"Command line: -ServerPassword=" + marker.text,
			"[2026-01-01T00:00:00Z] Chat: someone: " + marker.text,
			"-Note=[2026-01-01T00:00:00Z] " + marker.text,
			strings.ToLower(marker.text),
			strings.ToUpper(marker.text),
		}
		for _, line := range yes {
			if !marker.match(line) {
				t.Errorf("%s: %q is not taken as the marker line", marker.name, line)
			}
		}
		for _, line := range no {
			if marker.match(line) {
				t.Errorf("%s: %q is taken as the marker line", marker.name, line)
			}
		}
	}
	if IsReadyLine(WorldSavedMarker) || IsWorldSavedLine(ReadyMarker) {
		t.Fatal("the two markers are mixed up")
	}
}

func TestStatusAndSaveFiles(t *testing.T) {
	data := t.TempDir()
	if _, err := ReadStatus(data); !os.IsNotExist(err) {
		t.Fatalf("missing status: %v", err)
	}
	if err := os.WriteFile(StatusFilePath(data), []byte(`{"state":"ready","pid":42,"ports":{"game":7777,"query":27015},"players":3,"version":"1.0","last_save":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ReadStatus(data)
	if err != nil || s.State != StateReady || s.PID != 42 || s.Ports.Query != 27015 || s.LastSave.IsZero() {
		t.Fatalf("%+v %v", s, err)
	}
	os.MkdirAll(filepath.Join(data, "SaveGames"), 0o700)
	os.WriteFile(filepath.Join(data, filepath.FromSlash(SaveFileRel("main"))), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(data, "SaveGames", "alt.sav"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(data, "SaveGames", "notes.txt"), []byte("x"), 0o600)
	files, err := SaveFiles(data)
	if err != nil || !slices.Equal(files, []string{"SaveGames/alt.sav", "SaveGames/main.sav"}) {
		t.Fatalf("%v %v", files, err)
	}
	if got := ServerLogPath(data); got != filepath.Join(data, "Logs", "server.log") {
		t.Fatalf("log path %q", got)
	}
}

func TestPoliteStopAndRequestFile(t *testing.T) {
	if PoliteStop("linux") != StopSignal || PoliteStop("windows") != StopFile {
		t.Fatal("stop methods wrong")
	}
	if got := StopRequestPath("data"); got != filepath.Join("data", "control", "stop") {
		t.Fatalf("stop request path %q", got)
	}
}

func TestSecretValuesAndHiding(t *testing.T) {
	argv := Launch{GamePort: 1, QueryPort: 2, MaxPlayers: 3, DataDir: "d", Delivery: DeliveryCommandLine,
		Secrets: Secrets{ServerPassword: "open sesame", AdminPassword: "abc"}}.Args()
	argv = append(argv, "-OtherPassword=", "-Note=password")
	got := SecretValues(argv)
	if !slices.Equal(got, []string{"open sesame", "abc"}) {
		t.Fatalf("secret values %q", got)
	}
	text := "systemd-run failed: -ServerPassword=open sesame -AdminPassword=abc"
	hidden := HideValues(text, got)
	if strings.Contains(hidden, "open sesame") {
		t.Fatalf("value not hidden: %q", hidden)
	}
	if !strings.Contains(hidden, "=abc") {
		t.Fatalf("a value shorter than %d bytes was replaced: %q", MinHiddenLength, hidden)
	}
}

func TestProbe(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 64)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if string(buf[:n]) == "PING" {
				pc.WriteTo([]byte("PONG"), addr)
			}
		}
	}()
	port := pc.LocalAddr().(*net.UDPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Probe(ctx, "", port); err != nil {
		t.Fatalf("probe: %v", err)
	}
	// Nothing listening: the probe fails within its deadline.
	free, _ := net.ListenPacket("udp", "127.0.0.1:0")
	freePort := free.LocalAddr().(*net.UDPAddr).Port
	free.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if err := Probe(ctx2, "127.0.0.1", freePort); err == nil {
		t.Fatal("probe of a closed port succeeded")
	}
}
