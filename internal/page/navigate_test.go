package page_test

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// navigateDriver runs done and the release check's address (go and
// releaseCheckPath) with stubs of the page: a location whose replace and
// reload are recorded (replace takes the address it is given as the page's
// new one, a reload leaves it), and a history whose replaceState changes the
// address without loading anything. Each scenario starts at an address and
// either calls done with a notice code or goes where a release check that
// found result goes.
const navigateDriver = `
var replaced = [], reloaded = [];
var location = {
  href: '',
  replace: function (u) { replaced.push(u); location.href = new URL(u, location.href).href; },
  reload: function () { reloaded.push(location.href); }
};
var history = {
  state: null,
  replaceState: function (s, t, u) { location.href = new URL(u, location.href).href; }
};
(function () {
  var out = [];
  for (var i = 0; i < ARGS.length; i++) {
    var a = ARGS[i];
    replaced = []; reloaded = []; location.href = a.at;
    if (a.code) { done(a.code); } else { go(releaseCheckPath(a.result)); }
    out.push({ replaced: replaced, reloaded: reloaded, href: location.href });
  }
  process.stdout.write(JSON.stringify(out));
})();
`

// Going to an address that differs from the page's by the fragment alone
// is, for a browser, a jump within the page: nothing is loaded. The screens
// go to such an address when a change gives the notice the page shows
// already (the same settings saved twice, a second check for a release
// within a minute, which says the same), and the page then seemed to do
// nothing. done and the release check must always load a new document, and
// still end at the section the fragment names; where the address differs
// otherwise they go there as before. The script does this in go, run here
// with node and stubs of the page; skipped without node, except on the CI
// machines.
func TestNavigationLoadsInTheScript(t *testing.T) {
	node := nodeOrSkip(t, "go")
	prog := scriptBlock(t, "navigate")
	js, err := os.ReadFile("static/esm.js")
	if err != nil {
		t.Fatal(err)
	}
	// Nothing goes around go: no jump to an address with a fragment.
	for _, bad := range []string{"location.replace(u.pathname", "location.replace('/box?done="} {
		if strings.Contains(string(js), bad) {
			t.Errorf("esm.js has %q: it would only jump to the fragment of the address it is at", bad)
		}
	}
	if strings.Count(string(js), "go(releaseCheckPath(r));") != 1 || strings.Count(string(js), "go(u.pathname + u.search + u.hash);") != 1 {
		t.Error("esm.js does not go to the release check's address and the notice's address with go, once each")
	}

	const host = "http://esm.test"
	type result map[string]any
	cases := []struct {
		name   string
		at     string
		code   string // done(code), when set
		result result // else a release check that found this
		want   string
	}{
		{name: "a first notice", at: "/box", code: "started", want: "/box?done=started"},
		{name: "the same notice again, no fragment", at: "/box?done=started", code: "started", want: "/box?done=started"},
		{name: "a notice at a section", at: "/box#esm-h", code: "started", want: "/box?done=started#esm-h"},
		{name: "the same notice again at a section", at: "/box?done=started#esm-h", code: "started", want: "/box?done=started#esm-h"},
		{name: "another notice at a section", at: "/box?done=started#esm-h", code: "stopped", want: "/box?done=stopped#esm-h"},
		{name: "settings saved twice", at: "/servers/alpha/settings?done=saved#group-world", code: "saved", want: "/servers/alpha/settings?done=saved#group-world"},
		{name: "settings saved, then restarted", at: "/servers/alpha/settings?done=saved#group-world", code: "restarted", want: "/servers/alpha/settings?done=restarted#group-world"},
		{name: "a notice after a health check", at: "/box?check=1#health-h", code: "removed", want: "/box?check=1&done=removed#health-h"},
		{name: "the same notice after a health check", at: "/box?check=1&done=removed#health-h", code: "removed", want: "/box?check=1&done=removed#health-h"},

		{name: "a first check", at: "/box", result: result{}, want: "/box?done=release#esm-h"},
		{name: "a first check at the section", at: "/box#esm-h", result: result{}, want: "/box?done=release#esm-h"},
		{name: "a second check", at: "/box?done=release#esm-h", result: result{}, want: "/box?done=release#esm-h"},
		{name: "a second check within a minute", at: "/box?done=release#esm-h", result: result{"reused": true}, want: "/box?done=release-recent#esm-h"},
		{name: "a third check within a minute", at: "/box?done=release-recent#esm-h", result: result{"reused": true}, want: "/box?done=release-recent#esm-h"},
		{name: "a check the release server asks to wait for, twice", at: "/box?done=release-wait#esm-h", result: result{"retry_after": 30}, want: "/box?done=release-wait#esm-h"},
		{name: "a check at the notice without the section", at: "/box?done=release", result: result{}, want: "/box?done=release#esm-h"},
		{name: "a check after a health check", at: "/box?check=1#health-h", result: result{}, want: "/box?done=release#esm-h"},
	}
	type step struct {
		At     string `json:"at"`
		Code   string `json:"code,omitempty"`
		Result result `json:"result"`
	}
	var args []step
	for _, c := range cases {
		args = append(args, step{At: host + c.at, Code: c.code, Result: c.result})
	}
	arg, _ := json.Marshal(args)
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(prog + "\nvar ARGS = " + string(arg) + ";\n" + navigateDriver)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v\n%s", err, errOut.String())
	}
	var got []struct {
		Replaced []string `json:"replaced"`
		Reloaded []string `json:"reloaded"`
		Href     string   `json:"href"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node printed %q: %v", out.String(), err)
	}
	jumps := 0
	for n, c := range cases {
		g := got[n]
		cur, _ := url.Parse(host + c.at)
		want, _ := url.Parse(host + c.want)
		if len(g.Replaced)+len(g.Reloaded) != 1 {
			t.Errorf("%s: at %s it replaced %q and reloaded %q, want one of the two", c.name, c.at, g.Replaced, g.Reloaded)
			continue
		}
		// Where replacing the address would be a jump within the page, the
		// page is loaded again; elsewhere it goes there, as before.
		jump := fragmentOnly(cur, want, c.want)
		if jump {
			jumps++
		}
		if jump != (len(g.Reloaded) == 1) {
			t.Errorf("%s: at %s it replaced %q and reloaded %q, but going to %s is a jump within the page: %v", c.name, c.at, g.Replaced, g.Reloaded, c.want, jump)
		}
		for _, raw := range g.Replaced {
			target, err := cur.Parse(raw)
			if err != nil {
				t.Errorf("%s: went to %q: %v", c.name, raw, err)
				continue
			}
			if fragmentOnly(cur, target, raw) {
				t.Errorf("%s: at %s went to %q, which is a jump within the page and loads nothing", c.name, c.at, raw)
			}
		}
		// The page that is loaded is the one wanted, with the fragment that
		// scrolls it to its section.
		if g.Href != host+c.want || (len(g.Reloaded) == 1 && g.Reloaded[0] != host+c.want) {
			t.Errorf("%s: at %s it ends at %q (reloaded %q), want %s", c.name, c.at, g.Href, g.Reloaded, c.want)
		}
	}
	if jumps < 5 {
		t.Errorf("only %d cases are a jump within the page: the cases do not cover it", jumps)
	}
}
