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

func TestLaunchArgsGolden(t *testing.T) {
	l := Launch{
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
			"RulePassword":   "hunter22",
			"AnotherSetting": "a b c",
		},
		DataDir:        "/srv/durinn/instances/alpha/data",
		ServerPassword: "join-secret",
		AdminPassword:  "admin-secret",
		ExtraArgs:      []string{"-NoSteam", "-Extra=1 2", "-ExtraPassword=x"},
	}
	argv, masked := l.Args()
	wantArgv := []string{
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
		"-RulePassword=hunter22",
		"-ServerPassword=join-secret",
		"-AdminPassword=admin-secret",
		"-log",
		"-SavedDir=/srv/durinn/instances/alpha/data",
		"-NoSteam",
		"-Extra=1 2",
		"-ExtraPassword=x",
	}
	wantMasked := []string{
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
		"-RulePassword=********",
		"-ServerPassword=********",
		"-AdminPassword=********",
		"-log",
		"-SavedDir=/srv/durinn/instances/alpha/data",
		"-NoSteam",
		"-Extra=1 2",
		"-ExtraPassword=********",
	}
	if !slices.Equal(argv, wantArgv) {
		t.Errorf("argv mismatch\n got: %q\nwant: %q", argv, wantArgv)
	}
	if !slices.Equal(masked, wantMasked) {
		t.Errorf("masked mismatch\n got: %q\nwant: %q", masked, wantMasked)
	}
	for _, a := range masked {
		if strings.Contains(a, "secret") || strings.Contains(a, "hunter22") {
			t.Fatalf("secret in masked argv: %q", a)
		}
	}
}

func TestLaunchArgsMinimal(t *testing.T) {
	argv, masked := Launch{GamePort: 7778, QueryPort: 27016, MaxPlayers: 10, ServerName: "s", WorldName: "w", DataDir: "D"}.Args()
	want := []string{"Default", "-Port=7778", "-QueryPort=27016", "-MaxPlayers=10", "-ServerName=s", "-WorldName=w", "-log", "-SavedDir=D"}
	if !slices.Equal(argv, want) || !slices.Equal(masked, want) {
		t.Fatalf("got %q / %q", argv, masked)
	}
}

func TestReservedSwitches(t *testing.T) {
	for _, k := range []string{"Port", "-QUERYPORT", "MultiHome", "log", "SavedDir", "ServerPassword"} {
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

func TestLogPatterns(t *testing.T) {
	if !ReadyLine.MatchString("[2026-01-01T00:00:00Z] Server is ready (players 0/40)") {
		t.Fatal("ready line not matched")
	}
	if ReadyLine.MatchString("Server is readying") {
		t.Fatal("partial word matched")
	}
	if !WorldSavedLine.MatchString("[x] World saved (3)") {
		t.Fatal("saved line not matched")
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
	data := t.TempDir()
	if err := RequestStop(data); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "control", "stop")); err != nil {
		t.Fatal(err)
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
