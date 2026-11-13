package doctor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
)

// elfClass64 is the ELF header's class byte for 64-bit files.
const elfClass64 = 2

// checkELF64 reads the start of the file at path and reports whether it is
// a 64-bit ELF file, and if not, why. It runs no tool.
func checkELF64(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var head [5]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return errors.New("is not an ELF file (too short)")
	}
	if string(head[:4]) != "\x7fELF" {
		return errors.New("is not an ELF file")
	}
	if head[4] != elfClass64 {
		return errors.New("is a 32-bit ELF file; the 64-bit one is needed")
	}
	return nil
}

// checkSteamAppID reports whether the Steam app id is configured.
func checkSteamAppID(r *Report, cfg *config.Config) {
	if cfg.SteamAppID == "" {
		r.add("steam_app_id", Warn, "steam_app_id is not set in %s: builds get no %s next to their server binary, and a server writes it there itself at start; set it before installing builds",
			config.FileName, gameserver.SteamAppIDFile)
		return
	}
	r.add("steam_app_id", OK, "steam_app_id is set; every build installed from now on gets %s next to its server binary", gameserver.SteamAppIDFile)
}

// checkSteamRuntime checks the folder that holds the Steam client
// libraries: it exists, and each library is there as a 64-bit ELF file.
func checkSteamRuntime(r *Report, dir, goos string) {
	if goos != "linux" {
		r.add("steam_runtime", OK, "not checked on %s (the Steam client libraries are a Linux server's)", goos)
		return
	}
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.add("steam_runtime", Fail, "%s does not exist: put the 64-bit %s from the Steamworks SDK redistributable there (or set steam_runtime_dir)",
			dir, strings.Join(gameserver.SteamLibraries, " and "))
		return
	case err != nil:
		r.add("steam_runtime", Fail, "%s: %v", dir, err)
		return
	case !fi.IsDir():
		r.add("steam_runtime", Fail, "%s is not a folder", dir)
		return
	}
	var problems []string
	for _, lib := range gameserver.SteamLibraries {
		p := filepath.Join(dir, lib)
		fi, err := os.Stat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			problems = append(problems, lib+" is missing")
			continue
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", lib, err))
			continue
		case !fi.Mode().IsRegular():
			problems = append(problems, lib+" is not a regular file")
			continue
		}
		if err := checkELF64(p); err != nil {
			problems = append(problems, lib+" "+err.Error())
		}
	}
	if len(problems) > 0 {
		r.add("steam_runtime", Fail, "%s: %s", dir, strings.Join(problems, "; "))
		return
	}
	r.add("steam_runtime", OK, "%s holds 64-bit %s; servers get it first on %s", dir, strings.Join(gameserver.SteamLibraries, " and "),
		gameserver.LibraryPathVar)
}

// checkBuilds checks every installed build: its server binary for goos
// exists and is a regular executable file (a 64-bit ELF file on Linux), and
// when an app id is configured, the app id file next to it holds it.
func checkBuilds(r *Report, root, goos, appID string) {
	top := filepath.Join(root, builds.Dir)
	entries, err := os.ReadDir(top)
	if errors.Is(err, fs.ErrNotExist) {
		r.add("builds", OK, "no builds installed yet")
		return
	}
	if err != nil {
		r.add("builds", Warn, "cannot read %s: %v", top, err)
		return
	}
	var problems, notes []string
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !builds.ValidVersion(e.Name()) {
			continue
		}
		dir := filepath.Join(top, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, builds.ManifestName))
		if err != nil {
			continue // not a build
		}
		n++
		man, err := builds.ParseManifest(b)
		if err != nil {
			problems = append(problems, e.Name()+": "+err.Error())
			continue
		}
		rel, ok := man.Executables[goos]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: no server binary for %s", e.Name(), goos))
			continue
		}
		exe := filepath.Join(dir, filepath.FromSlash(rel))
		fi, err := os.Lstat(exe)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: server binary %s: %v", e.Name(), rel, err))
			continue
		case !fi.Mode().IsRegular():
			problems = append(problems, fmt.Sprintf("%s: server binary %s is not a regular file", e.Name(), rel))
			continue
		case goos != "windows" && runtime.GOOS != "windows" && fi.Mode().Perm()&0o100 == 0: // Windows files have no such bit
			problems = append(problems, fmt.Sprintf("%s: server binary %s is not executable", e.Name(), rel))
			continue
		}
		if goos == "linux" {
			if err := checkELF64(exe); err != nil {
				problems = append(problems, fmt.Sprintf("%s: server binary %s %s", e.Name(), rel, err))
				continue
			}
		}
		if appID != "" {
			idFile := filepath.Join(filepath.Dir(exe), gameserver.SteamAppIDFile)
			got, err := os.ReadFile(idFile)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				notes = append(notes, fmt.Sprintf("%s has no %s (installed before steam_app_id was set?)", e.Name(), gameserver.SteamAppIDFile))
			case err != nil:
				notes = append(notes, fmt.Sprintf("%s: %s: %v", e.Name(), gameserver.SteamAppIDFile, err))
			case strings.TrimSpace(string(got)) != appID:
				notes = append(notes, fmt.Sprintf("%s: %s names another app id than steam_app_id", e.Name(), gameserver.SteamAppIDFile))
			}
		}
	}
	slices.Sort(problems)
	slices.Sort(notes)
	switch {
	case len(problems) > 0:
		r.add("builds", Fail, "%s", strings.Join(append(problems, notes...), "; "))
	case len(notes) > 0:
		r.add("builds", Warn, "%s", strings.Join(notes, "; "))
	case n == 0:
		r.add("builds", OK, "no builds installed yet")
	default:
		r.add("builds", OK, "%s: server binary present and executable", plural(n, "build", "builds"))
	}
}
