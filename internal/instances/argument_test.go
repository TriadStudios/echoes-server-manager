package instances

import (
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
)

// The server joins its arguments into one command line and searches it for
// switches, so no setting that goes onto it may carry a quote, the name
// MULTIHOME, or white space followed by "-": with those a value could add
// a switch of its own, -MULTIHOME above all.
func TestSettingsCannotSmuggleSwitches(t *testing.T) {
	refused := map[string]func(*Instance){
		"extra value holds -MULTIHOME=":  func(i *Instance) { i.ExtraArgs = []string{"-Foo=bar -MULTIHOME=192.0.2.10"} },
		"extra value quote breakout":     func(i *Instance) { i.ExtraArgs = []string{`-Foo=a" -MULTIHOME=192.0.2.10 "b`} },
		"extra value no space":           func(i *Instance) { i.ExtraArgs = []string{"-Foo=x-MULTIHOME=192.0.2.10"} },
		"extra value another switch":     func(i *Instance) { i.ExtraArgs = []string{"-Foo=x -SecretsFile=/tmp/x"} },
		"extra value tab dash":           func(i *Instance) { i.ExtraArgs = []string{"-Foo=x -Port=1"} },
		"extra quote":                    func(i *Instance) { i.ExtraArgs = []string{`-Motd="hello"`} },
		"rule value holds -MULTIHOME=":   func(i *Instance) { i.Rules = map[string]string{"Foo": `a" -MULTIHOME=192.0.2.10 "b`} },
		"rule value space dash":          func(i *Instance) { i.Rules = map[string]string{"Foo": "a -Port=1"} },
		"rule value quote":               func(i *Instance) { i.Rules = map[string]string{"Foo": `a"b`} },
		"rule value multihome":           func(i *Instance) { i.Rules = map[string]string{"Foo": "use multihome"} },
		"rule key holds multihome":       func(i *Instance) { i.Rules = map[string]string{"NoMultiHomeHere": "1"} },
		"display name holds -MULTIHOME=": func(i *Instance) { i.DisplayName = `x" -MULTIHOME=192.0.2.10 "` },
		"display name MULTIHOME=":        func(i *Instance) { i.DisplayName = "MULTIHOME=192.0.2.10" },
		"display name space dash":        func(i *Instance) { i.DisplayName = "Alpha -Port=1" },
		"display name quote":             func(i *Instance) { i.DisplayName = `The "Best" Server` },
		"world multihome":                func(i *Instance) { i.World = "Multihome" },
		"map multihome":                  func(i *Instance) { i.Map = "/Levels/MultiHome/Map" },
	}
	for what, mutate := range refused {
		in := validInstance()
		mutate(&in)
		err := in.Validate()
		if code := fault.CodeOf(err); code != "unsafe_argument" && code != "invalid" {
			t.Errorf("%s: %v, want refused", what, err)
			continue
		}
		if err != nil && strings.Contains(err.Error(), "192.0.2.10") {
			t.Errorf("%s: the answer repeats the value: %v", what, err)
		}
	}
	accepted := map[string]func(*Instance){
		"display name with spaces":  func(i *Instance) { i.DisplayName = "Alpha EU - west" },
		"display name with dashes":  func(i *Instance) { i.DisplayName = "Alpha-EU" },
		"display name apostrophe":   func(i *Instance) { i.DisplayName = "Alpha's Server" },
		"rule value with a dash":    func(i *Instance) { i.Rules = map[string]string{"Motd": "well-met friends"} },
		"extra arg value form":      func(i *Instance) { i.ExtraArgs = []string{"-Motd Café"} },
		"extra arg negative number": func(i *Instance) { i.ExtraArgs = []string{"-Offset=-5"} },
	}
	for what, mutate := range accepted {
		in := validInstance()
		mutate(&in)
		if err := in.Validate(); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
}
