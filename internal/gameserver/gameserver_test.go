package gameserver

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func goldenLaunch() Launch {
	return Launch{
		Map:        "TestMap",
		GamePort:   7777,
		QueryPort:  27015,
		MaxPlayers: 40,
		ServerName: "Echoes Test Server",
		WorldName:  "main",
		Listed:     true,
		Rules: map[string]string{
			"PvP":            "true",
			"DayLength":      "3600",
			"BuildingDecay":  "false",
			"AnotherSetting": "a b c",
		},
		UserDir:     "/srv/durinn/instances/alpha/data",
		LogFile:     "/srv/durinn/instances/alpha/logs/server.log",
		SecretsFile: "/srv/durinn/instances/alpha/run/server-secrets.json",
		Secrets:     Secrets{ServerPassword: "join-secret", AdminPassword: "admin-secret"},
		ExtraArgs:   []string{"-NoSound", "-Extra=1 2"},
	}
}

// The fixed part of every command line, in the order a real server was
// started with.
var goldenHead = []string{
	"TestMap",
	"-unattended",
	"-Port=7777",
	"-QueryPort=27015",
	"-ServerName=Echoes Test Server",
	"-WorldName=main",
	"-MaxPlayers=40",
	"-ListInBrowser=True",
	"-UserDir=/srv/durinn/instances/alpha/data",
	"-abslog=/srv/durinn/instances/alpha/logs/server.log",
	"-AnotherSetting=a b c",
	"-BuildingDecay=false",
	"-DayLength=3600",
	"-PvP=true",
}

// By default the passwords are not on the command line at all: the server
// is pointed at the secrets file with one switch.
func TestLaunchArgsGolden(t *testing.T) {
	want := append(slices.Clone(goldenHead),
		"-SecretsFile=/srv/durinn/instances/alpha/run/server-secrets.json",
		"-NoSound",
		"-Extra=1 2",
	)
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
	want := append(slices.Clone(goldenHead),
		"-Password=join-secret",
		"-AdminPassword=admin-secret",
		"-NoSound",
		"-Extra=1 2",
	)
	if argv := l.Args(); !slices.Equal(argv, want) {
		t.Errorf("argv mismatch\n got: %q\nwant: %q", argv, want)
	}
	l.Secrets = Secrets{AdminPassword: "only-admin"}
	if argv := l.Args(); !slices.Contains(argv, "-AdminPassword=only-admin") || strings.Contains(strings.Join(argv, " "), "-Password") ||
		strings.Contains(strings.Join(argv, " "), SecretsFileSwitch) {
		t.Errorf("argv with only an admin password: %q", argv)
	}
}

// Exactly the fixed switch set, nothing more: no bind address switch ever,
// no -log, and an unlisted server says so.
func TestLaunchArgsMinimal(t *testing.T) {
	argv := Launch{Map: "M", GamePort: 7778, QueryPort: 27016, MaxPlayers: 10, ServerName: "s", WorldName: "w", UserDir: "D", LogFile: "L"}.Args()
	want := []string{"M", "-unattended", "-Port=7778", "-QueryPort=27016", "-ServerName=s", "-WorldName=w", "-MaxPlayers=10",
		"-ListInBrowser=False", "-UserDir=D", "-abslog=L"}
	if !slices.Equal(argv, want) {
		t.Fatalf("got %q", argv)
	}
	for _, a := range goldenLaunch().Args() {
		if strings.Contains(strings.ToUpper(a), "MULTIHOME") || a == "-log" {
			t.Fatalf("%q is on the command line", a)
		}
	}
}

func TestReservedAndRefusedSwitches(t *testing.T) {
	for _, k := range []string{"Port", "-QUERYPORT", "ServerName", "worldname", "MaxPlayers", "unattended", "-ListInBrowser",
		"UserDir", "-abslog", "Password", "adminpassword", "-SecretsFile", "secretsfile"} {
		if !IsReservedSwitch(k) {
			t.Errorf("%q not reserved", k)
		}
	}
	for _, k := range []string{"PvP", "DayLength", "Ports", "log", "SavedDir"} {
		if IsReservedSwitch(k) {
			t.Errorf("%q reserved", k)
		}
	}
	for _, k := range []string{"MULTIHOME", "-multihome", "--MultiHome"} {
		if why, ok := RefusedSwitch(k); !ok || !strings.Contains(why, "never passed") {
			t.Errorf("%q not refused (%q)", k, why)
		}
	}
	for _, k := range []string{"ini", "-INI", "-iniFile", "IniFile"} {
		if why, ok := RefusedSwitch(k); !ok || !strings.Contains(why, "are refused") {
			t.Errorf("%q not refused (%q)", k, why)
		}
	}
	for _, k := range []string{"Port", "Initial", "Mini", "NoIni"} {
		if _, ok := RefusedSwitch(k); ok {
			t.Errorf("%q refused", k)
		}
	}
	for _, k := range []string{"AnnouncementsToken", "-problemreporttoken"} {
		if !IsReservedSwitch(k) {
			t.Errorf("%q not reserved", k)
		}
	}
}

