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

var tagRe = regexp.MustCompile(`<[^>]*>`)

// textOf is the text of a piece of HTML as a reader sees it: without its
// tags, entities read, every run of white space one space.
func textOf(h string) string {
	return strings.Join(strings.Fields(html.UnescapeString(tagRe.ReplaceAllString(h, " "))), " ")
}

// The settings screen has a labelled field for every game setting of the
// catalog, in its group, with its help and default; the world rules carry
// the note that the game applies them at every start, as a plain note like
// the other groups, not as a warning. Values set are shown (also
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
	if !strings.Contains(textOf(world), "The game applies these at every start of the server. A rule left empty uses the game's default and replaces the value saved with the world.") {
		t.Errorf("the world rules do not carry the note:\n%s", world)
	}
	if strings.Contains(world, `class="alert`) || !strings.Contains(world, `<p class="note">The game applies these`) {
		t.Errorf("the note of the world rules is not a plain note like the others:\n%s", world)
	}
	if text := textOf(body); strings.Contains(text, "only when a world is created") || strings.Contains(text, "first created") {
		t.Error("the page still says that world rules apply only to a new world")
	}
	if !strings.Contains(textOf(body), "Settings and rules take effect when the server starts.") {
		t.Error("the page does not say when settings and rules take effect")
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
		// The help is shown whole, as text: its first sentence under the
		// field, the rest behind "More".
		if !strings.Contains(textOf(fs), x.Help) || !strings.Contains(fs, html.EscapeString(x.Label)) {
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
		`as <span class="mono">-ServerName</span> and as <span class="mono">-SteamServerName</span>`: "the two switches the server name goes to",
		`A new name reaches the server at its next start.`:                                           "when a new server name applies",
		`at most 63 bytes of UTF-8 (63 characters of plain ASCII`:                                    "the longest server name",
		`name="display_name" maxlength="63"`:                                                         "the server name field held to 63",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s: %q is missing", what, want)
		}
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
