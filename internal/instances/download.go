package instances

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
)

// BackupArchive checks a backup and returns it with a function that writes
// it as a gzip-compressed tar archive: its meta.json and save files, inside
// one folder named "<instance>-<backup id>". Every file the backup names is
// checked (see checkBackupFiles) before anything is written, so a damaged
// backup is refused with an error instead of a broken archive. Backups of a
// deleted instance can still be fetched by name.
func (m *Manager) BackupArchive(name, id string) (Backup, func(io.Writer) error, error) {
	if !ValidName(name) || !ValidBackupID(id) {
		return Backup{}, nil, fault.NotFoundf("backup %q of instance %q not found", id, name)
	}
	dir := backupDir(name, id)
	var b Backup
	if err := m.st.ReadJSON(dir+"/meta.json", &b); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Backup{}, nil, fault.NotFoundf("backup %q of instance %s not found", id, name)
		}
		return Backup{}, nil, err
	}
	b.ID = id
	if err := m.checkBackupFiles(name, id, b.Files); err != nil {
		return Backup{}, nil, err
	}
	top := name + "-" + id
	write := func(w io.Writer) error {
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		for _, f := range append([]string{"meta.json"}, b.Files...) {
			if err := m.addToTar(tw, dir+"/"+f, top+"/"+f); err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	}
	return b, write, nil
}

// addToTar adds the root-relative file rel to tw under the name name.
func (m *Manager) addToTar(tw *tar.Writer, rel, name string) error {
	f, err := m.st.Open(rel)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", rel)
	}
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: fi.Size(), ModTime: fi.ModTime().UTC().Truncate(time.Second)}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, fi.Size())
	return err
}

// MaxLogDownloadBytes bounds a log download: it holds at most this much of
// the newest part of the file.
const MaxLogDownloadBytes = 16 << 20

// logDownloadCut is the first line of a log download that left out the
// start of a longer file.
const logDownloadCut = "[ESM: the file is longer than 16 MiB; this download holds its newest 16 MiB]"

// LogDownload returns the server log (source "server") or the console
// output (source "console") of an instance as text, every line prepared
// like a log tail (see showOutput): masked and cut. It holds at most the
// newest MaxLogDownloadBytes of the file; when the file is longer, a first
// line says so. full is as for Logs: without it an instance that passes
// its passwords on the command line answers ErrOutputHidden.
func (m *Manager) LogDownload(name, source string, full bool) ([]byte, error) {
	in, err := m.get(name)
	if err != nil {
		return nil, err
	}
	in.mu.Lock()
	hidden, gen := in.cfg.OutputHidden(), in.hiddenGen
	in.mu.Unlock()
	if hidden && !full {
		return nil, ErrOutputHidden
	}
	var rel string
	switch source {
	case "", SourceServer:
		rel = serverLog(name)
	case SourceConsole:
		rel = consoleFile(name)
	default:
		return nil, fault.Invalidf("source must be server or console")
	}
	raw, cut, err := m.readEnd(rel, MaxLogDownloadBytes)
	if err != nil {
		return nil, err
	}
	if !full {
		// As in Logs: the instance may have changed while the file was read.
		in.mu.Lock()
		gone, changed := in.deleted, in.cfg.OutputHidden() || in.hiddenGen != gen
		in.mu.Unlock()
		if gone {
			return nil, fault.NotFoundf("instance %q not found", name)
		}
		if changed {
			return nil, ErrOutputHidden
		}
	}
	filter := in.outputFilter()
	var out bytes.Buffer
	if cut {
		out.WriteString(logDownloadCut + "\n")
	}
	for len(raw) > 0 {
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, raw = raw[:i], raw[i+1:]
		} else {
			raw = nil
		}
		out.WriteString(showOutput(filter, string(line)))
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// readEnd reads at most limit bytes from the end of the root-relative file
// rel, starting at a line start. cut reports that the start of the file
// was left out. A file that does not exist reads as empty.
func (m *Manager) readEnd(rel string, limit int64) (b []byte, cut bool, err error) {
	f, err := m.st.Open(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	off := max(fi.Size()-limit, 0)
	b = make([]byte, fi.Size()-off)
	n, err := f.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	b = b[:n]
	if off > 0 {
		// The first piece may be the end of a longer line.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			b = nil
		}
	}
	return b, off > 0, nil
}