// -CmdLineFile= is refused like the overrides of the configuration files.
func TestCommandLineFileIsARefusedSwitch(t *testing.T) {
	for _, k := range []string{"CmdLineFile", "-CmdLineFile", "cmdlinefile", "-CMDLINEFILE"} {
		if why, ok := RefusedSwitch(k); !ok || !strings.Contains(why, "is refused") {
			t.Errorf("%q not refused (%q)", k, why)
		}
	}
	for _, k := range []string{"CmdLine", "File", "CmdLineFiles", "LineFile"} {
		if _, ok := RefusedSwitch(k); ok {
			t.Errorf("%q refused", k)
		}
	}
}

// Made-up lines of the real shape: the engine's time and frame prefix,
// then a category, then the message.
const (
	stamp  = "[2026.01.02-03.04.05:678][  0]"
	stamp2 = "[2026.01.02-03.04.05:678][1234]"
)

// The log lines count only when their words start the message, after
// nothing but the engine's prefix. The same words anywhere else in a line
// (a repeated command line, a name, a chat message) never count.
func TestLogLinesAreAnchored(t *testing.T) {
	ready := ReadyLine("Example Server", "TestMap", 4)
	saved := WorldSavedLine("Example/TestMap__main.json", 2048)
	for _, marker := range []struct {
		name  string
		text  string
		match func(string) bool
	}{
		{"ready", ready, IsReadyLine},
		{"world saved", saved, IsWorldSavedLine},
		{"flushed", FlushedLine, IsWorldSavedLine},
		{"flushed at exit", "[EXIT] Synchronous write after the flush succeeded.", IsWorldSavedLine},
		{"resumed", ResumedLine("main") + " (slot TestMap__main)", IsWorldResumedLine},
	} {
		yes := []string{
			stamp + "LogExample: " + marker.text,
			stamp + "LogExample: " + marker.text + "\r",
			stamp2 + "LogExample: Display: " + marker.text,
			stamp + "Log_Example2: Warning: " + marker.text,
			stamp + "LogExample: VeryVerbose: " + marker.text,
			stamp + "LogExample: Log: " + marker.text,
		}
		no := []string{
			"",
			marker.text,
			" " + marker.text,
			"LogExample: " + marker.text,
			stamp + marker.text,
			stamp + " LogExample: " + marker.text,
			stamp + "LogExample:  " + marker.text,
			stamp + "LogExample:" + marker.text,
			stamp + "LogExample: TAG: " + marker.text,
			stamp + "LogExample: Display: Display: " + marker.text,
			stamp + "LogExample: display: " + marker.text,
			stamp + "LogA: LogB: " + marker.text,
			stamp + "9Log: " + marker.text,
			"[2026-01-01T00:00:00Z] " + marker.text,
			"[x][1]LogExample: " + marker.text,
			"[2026.01.02-03.04.05][  0]LogExample: " + marker.text,
			stamp + stamp + "LogExample: " + marker.text,
			stamp + "LogInit: Command line: TestMap -ServerName=" + marker.text,
			stamp + "LogInit: Command line: TestMap -Motd=" + marker.text,
			"Command line: -ServerPassword=" + marker.text,
			// Chat and other lines that repeat what a player typed.
			stamp + "LogChat: Bob: " + marker.text,
			stamp + "LogChat: Display: Bob: " + marker.text,
			stamp + "LogTemp: Display: [CHAT] Bob: " + marker.text,
			stamp + "LogTemp: [CHAT] " + marker.text,
			stamp + "LogNet: Login request: ?Name=" + marker.text,
			stamp + "LogTemp: Display: Player '" + marker.text + "' joined",
			"-Note=" + stamp + "LogExample: " + marker.text,
			stamp + "LogExample: " + strings.ToLower(marker.text),
			stamp + "LogExample: " + strings.ToUpper(marker.text),
		}
		for _, line := range yes {
			if !marker.match(line) {
				t.Errorf("%s: %q is not taken as the line", marker.name, line)
			}
		}
		for _, line := range no {
			if marker.match(line) {
				t.Errorf("%s: %q is taken as the line", marker.name, line)
			}
		}
	}
	// The lines are told apart.
	others := map[string]string{"ready": ready, "saved": saved, "flushed": FlushedLine, "id": ServerIDLine("1"),
		"resumed": ResumedLine("w"), "new": NewWorldLine("w")}
	for name, text := range others {
		l := stamp + "LogExample: " + text
		if name == "id" {
			l = stamp + ServerIDCategory + ": " + text
		}
		if IsReadyLine(l) != (name == "ready") || IsWorldSavedLine(l) != (name == "saved" || name == "flushed") ||
			IsWorldResumedLine(l) != (name == "resumed") {
			t.Errorf("%s line %q is mistaken for another", name, l)
		}
		if _, ok := ServerID(l); ok != (name == "id") {
			t.Errorf("%s line %q is mistaken for the logon line", name, l)
		}
	}
	// Near misses of each line.
	for _, l := range []string{
		"[SESSION] Dedicated session 'x' was not registered",
		"[SESSION] Dedicated session registration failed",
		"[SESSION] Dedicated session 'x' registered",
		"Wrote saved games",
		"Wrote save",
		"[SHUTDOWN] Synchronous write after the flush failed.",
		"Synchronous write after the flush succeeded.",
		"[CHAT] Synchronous write after the flush succeeded.",
		"[A] [B] Wrote save x (1 bytes)",
		"[CHAT] Wrote save x (1 bytes)",
		"Steam game server logged on with UID: ",
		"Steam game server logged on with UID: abc",
		"Steam game server logged on with UID: 123456789012345678901",
		"Steam game server logged on with UID: 12x",
		"[SERVER] New world 'x'",
	} {
		line := stamp + "LogExample: " + l
		_, id := ServerID(line)
		if IsReadyLine(line) || IsWorldSavedLine(line) || id || IsWorldResumedLine(line) {
			t.Errorf("%q is taken for a line it is not", l)
		}
	}
}

