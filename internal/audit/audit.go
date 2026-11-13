// Package audit appends one JSON line per action to <root>/audit.log.
//
// Entries name who did what to which target and whether it worked. They never
// carry secrets: free-text fields are scrubbed of anything that looks like an
// ESM token or a password switch value before they are written.
package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// FileName is the audit log name inside the ESM root.
const FileName = "audit.log"

// MaxField is the most bytes a text field of an entry keeps; a longer
// value is cut (see Append).
const MaxField = 256

// Actor kinds.
const (
	ActorUser   = "user"
	ActorToken  = "token"
	ActorSocket = "socket"
	ActorSystem = "system"
	// ActorAnonymous is a caller that sent no valid credentials.
	ActorAnonymous = "anonymous"
)

// Actor is who performed an action.
type Actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Role string `json:"role,omitempty"`
}

// Entry is one audit record.
type Entry struct {
	Time   time.Time `json:"time"`
	Actor  Actor     `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
}

// Log is the append-only audit log.
type Log struct {
	st     *store.Store
	mu     sync.Mutex
	f      *os.File
	now    func() time.Time
	notify func(Entry)
}

// Open opens (or creates, mode 0600) the audit log in the store root.
func Open(st *store.Store) (*Log, error) {
	f, err := st.OpenAppend(FileName, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open: %w", err)
	}
	return &Log{st: st, f: f, now: time.Now}, nil
}

// SetNotify registers a function called after every successful append (for
// example to publish the entry as an event). It must not block.
func (l *Log) SetNotify(fn func(Entry)) {
	l.mu.Lock()
	l.notify = fn
	l.mu.Unlock()
}

var secretLike = regexp.MustCompile(`esm_[A-Za-z0-9_-]{16,}`)

// Scrub replaces anything that looks like an ESM token with a mask, and
// masks the value of every password switch or key=value pair with the same
// rule the instance scrubbers use (gameserver.ScrubLine): a switch such as
// -ServerPassword=... is masked up to the next switch or the end of the
// line, spaces and punctuation included.
func Scrub(s string) string {
	s = secretLike.ReplaceAllString(s, "esm_********")
	return gameserver.ScrubLine(s, nil)
}

// clipMark ends a field that was cut.
const clipMark = "..."

// partialToken is what may be left of an ESM token at the end of a cut
// field: too short for the token pattern, but still a piece of one.
var partialToken = regexp.MustCompile(`esm_[A-Za-z0-9_-]*$`)

// clip cuts s to at most MaxField bytes, on a character boundary, and
// marks the cut. A token cut in two is masked like a whole one.
func clip(s string) string {
	if len(s) <= MaxField {
		return s
	}
	cut := MaxField - len(clipMark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return partialToken.ReplaceAllString(s[:cut], "esm_********") + clipMark
}

// Append writes e as one line. A zero Time is set to now. Every text field
// is cut to MaxField bytes first, so the caller's input never makes an
// entry large (or the masking slow); then the free-text fields are
// scrubbed (see Scrub).
func (l *Log) Append(e Entry) error {
	if e.Time.IsZero() {
		e.Time = l.now().UTC()
	}
	e.Actor.Kind = clip(e.Actor.Kind)
	e.Actor.Role = clip(e.Actor.Role)
	e.Action = clip(e.Action)
	// A mask can be longer than what it hides: cut again after scrubbing.
	e.Target = clip(Scrub(clip(e.Target)))
	e.Error = clip(Scrub(clip(e.Error)))
	e.Actor.ID = clip(Scrub(clip(e.Actor.ID)))
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errors.New("audit: log is closed")
	}
	if _, err := l.f.Write(b); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	if l.notify != nil {
		l.notify(e)
	}
	return nil
}

// Sync flushes the log to disk.
func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	return l.f.Sync()
}

// Close syncs and closes the log.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Sync()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// maxLine is the longest line Recent reads; a longer one is skipped.
const maxLine = 1 << 20

// Recent returns up to limit of the newest entries, oldest first.
// Lines that do not parse, or are longer than maxLine, are skipped.
func (l *Log) Recent(limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	f, err := l.st.Open(FileName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ring := make([]Entry, 0, limit)
	start := 0
	add := func(line []byte) {
		var e Entry
		if json.Unmarshal(line, &e) != nil {
			return
		}
		if len(ring) < limit {
			ring = append(ring, e)
			return
		}
		ring[start] = e
		start = (start + 1) % limit
	}
	br := bufio.NewReaderSize(f, 64<<10)
	var line []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > maxLine {
				tooLong, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // the same line goes on
		}
		if !tooLong && len(line) > 0 {
			add(line)
		}
		line, tooLong = line[:0], false
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	out := make([]Entry, 0, len(ring))
	out = append(out, ring[start:]...)
	out = append(out, ring[:start]...)
	return out, nil
}
