package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
)

var eventTypes = []string{events.TypeState, events.TypeLog, events.TypeBuild, events.TypeAudit, events.TypeUpdate}

// events streams events as Server-Sent Events. Each event has an id, a
// type (state, log, build or audit) and the event as JSON data. A client
// that reconnects with Last-Event-ID gets the events it missed while they
// are still in memory. Audit events are only sent to admins. Of an
// instance that passes its passwords on the server command line the read
// role gets no log events, and state events with a notice in place of the
// reason; that was decided when each event was published.
//
// The stream ends with its login: before every event and keep-alive it
// checks that the token or session it was opened with still holds (see
// loginHolds), so a deleted token or an ended session gets nothing more.
//
// A client that stops reading never holds anything up: the bus drops its
// subscription once the subscription's buffer is full (publishing never
// waits for a subscriber), and every write to the client has its own
// deadline (WriteTimeout), after which the stream ends. The client then
// reconnects with Last-Event-ID.
func (s *Server) events(w http.ResponseWriter, r *http.Request, c *call) error {
	q := r.URL.Query()
	f := events.Filter{Instance: q.Get("instance"), Public: !c.full()}
	if t := q.Get("types"); t != "" {
		for _, typ := range strings.Split(t, ",") {
			typ = strings.TrimSpace(typ)
			if !slices.Contains(eventTypes, typ) {
				return fault.Invalidf("types: %q is not one of %s", typ, strings.Join(eventTypes, ", "))
			}
			f.Types = append(f.Types, typ)
		}
	}
	if !c.role.Allows(auth.RoleAdmin) {
		if len(f.Types) == 0 {
			f.Types = []string{events.TypeState, events.TypeLog, events.TypeBuild, events.TypeUpdate}
		} else if f.Types = slices.DeleteFunc(f.Types, func(t string) bool { return t == events.TypeAudit }); len(f.Types) == 0 {
			return errStatus(http.StatusForbidden, "forbidden", "audit events need the admin role")
		}
	}
	after := s.o.Bus.LastID()
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		n, err := strconv.ParseUint(strings.TrimSpace(h), 10, 64)
		if err != nil {
			return fault.BadRequestf("Last-Event-ID must be an event id")
		}
		after = n
	}
	backlog, sub, complete := s.o.Bus.SubscribeFrom(after, f, sseBuffer)
	defer sub.Close()
	rc := http.NewResponseController(w)
	// deadline gives the next write WriteTimeout to get through.
	deadline := func() { _ = rc.SetWriteDeadline(time.Now().Add(s.o.WriteTimeout)) }
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	deadline()
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return nil
	}
	if !complete {
		fmt.Fprint(w, ": some events were missed\n\n")
	}
	for _, e := range backlog {
		if writeEvent(w, e) != nil {
			return nil
		}
	}
	if rc.Flush() != nil {
		return nil
	}
	tick := time.NewTicker(s.o.KeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case e, ok := <-sub.C:
			if !ok {
				return nil // shutting down, or too far behind: the client reconnects
			}
			if !s.loginHolds(c) {
				return nil
			}
			deadline()
			if writeEvent(w, e) != nil || rc.Flush() != nil {
				return nil
			}
		case <-tick.C:
			if !s.loginHolds(c) {
				return nil
			}
			deadline()
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil || rc.Flush() != nil {
				return nil
			}
		}
	}
}

// loginHolds reports whether the login a stream was opened with still
// holds, with at least the role it had: the token still exists, the
// session is still open (not logged out, not expired). A stream checks it
// before every event and every keep-alive, so it ends with its login.
func (s *Server) loginHolds(c *call) bool {
	var role auth.Role
	switch {
	case c.actor.Kind == audit.ActorSocket:
		return true
	case c.actor.Kind == audit.ActorToken:
		r, err := s.o.Auth.TokenRole(c.actor.ID)
		if err != nil {
			return false
		}
		role = r
	case c.session != nil:
		sess, err := s.o.Auth.Session(c.session.ID)
		if err != nil {
			return false
		}
		role = sess.Role
	default:
		return false
	}
	return role.Allows(c.role)
}

// sseBuffer is how many events a stream's subscription holds for a client
// that is slow to read; with one more the bus drops the subscription.
const sseBuffer = 1024

func writeEvent(w http.ResponseWriter, e events.Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, b)
	return err
}
