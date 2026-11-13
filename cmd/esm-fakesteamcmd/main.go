// Command esm-fakesteamcmd stands in for SteamCMD in tests. It runs the
// commands ESM gives SteamCMD, from its command line ("+command args ...")
// and from script files ("+runscript <file>"):
//
//	@ShutdownOnFailedCommand 1, @NoPromptForPassword 1   (settings, accepted)
//	force_install_dir "<folder>"                          (before app_update)
//	login anonymous                                       (only anonymous)
//	app_update <app> [-beta <branch> [-betapassword <pw>]] [validate]
//	quit
//
// What it serves comes from fakesteamcmd.json next to its executable:
//
//	{"app_id": "1000001", "buildid": "100", "content": "<folder>",
//	 "branch": "testing", "password": "<the branch password>",
//	 "record": "<file>"}
//
// app_update copies the content folder into the install folder (replacing
// what was there, except steamapps/) and writes
// steamapps/appmanifest_<app>.acf with the build id, like SteamCMD. A wrong
// branch password is refused the way SteamCMD refuses it ("ERROR! ...
// (Invalid Password)", exit code 5).
//
// Every run appends one JSON line to the record file: its command line,
// its HOME, and the permission bits of each script file and its folder.
// When the branch password appears anywhere on its command line it also
// writes "<record>.leak": tests check that this never happens.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type control struct {
	AppID    string `json:"app_id"`
	BuildID  string `json:"buildid"`
	Content  string `json:"content"`
	Branch   string `json:"branch"`
	Password string `json:"password"`
	Record   string `json:"record"`
}

type record struct {
	Args    []string `json:"args"`
	Home    string   `json:"home"`
	Scripts []string `json:"scripts"` // "<file mode> <folder mode>" per script
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	c, err := loadControl()
	if err != nil {
		fmt.Println("ERROR! fake SteamCMD:", err)
		return 2
	}
	rec := record{Args: args, Home: os.Getenv("HOME")}
	defer func() { writeRecord(c, rec, args) }()
	fmt.Println("Steam Console Client (c) Valve Corporation - fake for tests")
	cmds, err := commands(args, &rec)
	if err != nil {
		fmt.Println("ERROR!", err)
		return 2
	}
	s := &session{c: c}
	for _, cmd := range cmds {
		if code := s.do(cmd); code != 0 {
			return code
		}
	}
	return 0
}

func loadControl() (control, error) {
	var c control
	exe, err := os.Executable()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "fakesteamcmd.json"))
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func writeRecord(c control, rec record, args []string) {
	if c.Record == "" {
		return
	}
	if c.Password != "" {
		for _, a := range args {
			if strings.Contains(a, c.Password) {
				os.WriteFile(c.Record+".leak", []byte("the branch password was on the command line\n"), 0o600)
			}
		}
	}
	b, _ := json.Marshal(rec)
	f, err := os.OpenFile(c.Record, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// commands splits the command line into commands and reads script files.
func commands(args []string, rec *record) ([][]string, error) {
	var out [][]string
	for i := 0; i < len(args); {
		if !strings.HasPrefix(args[i], "+") {
			return nil, fmt.Errorf("unexpected argument %q", args[i])
		}
		cmd := []string{strings.TrimPrefix(args[i], "+")}
		i++
		for i < len(args) && !strings.HasPrefix(args[i], "+") {
			cmd = append(cmd, args[i])
			i++
		}
		if cmd[0] != "runscript" {
			out = append(out, cmd)
			continue
		}
		if len(cmd) != 2 {
			return nil, errors.New("runscript needs one file")
		}
		fi, err := os.Stat(cmd[1])
		if err != nil {
			return nil, err
		}
		di, err := os.Stat(filepath.Dir(cmd[1]))
		if err != nil {
			return nil, err
		}
		rec.Scripts = append(rec.Scripts, fmt.Sprintf("%o %o", fi.Mode().Perm(), di.Mode().Perm()))
		b, err := os.ReadFile(cmd[1])
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "//") {
				continue
			}
			words, err := split(line)
			if err != nil {
				return nil, err
			}
			out = append(out, words)
		}
	}
	return out, nil
}

