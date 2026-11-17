package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
)

// The site role is for the token of a website's admin panel (see
// auth.RoleSite). Its calls are the operator's, plus the routes opened to it
// with route.site, plus what the handlers named below let it ask for.

// Acting user of a site token.
const (
	// ActingUserHeader names the person a site token acts for, such as the
	// employee who clicked in the panel. ESM writes it in the audit entry
	// of the call (audit.Actor.ActingUser). It is only a note: nothing is
	// decided by it, and it is read from tokens of the site role only (any
	// other caller's is not looked at).
	ActingUserHeader = "X-ESM-Acting-User"
	// MaxActingUser is the most characters (not bytes) it may hold.
	MaxActingUser = 64
)

// actingUser reads ActingUserHeader of a request that a valid site token
// made. A missing or empty header is "" and no error. A value longer than
// MaxActingUser characters, one that is not valid UTF-8 or has a character
// that does not print (a control character, a line break, a tab, a mark
// that changes the direction of text), and a header sent more than once are
// refused with 400.
func actingUser(r *http.Request) (string, error) {
	vals := r.Header.Values(ActingUserHeader)
	switch len(vals) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", fault.New(fault.BadRequest, "bad_acting_user", "%s is sent once", ActingUserHeader)
	}
	v := vals[0]
	if v == "" {
		return "", nil
	}
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > MaxActingUser || !allPrintable(v) {
		return "", fault.New(fault.BadRequest, "bad_acting_user", "%s must be at most %d printable characters", ActingUserHeader, MaxActingUser)
	}
	return v, nil
}

// allPrintable reports whether every character of s is a letter, mark,
// number, punctuation, symbol or a plain space (unicode.IsPrint).
func allPrintable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// adminOnlyIf is for a call that is open to the site role in part: when what
// the caller asked for (asked) is something only an admin may ask, and the
// caller is not one, it is refused with 403 and msg. It looks at the role
// the call has, not at the token's name or at ActingUserHeader.
func (c *call) adminOnlyIf(asked bool, msg string) error {
	if asked && !c.role.Allows(auth.RoleAdmin) {
		return errStatus(http.StatusForbidden, "forbidden", "%s", msg)
	}
	return nil
}

// sitePatchFields are the fields of a change call (PATCH
// /instances/{name}) that the site role may send: what a panel needs to
// look after a server. It is an allow list, so a field added to the change
// request later is refused to the site role until it is put here (a test
// fails until every field of the request is classified). The fields that
// reach the server's command line or its host (extra_args, limits,
// bind_ip, ports, secret_delivery) and the build (changed by an update)
// are not on it. Names are the JSON keys exactly as written; admins are
// not limited by it.
var sitePatchFields = map[string]bool{
	"display_name":     true,
	"max_players":      true,
	"listed":           true,
	"rules":            true,
	"auto_update":      true,
	"update_max_wait":  true,
	"autostart":        true,
	"restart_on_crash": true,
	"schedule":         true,
	"backup":           true,
	"map":              true,
	"world":            true,
	"cluster":          true,
	"channel":          true,
}

// limitPatch refuses with 403 a change call that names a field the site
// role may not change, whatever its value (null too), before anything is
// decoded or changed. It looks at the keys the caller wrote, exactly: a
// key that is not on sitePatchFields is refused, also when it would only
// differ in case, or is not a field at all. The body is left to be read
// again. A caller with the admin role is not limited; a body that is not a
// JSON object is left to decode, which answers 400.
func (c *call) limitPatch(r *http.Request) error {
	if c.role.Allows(auth.RoleAdmin) {
		return nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	var keys map[string]json.RawMessage
	if json.Unmarshal(b, &keys) != nil {
		return nil
	}
	var no []string
	for k := range keys {
		if !sitePatchFields[k] {
			no = append(no, k)
		}
	}
	if len(no) == 0 {
		return nil
	}
	slices.Sort(no)
	const most = 5
	if len(no) > most {
		no = append(no[:most], "...")
	}
	for i, k := range no {
		no[i] = shorten(k, 40)
	}
	may := make([]string, 0, len(sitePatchFields))
	for k := range sitePatchFields {
		may = append(may, k)
	}
	slices.Sort(may)
	return errStatus(http.StatusForbidden, "forbidden",
		"the site role may not change %s; it may change %s", strings.Join(no, ", "), strings.Join(may, ", "))
}

// shorten cuts s to at most n characters, and marks the cut.
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
