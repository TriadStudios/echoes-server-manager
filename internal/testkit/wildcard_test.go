package testkit

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// wildcardPatterns find Go source lines that bind, or build an address
// for, all interfaces: 0.0.0.0, [::] or "::", an address with an empty
// host (":8080", ":"+port, Addr: ":..."), an empty host handed to
// JoinHostPort or netbind.PortFree, and the net package's zero addresses.
var wildcardPatterns = []*regexp.Regexp{
	regexp.MustCompile(`0\.0\.0\.0`),
	regexp.MustCompile(`"\[::\]`),
	regexp.MustCompile(`"::"`),
	regexp.MustCompile(`"(?:tcp|udp)[46]?",\s*":`),
	regexp.MustCompile(`(?:^|[\s(,{\[=])":"\s*\+`),
	regexp.MustCompile(`(?:^|[\s(,{\[=])":(?:\d|%d)`),
	regexp.MustCompile(`Addr:\s*":`),
	regexp.MustCompile(`JoinHostPort\(""`),
	regexp.MustCompile(`PortFree\(""`),
	regexp.MustCompile(`IPv4zero|IPv6unspecified|IPv4\(0,\s*0,\s*0,\s*0\)`),
}

// wildcardAllowed are the lines that may match: product code that follows
// the configuration (an instance without bind_ip binds all interfaces), and
// tests that use such an address without binding it. Every entry must still
// match a line.
var wildcardAllowed = []struct{ file, line, why string }{
	{"internal/netbind/netbind.go", `host = "0.0.0.0"`,
		"an empty bind address means all interfaces, as configured; refused in test binaries"},
	{"internal/gameserver/gameserver.go", `if host == "" || host == "0.0.0.0" || host == "::" {`,
		"Probe dials: a server on all interfaces is asked on 127.0.0.1"},
	{"internal/config/config_test.go", `{"0.0.0.0:8740", `, "listen rule checks; nothing is bound"},
	{"internal/config/config_test.go", `{"[::]:8740", `, "listen rule checks; nothing is bound"},
	{"internal/config/config_test.go", `{":8740", `, "listen rule checks; nothing is bound"},
	{"internal/netbind/netbind_test.go", `PortFree("", 1)`, "refused before binding: the guard under test"},
	{"internal/netbind/netbind_test.go", `Listen("tcp", ":3")`, "refused before binding: the guard under test"},
	{"internal/netbind/netbind_test.go", `[]string{"0.0.0.0:1", "192.0.2.10:2", ":3"}`, "the refusals listed"},
	{"cmd/esm-fakeserver/main_test.go", `[]string{"0.0.0.0", "::", "192.0.2.10", "example.com"}`,
		"refused by the fake's option parsing; nothing is bound"},
	{"internal/testkit/sockets_test.go", `net.ParseIP("0.0.0.0")`, "socket table parsing; nothing is bound"},
	{"internal/testkit/sockets_test.go", `net.ParseIP("::")`, "socket table parsing; nothing is bound"},
	{"internal/testkit/sockets_test.go", `"tcp 0.0.0.0:8081"`, "socket table parsing; nothing is bound"},
	{"internal/api/host_test.go", `const allInterfaces = "0.0.0.0:8740"`, "Host header check; nothing is bound"},
}

// moduleRoot finds the folder holding go.mod above the working folder.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "module github.com/TriadStudios/echoes-server-manager\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("the source is not here (a test binary run away from its package folder)")
		}
		dir = parent
	}
}

// No Go file binds all interfaces, or builds such an address, except the
// allowed lines. Tests bind 127.0.0.1 only: a test binary or fake server
// on all interfaces makes a desktop firewall ask about it, with every
// build.
func TestNoWildcardListenAddresses(t *testing.T) {
	root := moduleRoot(t)
	used := make([]bool, len(wildcardAllowed))
	files := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); p != root && (strings.HasPrefix(name, ".") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".go") || rel == "internal/testkit/wildcard_test.go" {
			return nil
		}
		files++
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, re := range wildcardPatterns {
				if !re.MatchString(line) {
					continue
				}
				allowed := false
				for i, a := range wildcardAllowed {
					if a.file == rel && strings.Contains(line, a.line) {
						used[i], allowed = true, true
					}
				}
				if !allowed {
					t.Errorf("%s:%d binds or names all interfaces (%s): %s", rel, n, re, strings.TrimSpace(line))
				}
				break
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 50 {
		t.Fatalf("only %d Go files found under %s", files, root)
	}
	for i, a := range wildcardAllowed {
		if !used[i] {
			t.Errorf("allowed line not found any more, remove the entry: %s: %s", a.file, a.line)
		}
	}
}
