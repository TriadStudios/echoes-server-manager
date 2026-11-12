package builds

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// Archive kinds.
const (
	kindZip   = "zip"
	kindTarGz = "tar.gz"
)

// archiveKind tells the archive type from a file name, or "".
func archiveKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"):
		return kindZip
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return kindTarGz
	}
	return ""
}

func unsafeArchive(format string, args ...any) error {
	return fault.New(fault.Invalid, "unsafe_archive", format, args...)
}

// writer places files below a root-relative folder, refusing anything that
// is not a plain file or folder, duplicates (also those that differ only in
// case) and builds above the size and file-count limits.
type writer struct {
	st     *store.Store
	dst    string // root-relative slash path of the target folder
	limits Limits
	seen   map[string]bool // lower-case names
	files  int
	bytes  int64
}

func newWriter(st *store.Store, dst string, l Limits) *writer {
	return &writer{st: st, dst: dst, limits: l, seen: make(map[string]bool)}
}

func (w *writer) claim(name string) (string, error) {
	clean, err := CleanName(name)
	if err != nil {
		return "", unsafeArchive("%v", err)
	}
	key := strings.ToLower(clean)
	if w.seen[key] {
		return "", unsafeArchive("%q appears twice", clean)
	}
	w.seen[key] = true
	return clean, nil
}

func (w *writer) dir(name string) error {
	clean, err := CleanName(name)
	if err != nil {
		return unsafeArchive("%v", err)
	}
	if w.seen[strings.ToLower(clean)] {
		return nil // a folder may be named more than once
	}
	return w.st.MkdirAll(w.dst+"/"+clean, 0o700)
}

// file writes one file. Executable bits in mode are kept (as 0500); every
// file is otherwise read-only (0400).
func (w *writer) file(name string, mode fs.FileMode, r io.Reader) error {
	clean, err := w.claim(name)
	if err != nil {
		return err
	}
	w.files++
	if w.files > w.limits.MaxFiles {
		return fault.New(fault.Invalid, "too_large", "the build has more than %d files", w.limits.MaxFiles)
	}
	perm := fs.FileMode(0o400)
	if mode&0o111 != 0 {
		perm = 0o500
	}
	f, err := w.st.Create(w.dst+"/"+clean, perm)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return unsafeArchive("%q appears twice", clean)
		}
		return err
	}
	left := w.limits.MaxBytes - w.bytes
	n, err := io.Copy(f, io.LimitReader(r, left+1))
	w.bytes += n
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", clean, err)
	}
	if n > left {
		return fault.New(fault.Invalid, "too_large", "the build is larger than %d bytes", w.limits.MaxBytes)
	}
	return nil
}

// unpackZip extracts a .zip file into the writer's folder.
func unpackZip(path string, w *writer) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return fault.Wrap(fault.Invalid, "bad_archive", err, "cannot read the zip archive")
	}
	defer zr.Close()
	for _, f := range zr.File {
		mode := f.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			return unsafeArchive("%q is a symbolic link; links are not allowed in builds", f.Name)
		case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
			if err := w.dir(f.Name); err != nil {
				return err
			}
		case !mode.IsRegular():
			return unsafeArchive("%q is not a regular file", f.Name)
		default:
			rc, err := f.Open()
			if err != nil {
				return fault.Wrap(fault.Invalid, "bad_archive", err, "cannot read %q", f.Name)
			}
			err = w.file(f.Name, mode, rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// unpackTarGz extracts a .tar.gz file into the writer's folder.
func unpackTarGz(path string, w *writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fault.Wrap(fault.Invalid, "bad_archive", err, "cannot read the gzip stream")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fault.Wrap(fault.Invalid, "bad_archive", err, "cannot read the tar archive")
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA: // old archives still use TypeRegA
			if err := w.file(h.Name, fs.FileMode(h.Mode), tr); err != nil {
				return err
			}
		case tar.TypeDir:
			if err := w.dir(h.Name); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
			// PAX global headers carry no file.
		case tar.TypeSymlink:
			return unsafeArchive("%q is a symbolic link; links are not allowed in builds", h.Name)
		case tar.TypeLink:
			return unsafeArchive("%q is a hard link; links are not allowed in builds", h.Name)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return unsafeArchive("%q is a device or pipe", h.Name)
		default:
			return unsafeArchive("%q has an unsupported entry type %q", h.Name, h.Typeflag)
		}
	}
}

// copyFolder copies a plain folder (no links, no special files) into the
// writer's folder. Files are read through an os.Root on the source, so a
// file swapped for a link that leaves the folder cannot be followed.
func copyFolder(src string, w *writer) error {
	sr, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer sr.Close()
	return fs.WalkDir(sr.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		switch t := d.Type(); {
		case t&fs.ModeSymlink != 0:
			return unsafeArchive("%q is a symbolic link; links are not allowed in builds", p)
		case d.IsDir():
			return w.dir(p)
		case !t.IsRegular():
			return unsafeArchive("%q is not a regular file", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f, err := sr.Open(filepath.FromSlash(p))
		if err != nil {
			return err
		}
		defer f.Close()
		return w.file(p, info.Mode(), f)
	})
}
