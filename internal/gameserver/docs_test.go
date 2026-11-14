package gameserver

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The documents list the names ESM knows the engine to search for: the
// design document every one of them, the API description all but the two
// that only add a digit to another.
func TestDocumentsListTheSearchedNames(t *testing.T) {
	design, err := os.ReadFile(filepath.Join("..", "..", "docs", "DESIGN.md"))
	if err != nil {
		t.Fatal(err)
	}
	api, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range SearchedNames() {
		if !strings.Contains(string(design), "`"+name+"`") {
			t.Errorf("docs/DESIGN.md does not list %s", name)
		}
		if short := strings.TrimRight(name, "23"); !strings.Contains(string(api), " "+short) {
			t.Errorf("docs/openapi.yaml does not list %s", short)
		}
	}
	for _, doc := range [][]byte{design, api} {
		for _, n := range []string{strconv.Itoa(MaxLineLength), strconv.Itoa(EngineLineLimit), strconv.Itoa(StartLineLimit), strconv.Itoa(StartRoom)} {
			if !strings.Contains(string(doc), n) {
				t.Errorf("a document does not name the limit %s", n)
			}
		}
	}
}
