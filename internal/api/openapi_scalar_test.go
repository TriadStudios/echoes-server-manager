package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// docs/openapi.yaml is read as text by TestOpenAPIMatchesRoutes, and ESM has
// no YAML library, so a plain scalar that a YAML parser would read otherwise
// than it was written has to be found by looking: a description with a comma
// in a flow mapping ({ type: integer, description: The bytes, when known. })
// is cut at the comma, and the rest becomes a key of its own; one with a
// colon and a space inside is no scalar at all. Both are written with quotes.
// The rules, for every line that is not in a block scalar (| or >):
//   - in a flow mapping, each part between the commas that are not in quotes,
//     brackets or braces is a key and its value, and a plain value has no ": "
//     and no " #";
//   - in a block mapping, a plain value has no ": " and no " #".
var (
	flowLine  = regexp.MustCompile(`^\s*(?:- )?(?:[^\s:{][^:]*:\s*)?\{(.*)\}\s*$`)
	blockLine = regexp.MustCompile(`^(\s*)(?:- )?([A-Za-z0-9_$"/{}.-]+):(?:\s+(\S.*))?$`)
	flowKey   = regexp.MustCompile(`^\s*("[^"]*"|'[^']*'|[A-Za-z0-9_$.-]+):(\s|$)`)
)

// splitFlow splits the inside of a flow mapping at its top level commas.
func splitFlow(s string) []string {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == 0x5c && quote == '"' { // a backslash in a double-quoted scalar
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == 0x27:
			quote = c
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// plainValue says that v is written without quotes, and is not a flow
// collection, a block scalar or a reference.
func plainValue(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && !strings.ContainsAny(v[:1], "\"'{[|>&*!%@`#")
}

func unsafePlain(v string) bool {
	return strings.Contains(v, ": ") || strings.Contains(v, " #") || strings.HasSuffix(v, ":")
}

// openAPIScalarProblems lists the lines of a YAML text that break the rules
// above.
func openAPIScalarProblems(text string) []string {
	var out []string
	skipBelow := -1 // the indentation of the key of a block scalar that runs
	for n, line := range strings.Split(text, "\n") {
		trim := strings.TrimLeft(line, " ")
		indent := len(line) - len(trim)
		if skipBelow >= 0 {
			if trim == "" || indent > skipBelow {
				continue
			}
			skipBelow = -1
		}
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		at := " (line " + strconv.Itoa(n+1) + ": "
		if m := flowLine.FindStringSubmatch(line); m != nil {
			for _, part := range splitFlow(m[1]) {
				k := flowKey.FindStringSubmatch(part)
				if k == nil {
					out = append(out, strings.TrimSpace(part)+at+"a part of a flow mapping that is no key and value; a comma in a plain scalar?)")
					continue
				}
				if v := strings.TrimSpace(part[len(k[0]):]); plainValue(v) && unsafePlain(v) {
					out = append(out, v+at+"a plain value with a colon or a comment mark)")
				}
			}
			continue
		}
		if m := blockLine.FindStringSubmatch(line); m != nil {
			v := strings.TrimSpace(m[3])
			if strings.HasPrefix(v, "|") || strings.HasPrefix(v, ">") {
				skipBelow = len(m[1])
				if strings.HasPrefix(trim, "- ") {
					skipBelow += 2
				}
				continue
			}
			if plainValue(v) && unsafePlain(v) {
				out = append(out, v+at+"a plain value with a colon or a comment mark)")
			}
		}
	}
	return out
}

func TestOpenAPIPlainScalarsAreSafe(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range openAPIScalarProblems(string(b)) {
		t.Errorf("docs/openapi.yaml: %s", p)
	}
}

// The check finds what it is for, and leaves alone what is written right.
func TestOpenAPIScalarCheckFindsWhatItIsFor(t *testing.T) {
	bad := `
properties:
  total: { type: integer, description: The bytes there are, when they are known. }
  id: { type: string, description: User name, token id, or "socket". }
  cancellable:
    type: boolean
    description: A cancel would be taken now: an install that has not begun.
  note: { type: string, description: Fine here but a # comment starts at this mark }
`
	got := strings.Join(openAPIScalarProblems(bad), "\n")
	for _, line := range []string{"(line 3:", "(line 4:", "(line 7:", "(line 8:"} {
		if !strings.Contains(got, line) {
			t.Errorf("no problem found at %s in:\n%s", line, got)
		}
	}
	if strings.Contains(got, "(line 5:") || strings.Contains(got, "(line 6:") {
		t.Errorf("a problem found where there is none:\n%s", got)
	}
	good := `
properties:
  total: { type: integer, description: "The bytes there are, when they are known." }
  id: { type: string, description: "User name, token id, or \"socket\"." }
  state: { type: string, enum: [a, b, c], default: a, examples: ["x, y"] }
  ref: { $ref: "#/components/schemas/X" }
  nested: { type: array, items: { type: string, description: "a, b" } }
  cancellable:
    type: boolean
    description: "A cancel would be taken now: an install that has not begun."
  more:
    description: |
      A block scalar may hold, commas: colons # and { braces, }
      of any kind.
    type: string
  list:
    - { $ref: "#/components/schemas/Y" }
    - $ref: "#/components/schemas/Z"
  /api/v1/x/{name}:
    get:
      summary: Fine
`
	if got := openAPIScalarProblems(good); len(got) != 0 {
		t.Fatalf("problems %q in what is written right", got)
	}
}
