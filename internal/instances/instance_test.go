package instances

import (
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
)

func validInstance() Instance {
	in := Instance{Name: "alpha", DisplayName: "Alpha", Channel: "live", Build: "1.0", World: "alpha",
		MaxPlayers: 10, Backup: BackupPolicy{Keep: 10}, Ports: Ports{Game: 7777, Query: 27015}}
	in.normalize()
	return in
}

func TestValidate(t *testing.T) {
	ok := validInstance()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(in *Instance){
		"upper-case name":       func(in *Instance) { in.Name = "Alpha" },
		"name with digit first": func(in *Instance) { in.Name = "1alpha" },
		"long name":             func(in *Instance) { in.Name = "a" + strings.Repeat("b", 32) },
		"bad channel":           func(in *Instance) { in.Channel = "beta" },
		"bad build":             func(in *Instance) { in.Build = "../1" },
		"bad world":             func(in *Instance) { in.World = "../w" },
		"world with space":      func(in *Instance) { in.World = "my world" },
		"map starting with -":   func(in *Instance) { in.Map = "-Port=1" },
		"map with ..":           func(in *Instance) { in.Map = "/Levels/../x" },
		"same ports":            func(in *Instance) { in.Ports.Query = in.Ports.Game },
		"port too big":          func(in *Instance) { in.Ports.Game = 70000 },
		"bad bind ip":           func(in *Instance) { in.BindIP = "example" },
		"zero players":          func(in *Instance) { in.MaxPlayers = 0 },
		"too many players":      func(in *Instance) { in.MaxPlayers = 201 },
		"bad rule key":          func(in *Instance) { in.Rules["1abc"] = "x" },
		"rule key with dash":    func(in *Instance) { in.Rules["a-b"] = "x" },
		"reserved rule":         func(in *Instance) { in.Rules["Port"] = "1" },
		"reserved rule case":    func(in *Instance) { in.Rules["serverpassword"] = "x" },
		"rule value newline":    func(in *Instance) { in.Rules["Motd"] = "a\nb" },
		"rule value long":       func(in *Instance) { in.Rules["Motd"] = strings.Repeat("x", 257) },
		"arg without dash":      func(in *Instance) { in.ExtraArgs = []string{"nodash"} },
		"reserved arg":          func(in *Instance) { in.ExtraArgs = []string{"-QueryPort=1"} },
		"reserved arg flag":     func(in *Instance) { in.ExtraArgs = []string{"-log"} },
		"arg with newline":      func(in *Instance) { in.ExtraArgs = []string{"-X=a\nb"} },
		"arg with NUL":          func(in *Instance) { in.ExtraArgs = []string{"-X=a\x00"} },
		"bad restart time":      func(in *Instance) { in.Schedule.RestartAt = []string{"24:00"} },
		"twice the same time":   func(in *Instance) { in.Schedule.RestartAt = []string{"04:00", "04:00"} },
		"backup too often":      func(in *Instance) { in.Backup.Every = config.Duration(time.Second) },
		"backup keep zero":      func(in *Instance) { in.Backup.Keep = 0 },
		"bad memory":            func(in *Instance) { in.Limits.MemoryMax = "8 gigs" },
		"negative cpu":          func(in *Instance) { in.Limits.CPUQuotaPercent = -1 },
		"display name control":  func(in *Instance) { in.DisplayName = "a\tb" },
		"bad cluster":           func(in *Instance) { in.Cluster = "Has Space" },
	}
	for name, mutate := range cases {
		in := validInstance()
		mutate(&in)
		if err := in.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := validInstance()
	good.Map = "/Levels/Example"
	good.Rules = map[string]string{"Difficulty": "hard", "PvP": "true"}
	good.ExtraArgs = []string{"-NoSteam", "-Mode=Fast"}
	good.Schedule.RestartAt = []string{"04:30", "16:30"}
	good.Backup = BackupPolicy{Every: config.Duration(time.Hour), Keep: 5}
	good.Limits = Limits{MemoryMax: "8G", CPUQuotaPercent: 200}
	good.BindIP = "192.0.2.10"
	good.Cluster = "eu-1"
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "512M": 512 << 20, "8G": 8 << 30, "8g": 8 << 30, "1T": 1 << 40, "1024": 1024} {
		if got, err := ParseMemory(in); err != nil || got != want {
			t.Errorf("ParseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "-1G", "8GB", "G", "1.5G", "99999999999999T"} {
		if _, err := ParseMemory(in); err == nil {
			t.Errorf("ParseMemory(%q) accepted", in)
		}
	}
}

func TestBackoffAndGiveUp(t *testing.T) {
	base, max := time.Second, 10*time.Second
	want := []time.Duration{1, 2, 4, 8, 10, 10}
	for i, w := range want {
		if got := BackoffDelay(base, max, i+1); got != w*time.Second {
			t.Errorf("delay after crash %d = %s, want %s", i+1, got, w*time.Second)
		}
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	window := 10 * time.Minute
	times := []time.Time{now.Add(-11 * time.Minute), now.Add(-10 * time.Minute), now.Add(-9 * time.Minute), now.Add(-time.Minute)}
	recent := RecentCrashes(times, now, window)
	if len(recent) != 2 {
		t.Fatalf("recent = %v, want the last two", recent)
	}
	if GiveUp(recent, 3) {
		t.Fatal("gave up after 2 of 3")
	}
	if !GiveUp(append(recent, now), 3) {
		t.Fatal("did not give up after 3 of 3")
	}
}

func TestNextRestart(t *testing.T) {
	loc := time.FixedZone("test", 2*3600)
	now := time.Date(2026, 3, 10, 10, 15, 0, 0, loc)
	cases := []struct {
		times []string
		want  time.Time
	}{
		{nil, time.Time{}},
		{[]string{"11:00"}, time.Date(2026, 3, 10, 11, 0, 0, 0, loc)},
		{[]string{"10:15"}, time.Date(2026, 3, 11, 10, 15, 0, 0, loc)},
		{[]string{"04:00", "16:30"}, time.Date(2026, 3, 10, 16, 30, 0, 0, loc)},
		{[]string{"04:00", "09:00"}, time.Date(2026, 3, 11, 4, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		if got := NextRestart(now, c.times); !got.Equal(c.want) {
			t.Errorf("NextRestart(%v) = %v, want %v", c.times, got, c.want)
		}
	}
}
