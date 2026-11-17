package page_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// formRevisionRe finds the revision the settings form says it shows.
var formRevisionRe = regexp.MustCompile(`<form [^>]*data-form="settings"[^>]*data-revision="([^"]*)"`)

// The settings form says which revision of the server's settings it shows,
// so the script can make its save conditional on it (If-Match), and a
// reader of a changed server gets the new revision at the next load. The
// form has autocomplete off, so that a browser does not put the values typed
// before a reload back into the fields (they would then be saved over what
// another admin changed). Admins get a notice, hidden until a save is
// refused because the settings changed meanwhile, with its words and the
// button that loads the page anew; a role that cannot save has neither.
func TestSettingsFormCarriesTheRevision(t *testing.T) {
	s := newSite(t)
	s.setup()
	src := s.root + string(os.PathSeparator) + "imports"
	dir := testkit.MakeBuild(t, src, testkit.BuildOptions{Version: "1.0"})
	s.must(http.StatusCreated, "POST", "/api/v1/builds", s.admin, api.InstallRequest{Source: dir}, nil)
	s.must(http.StatusCreated, "POST", "/api/v1/instances", s.admin, map[string]any{"name": "alpha", "bind_ip": "127.0.0.1", "build": "1.0"}, nil)

	shown := func() string {
		t.Helper()
		body := s.checkPage("/servers/alpha/settings", "session", s.csrf)
		m := formRevisionRe.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("the settings form has no data-revision:\n%s", beforeFieldsets(body))
		}
		return m[1]
	}
	if got := shown(); got != "1" {
		t.Fatalf("a new server's form shows revision %q, want 1", got)
	}
	s.must(http.StatusOK, "PATCH", "/api/v1/instances/alpha", s.admin, map[string]any{"display_name": "Alpha"}, nil)
	if got := shown(); got != "2" {
		t.Fatalf("after a change the form shows revision %q, want 2", got)
	}
	s.must(http.StatusOK, "PUT", "/api/v1/instances/alpha/secrets", s.admin, map[string]any{"server_password": "join me 7"}, nil)
	if got := shown(); got != "3" {
		t.Fatalf("after a change of secrets the form shows revision %q, want 3", got)
	}

	body := s.checkPage("/servers/alpha/settings", "session", s.csrf)
	if !regexp.MustCompile(`<form [^>]*data-form="settings"[^>]*autocomplete="off"`).MatchString(body) {
		t.Error("the settings form does not have autocomplete off")
	}
	box := between(body, `<div class="alert warn stack" id="changed-meanwhile"`, "</div>")
	if box == "" || !strings.Contains(box, ` role="alert" hidden>`) {
		t.Fatalf("the page has no hidden notice for a save that is refused because the settings changed:\n%s", beforeFieldsets(body))
	}
	text := textOf(box)
	for _, want := range []string{
		"These settings changed while this page was open",
		"Nothing was saved, so that your save does not overwrite theirs.",
		"Reload to see the settings as they are now; what you typed here is lost.",
		"Your settings were saved, but the passwords were not",
		"then set the passwords again.",
		"Reload the settings",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the notice does not say %q:\n%s", want, text)
		}
	}
	if !strings.Contains(box, `<p data-changed="all">`) || !strings.Contains(box, `<p data-changed="part" hidden>`) ||
		!strings.Contains(box, `<button type="button" class="btn" data-act="reload-settings">Reload the settings</button>`) {
		t.Errorf("the notice has not the two cases and the button the script looks for:\n%s", box)
	}

	// A role that may not save settings has no such notice, and no save
	// buttons to need it.
	read := s.token("read")
	ro := s.checkPage("/servers/alpha/settings", read, "")
	if strings.Contains(ro, "changed-meanwhile") || strings.Contains(ro, "data-save=") {
		t.Error("a read role's settings page has the notice or save buttons")
	}
}

// beforeFieldsets is the page up to its first fieldset, for a short failure
// message.
func beforeFieldsets(body string) string {
	if i := strings.Index(body, "<fieldset"); i >= 0 {
		return body[:i]
	}
	return body
}

// between returns the text from the first start to the first end after it,
// end included, or "".
func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		return ""
	}
	return s[i : i+j+len(end)]
}

