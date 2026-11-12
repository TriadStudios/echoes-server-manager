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
	"io/fs"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// FileName is the audit log name inside the ESM root.
const FileName = "audit.log"

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

// Append writes e as one line. A zero Time is set to now.
func (l *Log) Append(e Entry) error {
	if e.Time.IsZero() {
		e.Time = l.now().UTC()
	}
	e.Target = Scrub(e.Target)
	e.Error = Scrub(e.Error)
	e.Actor.ID = Scrub(e.Actor.ID)
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

// Recent returns up to limit of the newest entries, oldest first.
// Lines that do not parse are skipped.
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
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if len(ring) < limit {
			ring = append(ring, e)
			continue
		}
		ring[start] = e
		start = (start + 1) % limit
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(ring))
	out = append(out, ring[start:]...)
	out = append(out, ring[:start]...)
	return out, nil
}
