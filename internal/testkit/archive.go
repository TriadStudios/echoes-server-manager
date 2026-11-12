package testkit

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Entry is one archive member for WriteZip and WriteTarGz. Name is used
// as given (so tests can write hostile names such as "../x" or "/etc/x").
type Entry struct {
	Name     string
	Body     []byte
	Mode     fs.FileMode // permission bits; 0 means 0644 (0755 for dirs)
	Dir      bool
	Symlink  string // when set, the entry is a symlink to this target
	Hardlink string // tar only: when set, the entry is a hard link to this name
	Device   bool   // tar only: a character device entry
}

// FolderEntries reads every file and directory below dir as entries with
// slash names relative to dir, keeping permission bits.
func FolderEntries(t testing.TB, dir string) []Entry {
	t.Helper()
	var out []Entry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := Entry{Name: filepath.ToSlash(rel), Mode: info.Mode().Perm(), Dir: d.IsDir()}
		if !d.IsDir() {
			if e.Body, err = os.ReadFile(p); err != nil {
				return err
			}
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// MakeZip writes the contents of dir to a .zip at dest.
func MakeZip(t testing.TB, dir, dest string) {
	t.Helper()
	WriteZip(t, dest, FolderEntries(t, dir))
}

// MakeTarGz writes the contents of dir to a .tar.gz at dest.
func MakeTarGz(t testing.TB, dir, dest string) {
	t.Helper()
	WriteTarGz(t, dest, FolderEntries(t, dir))
}

func mode(e Entry) fs.FileMode {
	if e.Mode != 0 {
		return e.Mode
	}
	if e.Dir {
		return 0o755
	}
	return 0o644
}

// WriteZip writes entries to a .zip at dest.
func WriteZip(t testing.TB, dest string, entries []Entry) {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.Name, Method: zip.Deflate, Modified: time.Unix(1700000000, 0)}
		switch {
		case e.Dir:
			if h.Name == "" || h.Name[len(h.Name)-1] != '/' {
				h.Name += "/"
			}
			h.SetMode(fs.ModeDir | mode(e))
		case e.Symlink != "":
			h.SetMode(fs.ModeSymlink | 0o777)
		default:
			h.SetMode(mode(e))
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case e.Symlink != "":
			_, err = w.Write([]byte(e.Symlink))
		case !e.Dir:
			_, err = w.Write(e.Body)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// WriteTarGz writes entries to a .tar.gz at dest.
func WriteTarGz(t testing.TB, dest string, entries []Entry) {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.Name, Mode: int64(mode(e)), ModTime: time.Unix(1700000000, 0), Format: tar.FormatPAX}
		switch {
		case e.Dir:
			h.Typeflag = tar.TypeDir
		case e.Symlink != "":
			h.Typeflag = tar.TypeSymlink
			h.Linkname = e.Symlink
		case e.Hardlink != "":
			h.Typeflag = tar.TypeLink
			h.Linkname = e.Hardlink
		case e.Device:
			h.Typeflag = tar.TypeChar
			h.Devmajor, h.Devminor = 1, 3
		default:
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.Body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}
