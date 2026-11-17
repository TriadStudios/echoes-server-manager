// Package svcunit writes the systemd user unit that runs ESM itself
// (esm service install) and the shell script the unit runs before every
// start, which puts the old binary back when a self-updated ESM does not
// come up. It writes these two files and runs nothing.
package svcunit

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Name is the unit's name.
const Name = "esm.service"

// ScriptSuffix names the rollback script of a binary: exe+ScriptSuffix, next
// to the binary.
const ScriptSuffix = ".update.sh"

// rollbackScript is the script the unit runs before every start (see
// rollback.sh for what it does and why it is a file of its own).
//
//go:embed rollback.sh
var rollbackScript []byte

// Script returns the rollback script (a copy).
func Script() []byte { return bytes.Clone(rollbackScript) }

// ScriptPath is where the rollback script of the binary exe is written and
// where the unit looks for it.
func ScriptPath(exe string) string { return exe + ScriptSuffix }

// Dir returns the folder of systemd user units: $XDG_CONFIG_HOME/systemd/user,
// or ~/.config/systemd/user.
func Dir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot find the home folder: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user"), nil
}

// quote makes one word of a unit's command line: in double quotes, with
// "%" (systemd specifiers) and "$" (variables) doubled. Paths with quotes,
// backslashes or control characters are refused.
func quote(s string) (string, error) {
	if strings.ContainsAny(s, "\"\\") || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("%q has a quote, a backslash or a control character; move it to a plain path", s)
	}
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	return `"` + s + `"`, nil
}

// Unit returns the unit for the binary exe and the root, both absolute.
//
// Restart=always brings ESM back after it ends, also after a self-update.
// StartLimitIntervalSec=0 keeps systemd from giving up on a start that keeps
// failing (RestartSec=5 spaces the starts out): the rollback of a failed
// self-update happens at the 3rd start and needs that many.
//
// ExecStartPre runs the rollback script (rollback.sh) with the path of the
// binary before every start. The "-" in front makes systemd ignore its
// failure and a missing script or /bin/sh: it can never keep ESM from
// starting. The script is a file next to the binary (see WriteScript) and not
// text in the unit, so that the unit's quoting rules have nothing to do with
// it.
//
// KillMode=process: stopping or restarting ESM ends ESM alone. Game
// servers that the process supervisor started are ESM's children and live
// in this unit's control group; with the default KillMode systemd would
// end them with ESM. (The systemd supervisor runs each server in a unit of
// its own anyway.) ESM leaves its servers running when it stops and adopts
// them when it starts.
func Unit(exe, root string) ([]byte, error) {
	if !filepath.IsAbs(exe) || !filepath.IsAbs(root) {
		return nil, errors.New("the binary and the root must be absolute paths")
	}
	qe, err := quote(exe)
	if err != nil {
		return nil, err
	}
	qr, err := quote(root)
	if err != nil {
		return nil, err
	}
	qs, err := quote(ScriptPath(exe))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("# Written by \"esm service install\". Echoes Server Manager for this user.\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Echoes Server Manager\n")
	b.WriteString("StartLimitIntervalSec=0\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=exec\n")
	fmt.Fprintf(&b, "ExecStartPre=-/bin/sh %s %s\n", qs, qe)
	fmt.Fprintf(&b, "ExecStart=%s serve --root %s\n", qe, qr)
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("KillMode=process\n")
	b.WriteString("KillSignal=SIGTERM\n")
	b.WriteString("TimeoutStopSec=60\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.Bytes(), nil
}

// Write writes the unit into dir and returns its path. An existing,
// different unit is replaced only with force.
func Write(dir, exe, root string, force bool) (string, error) {
	u, err := Unit(exe, root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, Name)
	old, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(old, u):
		return path, nil
	case err == nil && !force:
		return "", fmt.Errorf("%s exists and differs; pass --force to replace it (a unit written by an older esm lacks the rollback of a failed self-update)", path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, u, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// WriteScript writes the rollback script that the unit for the binary exe
// runs (exe+ScriptSuffix, next to the binary, where self-updates keep their
// files) and returns its path. A script that is there and the same is left
// alone, and one that differs is replaced: it belongs to ESM, not to the
// owner. The unit works without it (it is ignored when missing), so a binary
// in a folder ESM cannot write to still gets its unit; only the rollback is
// then missing.
func WriteScript(exe string) (string, error) {
	path := ScriptPath(exe)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, rollbackScript) {
		return path, nil
	}
	if err := putScript(path); err != nil {
		return "", err
	}
	return path, nil
}

// RefreshScript brings an existing rollback script up to date: when the
// script next to the binary exe is there and differs from the one built into
// this ESM, it is replaced (a file of its own is written and moved over the
// name), so that a self-update, which replaces the binary only, also brings
// the script its new binary expects. It never makes a script that is not
// there: a box without the unit stays as clean as it was, and one whose unit
// has no script (esm service install could not write it) is the owner's to
// fix. changed says whether it wrote one.
func RefreshScript(exe string) (changed bool, err error) {
	path := ScriptPath(exe)
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case bytes.Equal(old, rollbackScript):
		return false, nil
	}
	if err := putScript(path); err != nil {
		return false, err
	}
	return true, nil
}

// putScript writes the script atomically: to a file of its own in the same
// folder, moved over path, so the shell never reads half of it.
func putScript(path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".esm-script-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(rollbackScript)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
