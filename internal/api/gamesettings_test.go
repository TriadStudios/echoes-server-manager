package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
)

// The catalog of the game's settings is read-only and open to every role;
// without a login it is refused like every other call.
func TestGameSettingsCatalog(t *testing.T) {
	r := newRig(t, Options{})
	if rec := r.do("GET", "/api/v1/game-settings", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a login: %d %s", rec.Code, rec.Body)
	}
	plain, _, err := r.auth.CreateToken("reader", auth.RoleRead)
	if err != nil {
		t.Fatal(err)
	}
	rec := r.do("GET", "/api/v1/game-settings", "", header("Authorization", "Bearer "+plain))
	r.status(rec, http.StatusOK)
	var got GameSettingList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(got.Settings, gameserver.GameSettings(), func(a, b gameserver.GameSetting) bool { return a.Key == b.Key && a.Type == b.Type }) ||
		len(got.Groups) != len(gameserver.GameSettingGroups()) {
		t.Fatalf("catalog: %s", rec.Body)
	}
	var raw struct {
		Settings []map[string]any `json:"settings"`
	}
	json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, s := range raw.Settings {
		for _, field := range []string{"key", "group", "label", "type", "default", "help", "new_world_only", "caution", "secret"} {
			if _, ok := s[field]; !ok {
				t.Errorf("%v: no %s", s["key"], field)
			}
		}
	}
	for _, rt := range Routes() {
		if rt.Path == "/api/v1/game-settings" && (rt.Method != "GET" || rt.Role != string(auth.RoleRead)) {
			t.Errorf("route %+v", rt)
		}
	}
	if rec := r.do("POST", "/api/v1/game-settings", "{}", header("Authorization", "Bearer "+plain, "Content-Type", "application/json")); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}
