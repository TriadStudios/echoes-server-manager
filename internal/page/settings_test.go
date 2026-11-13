package page_test

import (
	"html"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// fieldset returns the fieldset with the given id in body, or "".
func fieldset(body, id string) string {
	start := strings.Index(body, `<fieldset class="panel" id="`+id+`"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</fieldset>")
	if end < 0 {
		return ""
	}
	return body[start : start+end]
}

// The settings screen has a labelled field for every game setting of the
// catalog, in its group, with its help and default; the world rules carry
// the notice that only a new world takes them. Values set are shown (also
// one written another way than the field writes it), rules outside the
// catalog are listed apart, and a token is never shown.
func TestSettingsScreenShowsTheCatalog(t *testing.T) {
	s := newSite(t)
	s.setup()
	src := s.root + string(os.PathSeparator) + "imports"
	dir := testkit.MakeBuild(t, src, testkit.BuildOptions{Version: "1.0"})
	s.must(http.StatusCreated, "POST", "/api/v1/builds", s.admin, api.InstallRequest{Source: dir}, nil)
	s.must(http.StatusCreated, "POST", "/api/v1/instances", s.admin, map[string]any{"name": "alpha", "bind_ip": "127.0.0.1", "build": "1.0",
		"rules": map[string]string{"HarvestRate": "2.5", "PvP": "yes", "ChatFilter": "Drop", "MOTD": "Hello, traveller", "Banner": "a free rule"}}, nil)
	const token = "page-T0ken-1234"
	s.must(http.StatusOK, "PUT", "/api/v1/instances/alpha/secrets", s.admin, map[string]string{"announcements_token": token}, nil)
	var cat api.GameSettingList
	s.must(http.StatusOK, "GET", "/api/v1/game-settings", s.admin, nil, &cat)
	if len(cat.Settings) == 0 {
		t.Fatal("no catalog")
	}

	body := s.checkPage("/servers/alpha/settings", "session", s.csrf, token)
	for _, g := range cat.Groups {
		fs := fieldset(body, "group-"+g.ID)
		if fs == "" || !strings.Contains(fs, html.EscapeString(g.Title)) || !strings.Contains(fs, html.EscapeString(g.Note)) {
			t.Errorf("group %s: no fieldset with its title and note", g.ID)
		}
	}
	world := fieldset(body, "group-world")
	if !strings.Contains(world, "only when the world is first created") || !strings.Contains(world, `class="alert`) {
		t.Errorf("the world rules do not carry the notice:\n%s", world)
	}
	for _, x := range cat.Settings {
		fs := fieldset(body, "group-"+x.Group)
		name := `name="rule:` + x.Key + `"`
		if x.Secret {
			name = `name="` + x.SecretField + `"`
		}
		if !strings.Contains(fs, name) {
			t.Errorf("%s: no field in group %s", x.Key, x.Group)
			continue
		}
		if !strings.Contains(fs, html.EscapeString(x.Help)) || !strings.Contains(fs, html.EscapeString(x.Label)) {
			t.Errorf("%s: no label or help", x.Key)
		}
		if x.Default != "" && !strings.Contains(fs, "Default: ") {
			t.Errorf("%s: no default", x.Key)
		}
	}
	for want, what := range map[string]string{
		`name="rule:HarvestRate" min="0.1" max="1000" step="any" value="2.5" data-orig="2.5"`: "a number with its range and value",
		`<option value="Drop" selected>`:                        "the enum value set",
		`<option value="yes" selected>yes (as set)</option>`:    "a bool written another way, kept",
		`value="Hello, traveller" data-orig="Hello, traveller"`: "a text value",
		`name="rule:LastWipe" max="`:                            "a date that may not be in the future",
		`Announcements token (set)`:                             "a token that is set",
		`name="clear_announcements_token"`:                      "clearing a token",
		`Problem report token (none)`:                           "a token that is not set",
		`name="rule:Banner"`:                                    "a rule outside the catalog",
		`data-game`:                                             "the catalog fields are marked",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s: %q is missing", what, want)
		}
	}
	// A field whose rule is set says so, so that a rule stored with an
	// empty value is kept by a save that leaves it alone.
	if !regexp.MustCompile(`name="rule:HarvestRate"[^>]*data-set>`).MatchString(body) || regexp.MustCompile(`name="rule:XPRate"[^>]*data-set`).MatchString(body) {
		t.Error("data-set marks exactly the rules that are set")
	}
	other := fieldset(body, "rules")
	if strings.Contains(other, `name="rule:HarvestRate"`) || !strings.Contains(other, `name="rule:Banner"`) {
		t.Error("the other settings list rules outside the catalog, and only those")
	}
	if n := len(regexp.MustCompile(`name="rule:HarvestRate"`).FindAllString(body, -1)); n != 1 {
		t.Errorf("HarvestRate has %d fields", n)
	}
	// AllowNonSteamClients is marked.
	if !regexp.MustCompile(`(?s)data-setting="AllowNonSteamClients".*?Caution`).MatchString(body) {
		t.Error("the caution of AllowNonSteamClients is not shown")
	}
}
