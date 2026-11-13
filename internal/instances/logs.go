package instances

import (
	"bytes"
	"context"
	"encoding/json"
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
	maxLineBytes   = 4096      // longer lines are cut when they are shown
	maxFollowChunk = 256 << 10 // read at most this much per check
	maxTailBytes   = 8 << 20   // a tail reads at most this much from the end
	maxScrubBytes  = 64 << 10  // longer lines are cut to this before they are masked
	// scrubGuard is more than any token, or piece of one, at the end of a
	// cut piece (see showOutput).
	scrubGuard = 1 << 10
	// MaxTailLines is the most lines one log request returns.
	MaxTailLines = 5000
	// MaxLogReplyBytes bounds what one log request returns: its lines,
	// each counted as the JSON string it is sent as, take at most this
	// many bytes together. The oldest lines are left out first.
	MaxLogReplyBytes = 4 << 20
)

// tail follows a growing file through the store, one whole line at a time.
type tail struct {
	rel     string
	off     int64
	partial []byte
	// skip is set while the tail is inside a line whose start it did not
	// see or has already handed out: everything up to the next line end is
	// dropped, so the rest of a line is never taken for a line of its own.
	skip bool
}

// tailAt returns a tail that follows rel from its current end. If the file
// does not end with a line end, what follows first is the rest of an
// unfinished line and is dropped.
func (m *Manager) tailAt(rel string) tail {
	t := tail{rel: rel}
	f, err := m.st.Open(rel)
	if err != nil {
		return t
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return t
	}
	t.off = fi.Size()
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, t.off-1); err != nil || last[0] != '\n' {
		t.skip = true
	}
	return t
}

// follow returns the whole lines added to the file since the last call, as
// the file has them: not cut, not masked. A line that grows beyond
// maxLineBytes without ending is handed out once, as far as it got, and
// its rest is dropped. A file that shrank (was replaced or truncated) is
// read from the start.
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
		t.off, t.partial, t.skip = 0, nil, false
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
		if t.skip {
			t.skip = false
		} else {
			lines = append(lines, string(data[:i]))
		}
		data = data[i+1:]
	}
	switch {
	case t.skip:
		data = nil
	case len(data) > maxLineBytes:
		lines = append(lines, string(data))
		t.skip = true
		data = nil
	}
	t.partial = append([]byte(nil), data...)
	return lines
}

// showLine prepares one line for showing: without a trailing carriage
// return, cut to maxLineBytes and valid UTF-8. Mask a line before it is cut,
// so a value is never cut in two.
func showLine(line string) string {
	line = strings.TrimRight(line, "\r")
	if len(line) > maxLineBytes {
		line = line[:maxLineBytes]
	}
	return strings.ToValidUTF8(line, "�")
}

// tailLines returns the last n lines of a file as the file has them (an
// empty list when the file does not exist).
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
		out[i] = string(p)
	}
	return out, nil
}

// Logs returns the last lines of the server log (source "server") or the
// console output (source "console"), prepared for showing (see showOutput).
//
// full says that the caller may see the output of an instance that passes
// its passwords on the command line (operators and admins). Without it such
// an instance answers ErrOutputHidden: the read role gets none of that
// output, masked or not.
func (m *Manager) Logs(name, source string, lines int, full bool) ([]string, error) {
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
	if !full {
		// While the file was read the instance may have been set to
		// command-line delivery and started that way, or deleted and made
		// anew under the same name.
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
	for i := range out {
		out[i] = showOutput(filter, out[i])
	}
	return capReply(out), nil
}

// capReply keeps the newest lines that fit into MaxLogReplyBytes, each
// line counted as the JSON string it is sent as (plus a separator).
func capReply(lines []string) []string {
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		b, _ := json.Marshal(lines[i])
		total += len(b) + 1
		if total > MaxLogReplyBytes {
			return lines[i+1:]
		}
	}
	return lines
}

// waitWorldSaved waits until the server log gets a new world-saved line,
// up to d. It reports whether one was seen.
func (m *Manager) waitWorldSaved(ctx context.Context, name string, d time.Duration) bool {
	t := m.tailAt(serverLog(name))
	deadline := time.Now().Add(d)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, line := range m.follow(&t) {
			if gameserver.IsWorldSavedLine(line) {
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
