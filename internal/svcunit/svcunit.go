// Package svcunit writes the systemd user unit that runs ESM itself
// (esm service install). It writes one file and runs nothing.
package svcunit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Name is the unit's name.
const Name = "esm.service"

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
	var b bytes.Buffer
	b.WriteString("# Written by \"esm service install\". Echoes Server Manager for this user.\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Echoes Server Manager\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("After=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=exec\n")
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
		return "", fmt.Errorf("%s exists and differs; pass --force to replace it", path)
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
