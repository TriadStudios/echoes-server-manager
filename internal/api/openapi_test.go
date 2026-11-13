package api

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestOpenAPIMatchesRoutes checks that every registered route is in
// docs/openapi.yaml and that the file names no route that does not exist.
// The file is read as text: a path line "  /api/v1/...:" under "paths:"
// followed by method lines "    get:".
func TestOpenAPIMatchesRoutes(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pathRe := regexp.MustCompile(`^  (/\S*):\s*$`)
	methodRe := regexp.MustCompile(`^    (get|put|post|patch|delete|head|options):\s*$`)
	var documented []string
	inPaths := false
	current := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, " ") && line != "" {
			inPaths = line == "paths:"
			current = ""
			continue
		}
		if !inPaths {
			continue
		}
		if m := pathRe.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if m := methodRe.FindStringSubmatch(line); m != nil && current != "" {
			documented = append(documented, strings.ToUpper(m[1])+" "+current)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	var registered []string
	for _, r := range Routes() {
		registered = append(registered, r.Method+" "+r.Path)
	}
	slices.Sort(documented)
	slices.Sort(registered)
	for _, r := range registered {
		if !slices.Contains(documented, r) {
			t.Errorf("route %s is not in docs/openapi.yaml", r)
		}
	}
	for _, d := range documented {
		if !slices.Contains(registered, d) {
			t.Errorf("docs/openapi.yaml has %s, which is not a route", d)
		}
	}
	if len(registered) == 0 || len(documented) != len(slices.Compact(slices.Clone(documented))) {
		t.Fatalf("registered %d routes, documented %v", len(registered), documented)
	}
}
