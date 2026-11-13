package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
)

// ManifestSpec is what a manifest says besides the files: the build's
// version, commit, channel, default map and executables. Skip lists top
// level names of the folder that are not part of the build.
type ManifestSpec struct {
	Version     string
	Commit      string
	Channel     string
	DefaultMap  string
	Executables map[string]string
	Skip        []string
}

// MakeManifest lists every file of the folder dir with its SHA-256 and
// returns the manifest for spec. The same rules apply as for an install:
// no links or special files, clean names that differ in more than case,
// at most maxFiles files (0: the default). A manifest.json at the top is
// not listed (it is what is being made). The manifest is checked for every
// system it names an executable for.
func MakeManifest(ctx context.Context, dir string, spec ManifestSpec, maxFiles int) (*Manifest, error) {
	if maxFiles <= 0 {
		maxFiles = 200000
	}
	man := &Manifest{Version: spec.Version, Commit: spec.Commit, Channel: spec.Channel, DefaultMap: spec.DefaultMap,
		Executables: maps.Clone(spec.Executables), Files: map[string]string{}}
	if len(man.Executables) == 0 {
		return nil, fault.Invalidf("name the server binary for at least one system (linux=<path> or windows=<path>)")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, "invalid", err, "cannot open the folder %s", dir)
	}
	defer root.Close()
	seen := map[string]bool{}
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if !strings.Contains(p, "/") && slices.Contains(spec.Skip, p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		clean, err := CleanName(p)
		if err != nil {
			return unsafeArchive("%v", err)
		}
		switch t := d.Type(); {
		case t&fs.ModeSymlink != 0:
			return unsafeArchive("%q is a symbolic link; links are not allowed in builds", p)
		case d.IsDir():
			return nil
		case !t.IsRegular():
			return unsafeArchive("%q is not a regular file", p)
		case clean == ManifestName:
			return nil
		}
		if key := strings.ToLower(clean); seen[key] {
			return unsafeArchive("%q appears twice (names differ only in case)", clean)
		} else {
			seen[key] = true
		}
		if len(man.Files) >= maxFiles {
			return fault.New(fault.Invalid, "too_large", "the build has more than %d files", maxFiles)
		}
		f, err := root.Open(filepath.FromSlash(p))
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		man.Files[clean] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	if err != nil {
		var fe *fault.Error
		if errors.As(err, &fe) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("read the build folder: %w", err)
	}
	for _, goos := range slices.Sorted(maps.Keys(man.Executables)) {
		if err := man.Check(goos); err != nil {
			return nil, err
		}
	}
	return man, nil
}

// Encode returns the manifest as indented JSON with a final newline.
func (m *Manifest) Encode() []byte {
	b, _ := json.MarshalIndent(m, "", "  ")
	return append(b, '\n')
}

// InstallFolder installs the build in the folder src (an absolute path
// outside the import folder rules: ESM's own Steam cache) as spec.Version.
// The folder is copied (without the names in spec.Skip) into the install
// work folder, a manifest is made for the copy, and the install finishes
// as any other: every hash checked, the Steam app id file placed, the
// files made read-only, the folder moved into builds/.
func (m *Manager) InstallFolder(ctx context.Context, src string, spec ManifestSpec) (Build, error) {
	if !ValidVersion(spec.Version) {
		return Build{}, fault.Invalidf("version %q is not a valid build version", spec.Version)
	}
	if m.st.Exists(buildDir(spec.Version)) {
		return Build{}, fault.New(fault.Conflict, "build_exists", "build %s is already installed", spec.Version)
	}
	work := tempDir + "/" + tempPrefix + randomName()
	tree := work + "/tree"
	defer func() {
		if err := m.st.RemoveAll(work); err != nil {
			m.log.Warn("cannot remove install work folder", "path", work, "err", err)
		}
	}()
	if err := m.st.MkdirAll(tree, 0o700); err != nil {
		return Build{}, err
	}
	sr, err := os.OpenRoot(src)
	if err != nil {
		return Build{}, fmt.Errorf("open %s: %w", src, err)
	}
	defer sr.Close()
	w := newWriter(m.st, tree, m.opts.Limits)
	if err := copyFolderSkip(ctx, sr, w, spec.Skip); err != nil {
		return Build{}, err
	}
	abs, err := m.st.Abs(tree)
	if err != nil {
		return Build{}, err
	}
	man, err := MakeManifest(ctx, abs, spec, m.opts.Limits.MaxFiles)
	if err != nil {
		return Build{}, err
	}
	if err := m.st.WriteFile(tree+"/"+ManifestName, man.Encode(), 0o400); err != nil {
		return Build{}, err
	}
	return m.finish(tree)
}
