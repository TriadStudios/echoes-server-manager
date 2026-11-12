package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWriteReadJSON(t *testing.T) {
	s := open(t)
	type doc struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	if err := s.WriteJSON("instances/alpha/instance.json", doc{"alpha", 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	var got doc
	if err := s.ReadJSON("instances/alpha/instance.json", &got); err != nil {
		t.Fatal(err)
	}
	if got != (doc{"alpha", 3}) {
		t.Fatalf("got %+v", got)
	}
	// overwrite
	if err := s.WriteJSON("instances/alpha/instance.json", doc{"alpha", 4}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadJSON("instances/alpha/instance.json", &got); err != nil || got.Count != 4 {
		t.Fatalf("after overwrite: %+v %v", got, err)
	}
	// no temp files left behind
	entries, err := s.ReadDir("instances/alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "instance.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("unexpected files: %v", names)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(s.Path(), "instances", "alpha", "instance.json"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestReadJSONMissingAndUnknownFields(t *testing.T) {
	s := open(t)
	var v struct{ A int }
	if err := s.ReadJSON("nope.json", &v); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	if err := s.WriteFile("x.json", []byte(`{"A":1,"B":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadJSON("x.json", &v); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestPathsOutsideRootRefused(t *testing.T) {
	s := open(t)
	bad := []string{"../x", "a/../../x", "/etc/passwd", `C:\x`, `a\..\..\x`, "a/\x00b"}
	for _, p := range bad {
		if err := s.WriteFile(p, []byte("x"), 0o600); err == nil {
			t.Errorf("write %q accepted", p)
		}
		if _, err := s.ReadFile(p); err == nil {
			t.Errorf("read %q accepted", p)
		}
		if _, err := s.Abs(p); err == nil {
			t.Errorf("Abs %q accepted", p)
		}
	}
	if err := s.RemoveAll("."); err == nil {
		t.Error("RemoveAll of root accepted")
	}
	if _, err := s.Rel(filepath.Dir(s.Path())); err == nil {
		t.Error("Rel of a path outside the root accepted")
	}
}

func TestSymlinkEscapeRefused(t *testing.T) {
	s := open(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.Path(), "link")); err != nil {
		t.Skipf("cannot create symlink here: %v", err)
	}
	if err := s.WriteFile("link/evil.txt", []byte("x"), 0o600); err == nil {
		t.Fatal("write through a symlink leaving the root was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("file was written outside the root")
	}
	if _, err := s.OpenAppend("link/evil.log", 0o600); err == nil {
		t.Fatal("append through escaping symlink accepted")
	}
	// RemoveAll removes the link itself, never what it points to.
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAll("link"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatal("RemoveAll followed the symlink")
	}
}

func TestConcurrentWritersNeverTear(t *testing.T) {
	s := open(t)
	big := strings.Repeat("x", 64<<10)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 10 {
				v := map[string]string{"writer": fmt.Sprint(i, "-", j), "pad": big}
				if err := s.WriteJSON("state.json", v, 0o600); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			b, err := s.ReadFile("state.json")
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				if runtime.GOOS == "windows" {
					continue // a reader may race the replace on Windows; content is what matters
				}
				t.Error(err)
				return
			}
			var v map[string]string
			if err := json.Unmarshal(b, &v); err != nil {
				t.Errorf("torn read: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

func TestLockSerialises(t *testing.T) {
	s := open(t)
	if err := s.WriteJSON("n.json", 0, 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := s.Lock("n")
			defer unlock()
			var n int
			if err := s.ReadJSON("n.json", &n); err != nil {
				t.Error(err)
				return
			}
			if err := s.WriteJSON("n.json", n+1, 0o600); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n int
	if err := s.ReadJSON("n.json", &n); err != nil || n != 20 {
		t.Fatalf("n=%d err=%v, want 20", n, err)
	}
}

func TestAppendMkdirRemoveAll(t *testing.T) {
	s := open(t)
	f, err := s.OpenAppend("logs/a.log", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("one\n")
	f.Close()
	f, _ = s.OpenAppend("logs/a.log", 0o600)
	f.WriteString("two\n")
	f.Close()
	b, err := s.ReadFile("logs/a.log")
	if err != nil || string(b) != "one\ntwo\n" {
		t.Fatalf("got %q %v", b, err)
	}
	if err := s.MkdirAll("builds/v1/bin", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFile("builds/v1/bin/x", []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAll("builds/v1"); err != nil {
		t.Fatal(err)
	}
	if s.Exists("builds/v1") {
		t.Fatal("builds/v1 still exists")
	}
	if err := s.RemoveAll("builds/missing"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFile("plain", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.MkdirAll("plain/sub", 0o700); err == nil {
		t.Fatal("MkdirAll through a file accepted")
	}
}