// split cuts a script line into words; a double-quoted part is one word.
func split(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in, has := false, false
	for _, r := range line {
		switch {
		case r == '"':
			in, has = !in, true
		case (r == ' ' || r == '\t') && !in:
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if in {
		return nil, fmt.Errorf("unclosed quote in %q", line)
	}
	if has {
		out = append(out, cur.String())
	}
	return out, nil
}

type session struct {
	c        control
	dir      string
	loggedIn bool
}

func (s *session) do(cmd []string) int {
	switch name := strings.ToLower(cmd[0]); {
	case strings.HasPrefix(name, "@"):
		return 0
	case name == "force_install_dir":
		if len(cmd) != 2 || !filepath.IsAbs(cmd[1]) {
			fmt.Println("ERROR! force_install_dir needs one absolute folder")
			return 6
		}
		s.dir = cmd[1]
	case name == "login":
		if len(cmd) != 2 || cmd[1] != "anonymous" {
			fmt.Println("ERROR! the fake only logs in anonymously")
			return 5
		}
		fmt.Println("Logging in user 'anonymous' to Steam Public...OK")
		s.loggedIn = true
	case name == "app_update":
		return s.appUpdate(cmd[1:])
	case name == "quit":
		return 0
	default:
		fmt.Printf("ERROR! unknown command %q\n", cmd[0])
		return 7
	}
	return 0
}

func (s *session) appUpdate(args []string) int {
	if s.dir == "" || !s.loggedIn {
		fmt.Println("ERROR! force_install_dir and login must come before app_update")
		return 8
	}
	if len(args) == 0 {
		fmt.Println("ERROR! app_update needs an app id")
		return 8
	}
	app, branch, pw := args[0], "", ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-beta", "-betapassword":
			if i+1 >= len(args) {
				fmt.Printf("ERROR! %s needs a value\n", args[i])
				return 8
			}
			if args[i] == "-beta" {
				branch = args[i+1]
			} else {
				pw = args[i+1]
			}
			i++
		case "validate":
		default:
			fmt.Printf("ERROR! unknown app_update option %q\n", args[i])
			return 8
		}
	}
	if app != s.c.AppID {
		fmt.Printf("ERROR! Failed to install app '%s' (No subscription)\n", app)
		return 8
	}
	if branch != s.c.Branch {
		fmt.Printf("ERROR! Failed to install app '%s' (Invalid beta branch)\n", app)
		return 8
	}
	if s.c.Password != "" && pw != s.c.Password {
		fmt.Printf("ERROR! Failed to install app '%s' (Invalid Password)\n", app)
		return 5
	}
	fmt.Printf(" Update state (0x61) downloading, progress: 50.00 (1 / 2)\r Update state (0x61) downloading, progress: 100.00 (2 / 2)\n")
	if err := mirror(s.c.Content, s.dir); err != nil {
		fmt.Printf("ERROR! Failed to install app '%s' (Disk write failure: %v)\n", app, err)
		return 8
	}
	acf := fmt.Sprintf("\"AppState\"\n{\n\t\"appid\"\t\t\"%s\"\n\t\"Universe\"\t\t\"1\"\n\t\"name\"\t\t\"Fake Dedicated Server\"\n"+
		"\t\"StateFlags\"\t\t\"4\"\n\t\"installdir\"\t\t\"Fake\"\n\t\"buildid\"\t\t\"%s\"\n\t\"UserConfig\"\n\t{\n\t\t\"BetaKey\"\t\t\"%s\"\n\t}\n}\n",
		app, s.c.BuildID, branch)
	if err := os.MkdirAll(filepath.Join(s.dir, "steamapps"), 0o755); err == nil {
		err = os.WriteFile(filepath.Join(s.dir, "steamapps", "appmanifest_"+app+".acf"), []byte(acf), 0o644)
	}
	fmt.Printf("Success! App '%s' fully installed.\n", app)
	return 0
}

// mirror makes dst hold exactly what src holds (steamapps/ in dst stays).
func mirror(src, dst string) error {
	entries, err := os.ReadDir(dst)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if e.Name() == "steamapps" {
			continue
		}
		p := filepath.Join(dst, e.Name())
		makeWritable(p)
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// Steam depots can hold links; they come over as links.
			dest, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(dest, target)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm()|0o200)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	})
}

func makeWritable(p string) {
	filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err == nil {
			if fi, err := d.Info(); err == nil {
				os.Chmod(q, fi.Mode().Perm()|0o700)
			}
		}
		return nil
	})
}
