package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/release"
	"github.com/TriadStudios/echoes-server-manager/internal/svcunit"
)

// releaseClient is the client for ESM releases (tests replace it).
var releaseClient = func(version string) *release.Client {
	return &release.Client{UserAgent: "esm/" + version}
}

// VersionResult is what version prints with --json.
type VersionResult struct {
	Version string          `json:"version"`
	Release *release.Status `json:"release,omitempty"`
}

func (e *env) versionCmd(args []string) error {
	fs := e.flags("version")
	check := fs.Bool("check", false, "look up the newest release (on GitHub, over https)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if !*check {
		return e.result(VersionResult{Version: e.version}, "esm "+e.version)
	}
	st := releaseClient(e.version).Check(e.ctx, e.version)
	line := "esm " + e.version
	switch {
	case st.Latest == "":
		return fmt.Errorf("esm %s; %s", e.version, st.Note)
	case st.Available:
		line += "; release " + st.Latest + " is newer (esm update-self puts it in place)"
	case st.Note != "":
		line += "; newest release " + st.Latest + " (" + st.Note + ")"
	default:
		line += "; this is the newest release"
	}
	return e.result(VersionResult{Version: e.version, Release: &st}, line)
}

func (e *env) updateSelf(args []string) error {
	fs := e.flags("update-self")
	force := fs.Bool("force", false, "take the newest release even when it is not newer")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("update-self takes no arguments")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	res, err := releaseClient(e.version).Update(e.ctx, e.version, exe, runtime.GOOS, runtime.GOARCH, *force)
	if err != nil {
		return err
	}
	return e.result(res, fmt.Sprintf("esm updated from %s to %s at %s (the old binary is %s); restart the service to run it: systemctl --user restart esm",
		res.From, res.To, res.Binary, res.Previous))
}

// --- service install ---

func (e *env) serviceCmd(args []string) error {
	fs := e.flags("service")
	force := fs.Bool("force", false, "replace an existing unit file")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] != "install" {
		return usageErr("service needs install [--force]")
	}
	if runtime.GOOS != "linux" {
		return errors.New("esm service install writes a systemd user unit; it is for Linux only")
	}
	if err := config.CheckNotRoot(); err != nil {
		return err
	}
	root, err := config.ResolveRoot(e.g.root)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	dir, err := svcunit.Dir()
	if err != nil {
		return err
	}
	path, err := svcunit.Write(dir, exe, root, *force)
	if err != nil {
		return err
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "<this user>"
	}
	return e.result(map[string]string{"unit": path, "exec": exe, "root": root},
		fmt.Sprintf("wrote %s; start it with: systemctl --user daemon-reload && systemctl --user enable --now %s. "+
			"To keep it running after you log out, an admin runs once: sudo loginctl enable-linger %s (ESM does not run this)",
			path, svcunit.Name, user))
}

// --- build manifest ---

// multiKV collects repeatable key=value flags.
type multiKV map[string]string

func (m multiKV) String() string { return "" }
func (m multiKV) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" || val == "" {
		return fmt.Errorf("want <system>=<path>, like linux=Server/Binaries/Linux/Server")
	}
	if _, dup := m[k]; dup {
		return fmt.Errorf("%s is given twice", k)
	}
	m[k] = val
	return nil
}

func (e *env) buildManifest(args []string) error {
	fs := e.flags("build manifest")
	version := fs.String("version", "", "the build's version")
	channel := fs.String("channel", builds.ChannelLive, "live or test")
	commit := fs.String("commit", "", "the commit or source the build was made from")
	defMap := fs.String("default-map", "", "the map a server loads when its instance names none")
	exes := multiKV{}
	fs.Var(exes, "executable", "<system>=<path of the server binary inside the folder> (repeatable: linux=..., windows=...)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := want("build manifest", pos, 1, "a folder"); err != nil {
		return err
	}
	if *version == "" {
		return usageErr("build manifest needs --version")
	}
	for goos := range exes {
		if goos != "linux" && goos != "windows" {
			return usageErr("--executable %s=...: the system is linux or windows", goos)
		}
	}
	dir, err := filepath.Abs(pos[0])
	if err != nil {
		return err
	}
	man, err := builds.MakeManifest(e.ctx, dir, builds.ManifestSpec{Version: *version, Commit: *commit, Channel: *channel,
		DefaultMap: *defMap, Executables: exes}, 0)
	if err != nil {
		return err
	}
	out := filepath.Join(dir, builds.ManifestName)
	if err := os.WriteFile(out, man.Encode(), 0o644); err != nil {
		return err
	}
	systems := slices.Sorted(maps.Keys(man.Executables))
	return e.result(man, fmt.Sprintf("wrote %s: build %s (%s), %s, server binary for %s",
		out, man.Version, man.Channel, plural(len(man.Files), "file", "files"), strings.Join(systems, " and ")))
}

// --- steam ---

func (e *env) steamCmd(args []string, stdin io.Reader) error {
	fs := e.flags("steam")
	clear := fs.Bool("clear", false, "remove the password")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr("steam needs list or password <name>")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	switch pos[0] {
	case "list":
		if len(pos) != 1 {
			return usageErr("steam list takes no arguments")
		}
		var out api.SteamList
		if err := c.do(e.ctx, "GET", "/api/v1/steam", nil, &out); err != nil {
			return err
		}
		if len(out.Sources) == 0 {
			return e.result(out, "no Steam sources (steam_sources in "+config.FileName+")")
		}
		parts := make([]string, len(out.Sources))
		for i, s := range out.Sources {
			state := "never checked"
			switch {
			case s.Running:
				state = "SteamCMD runs now"
			case s.Error != "":
				state = "last check failed: " + s.Error
			case s.Version != "":
				state = "at " + s.Version
			}
			branch := "default branch"
			if s.Branch != "" {
				branch = "branch " + s.Branch + cond(!s.PasswordSet, " (no password set)")
			}
			parts[i] = fmt.Sprintf("%s (app %s, %s, %s): %s", s.Name, s.AppID, branch, s.Channel, state)
		}
		return e.result(out, strings.Join(parts, "; "))
	case "password":
		if err := want("steam password", pos[1:], 1, "a source name"); err != nil {
			return err
		}
		pw := ""
		if !*clear {
			if pw, err = readSecretLine(stdin); err != nil {
				return err
			}
		}
		var out api.SteamPasswordResult
		if err := c.do(e.ctx, "PUT", "/api/v1/steam/"+url.PathEscape(pos[1])+"/password", api.SteamPasswordRequest{Password: pw}, &out); err != nil {
			return err
		}
		if !out.PasswordSet {
			return e.result(out, "removed the branch password of "+out.Name)
		}
		return e.result(out, "set the branch password of "+out.Name)
	}
	return usageErr("unknown steam command %q (list or password)", pos[0])
}

// readSecretLine reads one line from stdin (never from the command line,
// where other users could see it).
func readSecretLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", usageErr("give the password on standard input, like: esm steam password <name> < file (or --clear to remove it)")
	}
	return line, nil
}
