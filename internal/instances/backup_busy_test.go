package instances

import (
	"context"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// A backup of a running server copies files the server keeps replacing:
// a file listed a moment ago may be gone (a temporary file renamed into
// place, an older copy being rotated), or held open by the server for a
// moment. Backups still succeed.
func TestBackupWhileTheServerSaves(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", map[string]string{"FakeSaveInterval": "3ms"})
	ctx := context.Background()
	if _, err := r.m.Start(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	for i := range 25 {
		b, err := r.m.Backup(ctx, "alpha", ReasonManual)
		if err != nil {
			t.Fatalf("backup %d: %v", i+1, err)
		}
		if len(b.Files) == 0 {
			t.Fatalf("backup %d holds no files", i+1)
		}
	}
}