// The logon line comes from the online subsystem: the LogOnline category,
// then its tag, then the words. Category and tag are pinned, so text a
// player types (after their name in a chat line) never matches.
func TestServerID(t *testing.T) {
	for line, want := range map[string]string{
		stamp + "LogOnline: STEAM: Steam game server logged on with UID: 90000000000000001": "90000000000000001",
		stamp2 + "LogOnline: STEAM: Steam game server logged on with UID: 42 (anonymous)":   "42",
		stamp + "LogOnline: Display: STEAM: Steam game server logged on with UID: 123\r":    "123",
		stamp + "LogOnline: Verbose: OSS: Steam game server logged on with UID: 7":          "7",
		stamp + ServerIDCategory + ": " + ServerIDLine("90000000000000002"):                 "90000000000000002",
	} {
		if got, ok := ServerID(line); !ok || got != want {
			t.Errorf("ServerID(%q) = %q, %v; want %q", line, got, ok, want)
		}
	}
	for _, line := range []string{
		"LogOnline: STEAM: Steam game server logged on with UID: 1",
		"Steam game server logged on with UID: 1",
		stamp + "LogOnline: Steam game server logged on with UID: 1",
		stamp + "LogExample: STEAM: Steam game server logged on with UID: 1",
		stamp + "LogChat: Bob: STEAM: Steam game server logged on with UID: 1",
		stamp + "LogChat: Display: Bob: STEAM: Steam game server logged on with UID: 1",
		stamp + "LogOnline: Bob: STEAM: Steam game server logged on with UID: 1",
		stamp + "LogOnline: STEAM: Bob: Steam game server logged on with UID: 1",
		stamp + "LogOnline: Display: TAG: Steam game server logged on with UID: 1",
		stamp + "LogOnline: STEAM:  Steam game server logged on with UID: 1",
		stamp + "LogOnline: STEAM: Steam game server logged on with UID: ",
		stamp + "LogOnline: STEAM: Steam game server logged on with UID: 12x",
		stamp + "LogOnline: STEAM: Steam game server logged on with UID: 123456789012345678901",
		stamp + "LogInit: Command line: -Motd=STEAM: Steam game server logged on with UID: 1",
	} {
		if id, ok := ServerID(line); ok {
			t.Errorf("ServerID(%q) = %q", line, id)
		}
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
	if files, err := SaveFiles(data); err != nil || len(files) != 0 {
		t.Fatalf("no save folder: %v %v", files, err)
	}
	saves := filepath.Join(data, filepath.FromSlash(SaveDirRel))
	write := func(rel string) {
		p := filepath.Join(saves, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("Example/TestMap__main.json")
	write("Example/TestMap__main.json.bak1")
	write("Example/TestMap__main.meta.json")
	write("top.sav")
	os.WriteFile(filepath.Join(data, "notes.txt"), []byte("x"), 0o600)
	os.MkdirAll(filepath.Join(data, "Saved", "Logs"), 0o700)
	os.WriteFile(filepath.Join(data, "Saved", "Logs", "x.log"), []byte("x"), 0o600)
	files, err := SaveFiles(data)
	want := []string{"Saved/SaveGames/Example/TestMap__main.json", "Saved/SaveGames/Example/TestMap__main.json.bak1",
		"Saved/SaveGames/Example/TestMap__main.meta.json", "Saved/SaveGames/top.sav"}
	if err != nil || !slices.Equal(files, want) {
		t.Fatalf("%v %v", files, err)
	}
	for _, f := range want {
		if !IsSaveFile(f) {
			t.Errorf("%q is not a save file", f)
		}
	}
	for _, f := range []string{"", "Saved/SaveGames", "Saved/SaveGames/", "Saved/Logs/x.log", "Saved/SaveGames/../x", "Saved/SaveGames/a//b",
		"/Saved/SaveGames/a", `Saved/SaveGames/a\b`, "Saved/SaveGames/c:d", "notes.txt", "saved/savegames/a"} {
		if IsSaveFile(f) {
			t.Errorf("%q is a save file", f)
		}
	}
	if got := ServerLogPath("inst"); got != filepath.Join("inst", "logs", "server.log") {
		t.Fatalf("log path %q", got)
	}
	if got := DataDirPath("inst"); got != filepath.Join("inst", "data") {
		t.Fatalf("data path %q", got)
	}
}

func TestPoliteStopAndRequestFile(t *testing.T) {
	if PoliteStop("linux") != StopInterrupt || PoliteStop("windows") != StopFile || StopInterrupt.String() != "SIGINT" {
		t.Fatal("stop methods wrong")
	}
	if got := StopRequestPath("data"); got != filepath.Join("data", "control", "stop") {
		t.Fatalf("stop request path %q", got)
	}
}

func TestLibraryPath(t *testing.T) {
	if got := LibraryPath("/r/steam/linux64", ""); got != "/r/steam/linux64" {
		t.Fatalf("%q", got)
	}
	if got := LibraryPath("/r/steam/linux64", "/opt/lib"); got != "/r/steam/linux64:/opt/lib" {
		t.Fatalf("%q", got)
	}
}

// Probes of a loopback address send from that loopback address, never from
// all interfaces.
func TestLoopbackLocalAddr(t *testing.T) {
	for host, want := range map[string]string{"127.0.0.1": "127.0.0.1:0", "::1": "[::1]:0", "127.0.0.9": "127.0.0.9:0"} {
		if got := LoopbackLocalAddr(host); got == nil || got.String() != want {
			t.Errorf("LoopbackLocalAddr(%q) = %v, want %s", host, got, want)
		}
	}
	for _, host := range []string{"", "192.0.2.10", "localhost", "2001:db8::1"} {
		if got := LoopbackLocalAddr(host); got != nil {
			t.Errorf("LoopbackLocalAddr(%q) = %v, want nil", host, got)
		}
	}
}

// gameFind finds "key=" in a command line the way the server's switch
// reader does: without regard to case, and only where the character before
// the key is not a letter or digit. It returns the value up to the next
// space (every value here is plain).
func gameFind(cmdline, key string) (string, bool) {
	low, k := strings.ToLower(cmdline), strings.ToLower(key)+"="
	for i := 0; ; {
		j := strings.Index(low[i:], k)
		if j < 0 {
			return "", false
		}
		at := i + j
		if c := low[at-1]; !('a' <= c && c <= 'z' || '0' <= c && c <= '9') {
			v, _, _ := strings.Cut(cmdline[at+len(k):], " ")
			return v, true
		}
		i = at + 1
	}
}

// With command-line delivery each password and token is found by the
// switch name the server reads, and no other: the join password is
// "Password=", which "ServerPassword=" does not match (a letter before it).
func TestSecretSwitchesAreTheServers(t *testing.T) {
	l := goldenLaunch()
	l.Delivery = DeliveryCommandLine
	l.Secrets = Secrets{ServerPassword: "join-secret", AdminPassword: "admin-secret", AnnouncementsToken: "ann-token-1", ProblemReportToken: "rep-token-1"}
	l.ExtraArgs = nil
	cmdline := " " + strings.Join(l.Args(), " ")
	for key, want := range map[string]string{"Password": "join-secret", "AdminPassword": "admin-secret",
		"AnnouncementsToken": "ann-token-1", "ProblemReportToken": "rep-token-1"} {
		if got, ok := gameFind(cmdline, key); !ok || got != want {
			t.Errorf("the server reads %s= as %q (found %v), want %q", key, got, ok, want)
		}
	}
	l.Secrets = Secrets{AdminPassword: "admin-secret"}
	if got, ok := gameFind(" "+strings.Join(l.Args(), " "), "Password"); ok {
		t.Errorf("with only an admin password the server reads a join password %q", got)
	}
	if !IsReservedSwitch("Password") || !IsReservedSwitch("-password") {
		t.Error("the join password switch is not reserved")
	}
}
