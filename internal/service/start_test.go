package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// writeConfig writes a test esm.json (loopback, any free port, process
// supervisor) into root, with extra fields.
func writeConfig(t *testing.T, root string, extra map[string]any) {
	t.Helper()
	cfg := map[string]any{"listen": "127.0.0.1:0", "supervisor": "process"}
	for k, v := range extra {
		cfg[k] = v
	}
	b, _ := json.Marshal(cfg)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "esm.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// shortRoot is a fresh root with a short path, so the socket path stays
// under the platform limit.
func shortRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "esms")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}

func stop(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

// The setup code is printed once as a plain line on stderr, for the
// person who starts ESM; the JSON service log, which may be collected and
// kept elsewhere, says that a code was made but not the code.
func TestSetupCodeIsNotInTheServiceLog(t *testing.T) {
	root := shortRoot(t)
	writeConfig(t, root, nil)
	stderr, logOut := &syncBuffer{}, &syncBuffer{}
	s, err := Start(Options{Root: root, Version: "test", Stderr: stderr,
		Logger: slog.New(slog.NewJSONHandler(logOut, nil))})
	if err != nil {
		t.Fatal(err)
	}
	stop(t, s)
	m := regexp.MustCompile(`ESM setup code: ([A-Z2-7]{24})`).FindStringSubmatch(stderr.String())
	if m == nil {
		t.Fatalf("no setup code on stderr: %q", stderr.String())
	}
	if strings.Contains(logOut.String(), m[1]) {
		t.Fatalf("the setup code is in the service log: %s", logOut.String())
	}
	if !strings.Contains(logOut.String(), "setup code created") {
		t.Fatalf("the service log does not say a setup code was made: %s", logOut.String())
	}
}

// A socket path longer than the system allows is named, with its length,
// the limit and the setting to change, before anything listens.
func TestSocketPathTooLong(t *testing.T) {
	root := filepath.Join(shortRoot(t), strings.Repeat("d", 60), strings.Repeat("e", 60))
	writeConfig(t, root, nil)
	_, err := Start(Options{Root: root, Version: "test", Stderr: &syncBuffer{}})
	if err == nil {
		t.Fatal("ESM started with a socket path that is too long")
	}
	n := len(filepath.Join(root, "esm.sock"))
	for _, want := range []string{"esm.sock", "socket", "esm.json", strconv.Itoa(n), strconv.Itoa(maxSocketPath)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}
