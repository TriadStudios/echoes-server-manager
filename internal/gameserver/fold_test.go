package gameserver

import (
	"testing"
	"time"
)

// The game compares the words of a choice and of a switch that is on or
// off without regard to the case of ASCII letters only. A letter that
// Unicode folds onto an ASCII one (the long s onto "s", the Kelvin sign
// onto "k") makes another word to the game, which it ignores; ESM refuses
// it instead of showing a setting the game does not apply.
func TestChoicesAndSwitchesFoldASCIIOnly(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for key, values := range map[string][]string{
		"ChatFilter":    {"Ma\u017fk", "Mas\u212a", "MA\u017fK", "\uff2dask"},
		"DeathDropMode": {"Everyth\u0131ng", "Noth\u0130ng", "\u0130nventoryOnly"},
		"PvP":           {"Fal\u017fe", "fal\u017fe", "ye\u017f", "\uff34rue", "TR\u00dce", "\u0131"},
		"ChatLog":       {"YE\u017f"},
	} {
		s, ok := LookupGameSetting(key)
		if !ok {
			t.Fatalf("%s is not in the catalog", key)
		}
		for _, v := range values {
			if err := s.Check(v, now); err == nil {
				t.Errorf("%s=%q accepted: the game does not read it as one of its words", key, v)
			}
		}
	}
	for key, values := range map[string][]string{
		"ChatFilter":    {"mask", "MASK", "mAsK", "off", "DROP"},
		"DeathDropMode": {"everything", "INVENTORYONLY", "nothing"},
		"PvP":           {"TRUE", "False", "YES", "nO", "1", "0"},
	} {
		s, _ := LookupGameSetting(key)
		for _, v := range values {
			if err := s.Check(v, now); err != nil {
				t.Errorf("%s=%q refused: %v", key, v, err)
			}
		}
	}
	// A key in another case is found by the ASCII rule too.
	if s, ok := FoldGameSetting("chatfilter"); !ok || s.Key != "ChatFilter" {
		t.Error("the case-blind lookup does not find ChatFilter")
	}
	if _, ok := FoldGameSetting("La\u017ftWipe"); ok {
		t.Error("the case-blind lookup takes a long s for an s")
	}
	if _, ok := FoldGameSetting("ChatBloc\u212aedWords"); ok {
		t.Error("the case-blind lookup takes a Kelvin sign for a k")
	}
}