// These checks of the script need no node: the settings form saves through
// settingsSave and nothing else, the call can take further headers, the
// notice's button has its action, and the answer that means "changed" is
// the one the script looks for.
func TestSettingsSaveIsConditionalInTheScript(t *testing.T) {
	b, err := os.ReadFile("static/esm.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, need := range []string{
		"settingsSave(inst(name), f.dataset.revision, body(), { call: call })",
		"e.status === 412 && e.code === 'changed'",
		"'If-Match': '\"' + n + '\"'",
		"async function call(method, path, body, headers) {",
		"'reload-settings': async function () {",
		"u.searchParams.delete('done');",
		"document.getElementById('changed-meanwhile')",
		"changedNotice(box, res.saved)",
	} {
		if !strings.Contains(js, need) {
			t.Errorf("the script lacks %q", need)
		}
	}
	// The settings form's own PATCH and PUT are the two of settingsSave,
	// each with its If-Match; none is sent from the form without one.
	for _, bad := range []string{
		"call('PATCH', inst(name), parts.change)",
		"call('PUT', inst(name) + '/secrets', parts.secrets)",
	} {
		if strings.Contains(js, bad) {
			t.Errorf("the settings form still saves with %q, without If-Match", bad)
		}
	}
	if strings.Count(js, "io.call('PATCH', base, parts.change, ifMatch(rev))") != 1 || strings.Count(js, "io.call('PUT', base + '/secrets', parts.secrets, ifMatch(rev))") != 1 {
		t.Error("settingsSave does not send both its calls with ifMatch(rev)")
	}
}

// settingsSaveDriver runs settingsSave once for each scenario with a call
// that answers as the scenario says, in order (a body, or a failure with a
// status and a code), and records what it was asked.
const settingsSaveDriver = `
(async function () {
  var out = [];
  for (var i = 0; i < ARGS.length; i++) {
    var a = ARGS[i];
    var calls = [];
    var n = 0;
    var io = { call: function (method, path, body, headers) {
      calls.push({ method: method, path: path, body: body, headers: headers || null });
      var ans = a.answers[n++] || {};
      if (ans.fail) {
        var e = new Error('refused');
        e.status = ans.fail.status;
        e.code = ans.fail.code;
        return Promise.reject(e);
      }
      return Promise.resolve(ans.body || {});
    } };
    var res = null, threw = null;
    try { res = await settingsSave(a.base, a.revision, a.parts, io); } catch (e) { threw = { status: e.status, code: e.code }; }
    out.push({ res: res, threw: threw, calls: calls });
  }
  process.stdout.write(JSON.stringify(out));
})();
`

// saveCall is a call settingsSave makes, as the driver records it.
type saveCall struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Body    map[string]any    `json:"body"`
	Headers map[string]string `json:"headers"`
}

// saveScenario is one run of settingsSave: what it is given (the exported
// fields, which the driver reads) and what it must do.
type saveScenario struct {
	Name     string         `json:"name"`
	Base     string         `json:"base"`
	Revision any            `json:"revision"`
	Parts    map[string]any `json:"parts"`
	Answers  []any          `json:"answers"`

	changed bool
	saved   []string
	threw   bool // a failure is thrown, with this status
	status  int
	calls   []saveCall
}

// saveRun is what the driver recorded of one scenario.
type saveRun struct {
	Res *struct {
		Changed bool     `json:"changed"`
		Saved   []string `json:"saved"`
	} `json:"res"`
	Threw *struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	} `json:"threw"`
	Calls []saveCall `json:"calls"`
}

func sameCalls(a, b []saveCall) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Method != b[i].Method || a[i].Path != b[i].Path || !reflect.DeepEqual(a[i].Body, b[i].Body) ||
			len(a[i].Headers) != len(b[i].Headers) || (len(a[i].Headers) > 0 && !reflect.DeepEqual(a[i].Headers, b[i].Headers)) {
			return false
		}
	}
	return true
}

