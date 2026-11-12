package instances

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
)

// Log sources.
const (
	SourceServer  = "server"
	SourceConsole = "console"
)

const (
	maxLineBytes   = 4096      // longer lines are cut
	maxFollowChunk = 256 << 10 // read at most this much per check
	maxTailBytes   = 8 << 20   // a tail reads at most this much from the end
	// MaxTailLines is the most lines one log request returns.
	MaxTailLines = 5000
)

// tail follows a growing file through the store.
type tail struct {
	rel     string
	off     int64
	partial []byte
}

func (m *Manager) fileSize(rel string) int64 {
	fi, err := m.st.Stat(rel)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// follow returns the complete lines added to the file since the last call.
// A file that shrank (was replaced or truncated) is read from the start.
func (m *Manager) follow(t *tail) []string {
	f, err := m.st.Open(t.rel)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	size := fi.Size()
	if size < t.off {
		t.off, t.partial = 0, nil
	}
	if size == t.off {
		return nil
	}
	n := min(size-t.off, maxFollowChunk)
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, t.off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	t.off += int64(got)
	data := append(t.partial, buf[:got]...)
	var lines []string
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, cleanLine(data[:i]))
		data = data[i+1:]
	}
	if len(data) > maxLineBytes {
		lines = append(lines, cleanLine(data))
		data = nil
	}
	t.partial = append([]byte(nil), data...)
	return lines
}

func cleanLine(b []byte) string {
	b = bytes.TrimRight(b, "\r")
	if len(b) > maxLineBytes {
		b = b[:maxLineBytes]
	}
	return strings.ToValidUTF8(string(b), "�")
}

// tailLines returns the last n lines of a file (an empty list when the file
// does not exist).
func (m *Manager) tailLines(rel string, n int) ([]string, error) {
	f, err := m.st.Open(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	off := size
	var buf []byte
	for off > 0 && bytes.Count(buf, []byte{'\n'}) <= n && size-off < maxTailBytes {
		step := min(int64(64<<10), off)
		off -= step
		chunk := make([]byte, step)
		if _, err := f.ReadAt(chunk, off); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		buf = append(chunk, buf...)
	}
	buf = bytes.TrimSuffix(buf, []byte{'\n'})
	if len(buf) == 0 {
		return []string{}, nil
	}
	parts := bytes.Split(buf, []byte{'\n'})
	if off > 0 && len(parts) > 0 {
		parts = parts[1:] // the first piece may be the end of a longer line
	}
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = cleanLine(p)
	}
	return out, nil
}

// Logs returns the last lines of the server log (source "server") or the
// console output (source "console").
// Secret values of the instance, and anything that looks like a password
// switch, are masked.
func (m *Manager) Logs(name, source string, lines int) ([]string, error) {
	in, err := m.get(name)
	if err != nil {
		return nil, err
	}
	if lines <= 0 {
		lines = 200
	}
	lines = min(lines, MaxTailLines)
	var rel string
	switch source {
	case "", SourceServer:
		rel = serverLog(name)
	case SourceConsole:
		rel = consoleFile(name)
	default:
		return nil, fault.Invalidf("source must be server or console")
	}
	out, err := m.tailLines(rel, lines)
	if err != nil {
		return nil, err
	}
	in.mu.Lock()
	hidden := in.hidden
	in.mu.Unlock()
	for i := range out {
		out[i] = scrub(out[i], hidden)
	}
	return out, nil
}

// waitWorldSaved waits until the server log gets a new world-saved line,
// up to d. It reports whether one was seen.
func (m *Manager) waitWorldSaved(ctx context.Context, name string, d time.Duration) bool {
	t := tail{rel: serverLog(name), off: m.fileSize(serverLog(name))}
	deadline := time.Now().Add(d)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, line := range m.follow(&t) {
			if gameserver.WorldSavedLine.MatchString(line) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-m.quit:
			return false
		case <-tick.C:
		}
	}
}
