package instances

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// readArchive unpacks a tar.gz into name -> content.
func readArchive(t *testing.T, b []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg || h.Mode != 0o600 {
			t.Fatalf("%s: type %c mode %o", h.Name, h.Typeflag, h.Mode)
		}
		c, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = string(c)
	}
}

// A backup downloads as a tar.gz of its meta.json and save files, and a
// damaged backup is refused before anything is written.
func TestBackupArchive(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	ctx := context.Background()
	if _, err := r.m.Start(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	b, err := r.m.Backup(ctx, "alpha", ReasonManual)
	if err != nil || len(b.Files) != 1 {
		t.Fatalf("backup %+v %v", b, err)
	}
	got, write, err := r.m.BackupArchive("alpha", b.ID)
	if err != nil || got.ID != b.ID || got.SizeBytes != b.SizeBytes {
		t.Fatalf("archive of %s: %+v %v", b.ID, got, err)
	}
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		t.Fatal(err)
	}
	files := readArchive(t, buf.Bytes())
	top := "alpha-" + b.ID + "/"
	save, err := r.st.ReadFile(backupDir("alpha", b.ID) + "/" + b.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[top+b.Files[0]] != string(save) || !strings.Contains(files[top+"meta.json"], b.ID) {
		names := []string{}
		for n := range files {
			names = append(names, n)
		}
		slices.Sort(names)
		t.Fatalf("archive holds %v", names)
	}
	for _, id := range []string{"20260101T000000Z-manual", "../../x", "", b.ID + "x"} {
		if _, _, err := r.m.BackupArchive("alpha", id); fault.KindOf(err) != fault.NotFound {
			t.Errorf("backup %q: %v, want not found", id, err)
		}
	}
	if _, _, err := r.m.BackupArchive("Bad", b.ID); fault.KindOf(err) != fault.NotFound {
		t.Errorf("bad instance name: %v", err)
	}
	meta := b
	meta.Files = []string{"../../../esm.json"}
	if err := r.st.WriteJSON(backupDir("alpha", b.ID)+"/meta.json", meta, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.m.BackupArchive("alpha", b.ID); fault.CodeOf(err) != "backup_damaged" {
		t.Fatalf("damaged backup: %v", err)
	}
}

// A log download is the newest part of the file, every line prepared as in
// a tail, and is refused to the read role like the tail.
func TestLogDownload(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	if err := r.st.MkdirAll(path.Dir(serverLog("alpha")), 0o700); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 5000)
	text := "first\r\n-ServerPassword=hunter2hunter2 -log\n" + long + "\nlast"
	if err := r.st.WriteFile(serverLog("alpha"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := r.m.LogDownload("alpha", SourceServer, false)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 4 || lines[0] != "first" || strings.Contains(string(b), "hunter2") || len(lines[2]) != 4096 || lines[3] != "last" {
		t.Fatalf("download: %d lines %.80q", len(lines), b)
	}
	if b, err := r.m.LogDownload("alpha", SourceConsole, false); err != nil || len(b) != 0 {
		t.Fatalf("console without output: %q %v", b, err)
	}
	if _, err := r.m.LogDownload("alpha", "other", true); fault.KindOf(err) != fault.Invalid {
		t.Fatalf("unknown source: %v", err)
	}
	if _, err := r.m.LogDownload("nobody", SourceServer, true); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("unknown instance: %v", err)
	}
	// Longer than the cap: the newest part, from a line start, and a note.
	var big strings.Builder
	for i := 0; big.Len() < MaxLogDownloadBytes+(1<<20); i++ {
		fmt.Fprintf(&big, "line %07d %s\n", i, strings.Repeat("y", 100))
	}
	if err := r.st.WriteFile(serverLog("alpha"), []byte(big.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err = r.m.LogDownload("alpha", SourceServer, true)
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if lines[0] != logDownloadCut || !strings.HasPrefix(lines[1], "line ") || len(lines[1]) != len(lines[2]) ||
		!strings.HasSuffix(big.String(), lines[len(lines)-1]+"\n") || len(b) > MaxLogDownloadBytes+len(logDownloadCut)+1 {
		t.Fatalf("capped download: %d bytes, starts %.60q", len(b), b)
	}
	// Passwords on the command line: nothing for the read role.
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryCommandLine)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.LogDownload("alpha", SourceServer, false); !errors.Is(err, ErrOutputHidden) {
		t.Fatalf("read role: %v", err)
	}
	if _, err := r.m.LogDownload("alpha", SourceConsole, true); err != nil {
		t.Fatalf("operator: %v", err)
	}
}
