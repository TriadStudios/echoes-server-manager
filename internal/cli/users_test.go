package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
)

// A password line is read a byte at a time, without its line end, and
// nothing after it is taken; a line that is far too long is refused.
func TestReadLine(t *testing.T) {
	for in, want := range map[string]string{
		"a long password 1\n":         "a long password 1",
		"a long password 1\r\nnext\n": "a long password 1",
		"no line end":                 "no line end",
		"\n":                          "",
		"":                            "",
		" spaces kept \n":             " spaces kept ",
	} {
		r := strings.NewReader(in)
		got, err := readLine(r)
		if err != nil || got != want {
			t.Errorf("readLine(%q) = %q, %v; want %q", in, got, err, want)
		}
		if in == "a long password 1\r\nnext\n" {
			if rest, _ := io.ReadAll(r); string(rest) != "next\n" {
				t.Errorf("readLine took more than its line: %q left", rest)
			}
		}
	}
	if _, err := readLine(strings.NewReader(strings.Repeat("x", maxPasswordInput+1) + "\n")); err == nil {
		t.Error("a line longer than the bound was read")
	}
}

// Only a terminal is asked: a file or a pipe on standard input is read as
// one line, without a prompt.
func TestFilesAndPipesAreNoTerminal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "pw"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("a file is taken for a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Error("a pipe is taken for a terminal")
	}
	go func() { io.WriteString(w, "piped password 1\nmore\n") }()
	var errOut bytes.Buffer
	e := &env{in: r, errOut: &errOut}
	pw, err := e.newPassword("admin")
	if err != nil || pw != "piped password 1" || errOut.Len() != 0 {
		t.Fatalf("from a pipe: %q %v, prompt %q", pw, err, errOut.String())
	}
}

// user passwd takes the login's name and nothing else, and never repeats
// what else it was given (it may be the password): not as an argument, not
// as the name or the value of a flag, before the verb or after it, not as
// a root it cannot reach. With --url it is refused before anything is read
// or sent: it works on the box only.
func TestUserPasswdTakesNoPasswordArgument(t *testing.T) {
	const secret = "Typed-In-The-Wrong-Place-9"
	for _, args := range [][]string{
		{"user", "passwd", "admin", secret},
		{"user", "passwd", secret, "admin"},
		{"user", "passwd", "admin", "--", secret},
		{"user", "passwd", "--password=" + secret, "admin"},
		{"user", "passwd", "admin", "--new", secret},
		{"user", "passwd"},
		{"--url", "http://127.0.0.1:1", "--token", "esm_x", "user", "passwd", "admin"},
		{"user", "passwd", "admin", "--json=" + secret},
		{"--json", "user", "passwd", "admin", "--json=" + secret},
		{"user", "passwd", "admin", "-" + secret},
		{"user", "passwd", "admin", "--" + secret + "=x"},
		{"user", "--json=" + secret, "passwd", "admin"},
		{"--json=" + secret, "user", "passwd", "admin"},
		{"-" + secret, "user", "passwd", "admin"},
		{"user", "passwd", "admin", "--root=" + secret},
		{"user", "passwd", "admin", "--root", secret},
	} {
		var out, errOut bytes.Buffer
		stdin := strings.NewReader(secret + "\n")
		code := MainIn(append([]string{"--root", t.TempDir()}, args...), stdin, &out, &errOut, "test")
		if code != 1 || strings.Contains(out.String()+errOut.String(), secret) {
			t.Errorf("%q: %d %q %q", args, code, out.String(), errOut.String())
		}
		if stdin.Len() != len(secret)+1 {
			t.Errorf("%q: standard input was read", args)
		}
	}
}

func TestDescribeUsers(t *testing.T) {
	if got := describeUsers(nil); !strings.HasPrefix(got, "no logins") {
		t.Errorf("no users: %q", got)
	}
	got := describeUsers([]api.UserView{
		{Username: "admin", Role: "admin", LastLogin: time.Now().Add(-time.Hour), Sessions: 1},
		{Username: "ops", Role: "operator"},
	})
	if !strings.HasPrefix(got, "2 logins: admin (admin, last login 1h0m0s ago, 1 session open), ops (operator, never logged in, 0 sessions open)") {
		t.Errorf("describeUsers = %q", got)
	}
}
