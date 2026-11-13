package steam

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Bounds of the app manifest parser.
const (
	// MaxACFBytes is the largest app manifest read.
	MaxACFBytes = 1 << 20
	maxACFDepth = 16
	maxACFTerms = 100000
	maxACFText  = 4096
)

// AppManifest is what ESM reads from SteamCMD's
// steamapps/appmanifest_<app>.acf after an app_update.
type AppManifest struct {
	AppID   string
	BuildID string
	// StateFlags says how complete the install is; bit 4 is "fully
	// installed".
	StateFlags int64
}

// FullyInstalled reports whether SteamCMD marked the app fully installed.
func (a AppManifest) FullyInstalled() bool { return a.StateFlags&4 != 0 }

// kv is one key of Valve's KeyValues text: a string value or a block.
type kv struct {
	key   string
	value string
	block []kv
	isBlk bool
}

var digitsRe = regexp.MustCompile(`^[0-9]{1,20}$`)

// ParseAppManifest reads an app manifest strictly: Valve's KeyValues text
// with quoted strings only, one top-level "AppState" block, and an appid,
// a buildid and StateFlags of digits, each named once. Anything else, and
// anything beyond the size, depth and length bounds, is refused.
func ParseAppManifest(b []byte) (AppManifest, error) {
	if len(b) > MaxACFBytes {
		return AppManifest{}, fmt.Errorf("app manifest larger than %d bytes", MaxACFBytes)
	}
	p := &kvParser{s: b}
	top, err := p.pairs(0)
	if err != nil {
		return AppManifest{}, err
	}
	if p.pos < len(p.s) {
		return AppManifest{}, p.errf("text after the last block")
	}
	if len(top) != 1 || !top[0].isBlk || !strings.EqualFold(top[0].key, "AppState") {
		return AppManifest{}, errors.New("app manifest: expected one \"AppState\" block")
	}
	var am AppManifest
	found := map[string]bool{}
	for _, e := range top[0].block {
		k := strings.ToLower(e.key)
		switch k {
		case "appid", "buildid", "stateflags":
		default:
			continue
		}
		if e.isBlk {
			return AppManifest{}, fmt.Errorf("app manifest: %s is a block, not a value", e.key)
		}
		if found[k] {
			return AppManifest{}, fmt.Errorf("app manifest: %s is named twice", e.key)
		}
		found[k] = true
		if !digitsRe.MatchString(e.value) {
			return AppManifest{}, fmt.Errorf("app manifest: %s is not a number", e.key)
		}
		switch k {
		case "appid":
			am.AppID = strings.TrimLeft(e.value, "0")
		case "buildid":
			am.BuildID = e.value
		case "stateflags":
			n, err := strconv.ParseInt(e.value, 10, 64)
			if err != nil {
				return AppManifest{}, fmt.Errorf("app manifest: StateFlags is out of range")
			}
			am.StateFlags = n
		}
	}
	for _, k := range []string{"appid", "buildid", "stateflags"} {
		if !found[k] {
			return AppManifest{}, fmt.Errorf("app manifest: no %s", k)
		}
	}
	if am.AppID == "" || strings.Trim(am.BuildID, "0") == "" {
		return AppManifest{}, errors.New("app manifest: appid and buildid must not be 0")
	}
	return am, nil
}

type kvParser struct {
	s     []byte
	pos   int
	terms int
}

func (p *kvParser) errf(format string, args ...any) error {
	line := 1 + strings.Count(string(p.s[:min(p.pos, len(p.s))]), "\n")
	return fmt.Errorf("app manifest line %d: %s", line, fmt.Sprintf(format, args...))
}

// skip passes white space and // comments.
func (p *kvParser) skip() {
	for p.pos < len(p.s) {
		switch c := p.s[p.pos]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			p.pos++
		case c == '/' && p.pos+1 < len(p.s) && p.s[p.pos+1] == '/':
			for p.pos < len(p.s) && p.s[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

// pairs reads key/value pairs until the end of a block ("}") or, at depth 0,
// the end of the text.
func (p *kvParser) pairs(depth int) ([]kv, error) {
	if depth > maxACFDepth {
		return nil, p.errf("blocks nested deeper than %d", maxACFDepth)
	}
	var out []kv
	for {
		p.skip()
		if p.pos >= len(p.s) {
			if depth > 0 {
				return nil, p.errf("a block is not closed")
			}
			return out, nil
		}
		if p.s[p.pos] == '}' {
			if depth == 0 {
				return nil, p.errf("\"}\" without a block")
			}
			p.pos++
			return out, nil
		}
		if p.terms++; p.terms > maxACFTerms {
			return nil, p.errf("more than %d entries", maxACFTerms)
		}
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		p.skip()
		if p.pos >= len(p.s) {
			return nil, p.errf("%q has no value", key)
		}
		if p.s[p.pos] == '{' {
			p.pos++
			blk, err := p.pairs(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, kv{key: key, block: blk, isBlk: true})
			continue
		}
		val, err := p.str()
		if err != nil {
			return nil, err
		}
		out = append(out, kv{key: key, value: val})
	}
}

// str reads one quoted string with the escapes \\, \", \n and \t.
func (p *kvParser) str() (string, error) {
	if p.s[p.pos] != '"' {
		return "", p.errf("expected a quoted string")
	}
	p.pos++
	var b strings.Builder
	for {
		if p.pos >= len(p.s) {
			return "", p.errf("a string is not closed")
		}
		c := p.s[p.pos]
		p.pos++
		switch {
		case c == '"':
			return b.String(), nil
		case c == '\\':
			if p.pos >= len(p.s) {
				return "", p.errf("a string is not closed")
			}
			e := p.s[p.pos]
			p.pos++
			switch e {
			case '\\', '"':
				b.WriteByte(e)
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				return "", p.errf("unknown escape \\%c", e)
			}
		case c == '\n':
			return "", p.errf("a string runs over a line end")
		default:
			b.WriteByte(c)
		}
		if b.Len() > maxACFText {
			return "", p.errf("a string longer than %d bytes", maxACFText)
		}
	}
}