// The script's settingsSave makes the two calls of a save of the settings
// form, each only if there is something to send, both conditional on the
// revision the form showed: the settings with it and the passwords with the
// revision the answer to the settings gave, so a form does not refuse its
// own save but refuses what another change came between. When the API says
// the settings changed (412 "changed") it says so, with what was saved
// before that, and makes no further call; any other failure is thrown as it
// is. Run with node and a call that answers as told; skipped without node,
// except on the CI machines.
func TestSettingsSaveInTheScript(t *testing.T) {
	node := nodeOrSkip(t, "settingsSave")
	prog := scriptBlock(t, "settingsSave")
	changeBody := map[string]any{"display_name": "Alpha", "rules": map[string]any{"A": "b"}}
	secretsBody := map[string]any{"server_password": "join me 7"}
	ok := func(rev int) any { return map[string]any{"body": map[string]any{"revision": rev}} }
	fail := func(status int, code string) any {
		return map[string]any{"fail": map[string]any{"status": status, "code": code}}
	}
	const base = "/api/v1/instances/alpha"
	parts := func(change, secrets map[string]any) map[string]any {
		if change == nil {
			change = map[string]any{}
		}
		if secrets == nil {
			secrets = map[string]any{}
		}
		return map[string]any{"change": change, "secrets": secrets}
	}
	ifm := func(rev string) map[string]string { return map[string]string{"If-Match": `"` + rev + `"`} }
	patch := func(rev string) saveCall { return saveCall{"PATCH", base, changeBody, ifm(rev)} }
	put := func(rev string) saveCall { return saveCall{"PUT", base + "/secrets", secretsBody, ifm(rev)} }
	scenarios := []saveScenario{
		{Name: "both", Base: base, Revision: "4", Parts: parts(changeBody, secretsBody), Answers: []any{ok(5), ok(6)},
			saved: []string{"settings", "passwords"}, calls: []saveCall{patch("4"), put("5")}},
		{Name: "settings only", Base: base, Revision: "12", Parts: parts(changeBody, nil), Answers: []any{ok(13)},
			saved: []string{"settings"}, calls: []saveCall{patch("12")}},
		{Name: "passwords only", Base: base, Revision: "4", Parts: parts(nil, secretsBody), Answers: []any{ok(5)},
			saved: []string{"passwords"}, calls: []saveCall{put("4")}},
		{Name: "nothing to save", Base: base, Revision: "4", Parts: parts(nil, nil), saved: []string{}},
		{Name: "settings refused as changed", Base: base, Revision: "4", Parts: parts(changeBody, secretsBody), Answers: []any{fail(412, "changed")},
			changed: true, saved: []string{}, calls: []saveCall{patch("4")}},
		{Name: "passwords refused as changed", Base: base, Revision: "4", Parts: parts(nil, secretsBody), Answers: []any{fail(412, "changed")},
			changed: true, saved: []string{}, calls: []saveCall{put("4")}},
		{Name: "passwords refused as changed after the settings were saved", Base: base, Revision: "4", Parts: parts(changeBody, secretsBody),
			Answers: []any{ok(5), fail(412, "changed")}, changed: true, saved: []string{"settings"}, calls: []saveCall{patch("4"), put("5")}},
		// Other failures are not "changed": they are thrown as they are.
		{Name: "a 412 with another code", Base: base, Revision: "4", Parts: parts(changeBody, nil), Answers: []any{fail(412, "something_else")},
			threw: true, status: 412, calls: []saveCall{patch("4")}},
		{Name: "a refused value", Base: base, Revision: "4", Parts: parts(changeBody, secretsBody), Answers: []any{fail(422, "invalid")},
			threw: true, status: 422, calls: []saveCall{patch("4")}},
		{Name: "a refused password after the settings were saved", Base: base, Revision: "4", Parts: parts(changeBody, secretsBody),
			Answers: []any{ok(5), fail(422, "password_too_short")}, threw: true, status: 422, calls: []saveCall{patch("4"), put("5")}},
		{Name: "a failure that is no answer", Base: base, Revision: "4", Parts: parts(changeBody, nil), Answers: []any{fail(0, "")},
			threw: true, status: 0, calls: []saveCall{patch("4")}},
		// A form that does not know its revision names none: there is
		// nothing to name, and the call is as it was before revisions.
		{Name: "no revision", Base: base, Revision: "", Parts: parts(changeBody, secretsBody), Answers: []any{ok(5), ok(6)},
			saved: []string{"settings", "passwords"}, calls: []saveCall{{"PATCH", base, changeBody, nil}, put("5")}},
		{Name: "a revision that is none", Base: base, Revision: "0", Parts: parts(nil, secretsBody), Answers: []any{ok(1)},
			saved: []string{"passwords"}, calls: []saveCall{{"PUT", base + "/secrets", secretsBody, nil}}},
		{Name: "a revision that is not a number", Base: base, Revision: "abc", Parts: parts(nil, secretsBody), Answers: []any{ok(1)},
			saved: []string{"passwords"}, calls: []saveCall{{"PUT", base + "/secrets", secretsBody, nil}}},
		{Name: "no revision in the answer", Base: base, Revision: "4", Parts: parts(changeBody, secretsBody),
			Answers: []any{map[string]any{"body": map[string]any{}}, ok(6)},
			saved:   []string{"settings", "passwords"}, calls: []saveCall{patch("4"), {"PUT", base + "/secrets", secretsBody, nil}}},
		{Name: "a revision as a number", Base: base, Revision: 7, Parts: parts(changeBody, nil), Answers: []any{ok(8)},
			saved: []string{"settings"}, calls: []saveCall{patch("7")}},
	}
	arg, _ := json.Marshal(scenarios)
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(prog + "\nvar ARGS = " + string(arg) + ";\n" + settingsSaveDriver)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v\n%s", err, errOut.String())
	}
	var got []saveRun
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != len(scenarios) {
		t.Fatalf("node printed %q: %v", out.String(), err)
	}
	for n, c := range scenarios {
		g := got[n]
		if c.threw {
			if g.Threw == nil || g.Threw.Status != c.status || g.Res != nil {
				t.Errorf("%s: %+v, want the failure with status %d thrown", c.Name, g, c.status)
			}
		} else if g.Threw != nil || g.Res == nil || g.Res.Changed != c.changed || len(g.Res.Saved) != len(c.saved) ||
			(len(c.saved) > 0 && !reflect.DeepEqual(g.Res.Saved, c.saved)) {
			t.Errorf("%s: %+v, want changed=%v, saved %v", c.Name, g, c.changed, c.saved)
		}
		if !sameCalls(g.Calls, c.calls) {
			t.Errorf("%s: calls\n%+v\nwant\n%+v", c.Name, g.Calls, c.calls)
		}
	}
}

// changedNoticeDriver runs changedNotice on a box of two paragraphs, one
// for each case, and a button, as the page has them, for each list of what
// was saved.
const changedNoticeDriver = `
(function () {
  var out = [];
  for (var i = 0; i < ARGS.length; i++) {
    var button = { tag: 'button' };
    function para(which, hidden) {
      return { which: which, hidden: hidden, getAttribute: function (n) { return n === 'data-changed' ? this.which : null; } };
    }
    var all = para('all', false), part = para('part', false);
    var box = {
      hidden: true,
      querySelectorAll: function (sel) { return sel === '[data-changed]' ? [all, part] : []; },
      querySelector: function (sel) { return sel === 'button' ? button : null; }
    };
    var got = changedNotice(box, ARGS[i]);
    out.push({ box: box.hidden, all: all.hidden, part: part.hidden, button: got === button });
  }
  process.stdout.write(JSON.stringify(out));
})();
`

// The notice of a save that is refused because the settings changed shows
// the paragraph for what happened (nothing saved, or the settings saved and
// the passwords not) and hides the other, shows the box, and hands back its
// button to focus. Run with node and stubs of the page; skipped without
// node, except on the CI machines.
func TestChangedNoticeInTheScript(t *testing.T) {
	node := nodeOrSkip(t, "changedNotice")
	prog := scriptBlock(t, "settingsSave")
	cases := []struct {
		saved []string
		all   bool // the paragraph for nothing saved is shown
	}{
		{[]string{}, true},
		{[]string{"settings"}, false},
		{[]string{"settings", "passwords"}, false},
	}
	var args [][]string
	for _, c := range cases {
		args = append(args, c.saved)
	}
	arg, _ := json.Marshal(args)
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(prog + "\nvar ARGS = " + string(arg) + ";\n" + changedNoticeDriver)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v\n%s", err, errOut.String())
	}
	var got []struct {
		Box    bool `json:"box"`
		All    bool `json:"all"`
		Part   bool `json:"part"`
		Button bool `json:"button"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node printed %q: %v", out.String(), err)
	}
	for n, c := range cases {
		g := got[n]
		if g.Box || !g.Button || g.All != !c.all || g.Part != c.all {
			t.Errorf("saved %v: box hidden %v, paragraph for nothing saved hidden %v, for part saved hidden %v, button handed back %v; want the %s paragraph shown",
				c.saved, g.Box, g.All, g.Part, g.Button, map[bool]string{true: "first", false: "second"}[c.all])
		}
	}
}
