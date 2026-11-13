package gameserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// The query port speaks the Source server query protocol, A2S. ESM asks
// only for A2S_INFO. CONFIRMED: a real server answers A2S_INFO on its query
// port (from outside the box). UNCONFIRMED: whether it answers before it
// has registered its session (ESM checks the log line first), and whether
// it answers on the box's loopback address.
//
// Request: 0xFFFFFFFF 'T' "Source Engine Query\0", plus a 4-byte challenge
// when the server asked for one. Challenge reply: 0xFFFFFFFF 'A' and 4
// bytes. Info reply: 0xFFFFFFFF 'I', the protocol version, then name, map,
// folder and game as zero-terminated strings, the app id (2 bytes, little
// endian), players, max players, bots, server type, environment,
// visibility and VAC (one byte each), the version string, and optional
// extra data ESM does not read.
const (
	a2sHeader    = "\xff\xff\xff\xff"
	a2sSplit     = "\xfe\xff\xff\xff"
	a2sInfo      = 'T'
	a2sChallenge = 'A'
	a2sInfoReply = 'I'
	a2sPayload   = "Source Engine Query\x00"
	// MaxQueryPacket is the largest reply ESM accepts: one packet of the
	// protocol. Anything longer, and replies split over packets, are
	// refused.
	MaxQueryPacket = 1400
	// maxQueryString bounds each string of an info reply.
	maxQueryString = 256
	// maxChallenges is how many challenges ESM answers before it gives up.
	maxChallenges = 2
)

// Info is what ESM reads from an A2S_INFO reply.
type Info struct {
	Protocol   byte
	Name       string
	Map        string
	Folder     string
	Game       string
	AppID      uint16
	Players    int
	MaxPlayers int
	Bots       int
	ServerType byte // 'd' dedicated, 'l' listen, 'p' proxy
	OS         byte // 'l' Linux, 'w' Windows, 'm' or 'o' Mac
	Password   bool
	VAC        bool
	Version    string
}

// Errors of a query.
var (
	ErrQueryShort      = errors.New("gameserver: query reply too short")
	ErrQueryLong       = errors.New("gameserver: query reply longer than one packet")
	ErrQuerySplit      = errors.New("gameserver: query reply split over packets")
	ErrQueryHeader     = errors.New("gameserver: query reply without the protocol header")
	ErrQueryType       = errors.New("gameserver: unexpected query reply type")
	ErrQueryString     = errors.New("gameserver: query reply string too long or not terminated")
	ErrQueryChallenges = errors.New("gameserver: the server kept asking for a challenge")
)

// InfoRequest returns an A2S_INFO request, with challenge appended when it
// is not nil.
func InfoRequest(challenge []byte) []byte {
	b := make([]byte, 0, len(a2sHeader)+1+len(a2sPayload)+4)
	b = append(b, a2sHeader...)
	b = append(b, a2sInfo)
	b = append(b, a2sPayload...)
	return append(b, challenge...)
}

// ParseInfoRequest reports whether b is an A2S_INFO request and returns
// the challenge it carries (nil when none). Anything else is not one.
func ParseInfoRequest(b []byte) (challenge []byte, ok bool) {
	head := a2sHeader + string(rune(a2sInfo)) + a2sPayload
	if len(b) < len(head) || string(b[:len(head)]) != head {
		return nil, false
	}
	rest := b[len(head):]
	switch len(rest) {
	case 0:
		return nil, true
	case 4:
		return rest, true
	}
	return nil, false
}

// ChallengeReply returns the reply that asks for challenge.
func ChallengeReply(challenge [4]byte) []byte {
	return append([]byte(a2sHeader+string(rune(a2sChallenge))), challenge[:]...)
}

// InfoReply returns an A2S_INFO reply for info, without extra data.
func InfoReply(info Info) []byte {
	var b bytes.Buffer
	b.WriteString(a2sHeader)
	b.WriteByte(a2sInfoReply)
	b.WriteByte(info.Protocol)
	for _, s := range []string{info.Name, info.Map, info.Folder, info.Game} {
		b.WriteString(strings.ReplaceAll(s, "\x00", ""))
		b.WriteByte(0)
	}
	binary.Write(&b, binary.LittleEndian, info.AppID)
	for _, n := range []int{info.Players, info.MaxPlayers, info.Bots} {
		b.WriteByte(byte(min(max(n, 0), 255)))
	}
	b.WriteByte(info.ServerType)
	b.WriteByte(info.OS)
	b.WriteByte(boolByte(info.Password))
	b.WriteByte(boolByte(info.VAC))
	b.WriteString(strings.ReplaceAll(info.Version, "\x00", ""))
	b.WriteByte(0)
	b.WriteByte(0) // no extra data
	return b.Bytes()
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// ParseQueryReply reads one reply packet. It returns the challenge of a
// challenge reply, or the info of an info reply. Every field is bounds
// checked; a reply that is cut short, longer than one packet, split over
// packets or of another type is an error.
func ParseQueryReply(b []byte) (challenge []byte, info Info, err error) {
	switch {
	case len(b) > MaxQueryPacket:
		return nil, Info{}, ErrQueryLong
	case len(b) >= 4 && string(b[:4]) == a2sSplit:
		return nil, Info{}, ErrQuerySplit
	case len(b) < 5:
		return nil, Info{}, ErrQueryShort
	case string(b[:4]) != a2sHeader:
		return nil, Info{}, ErrQueryHeader
	}
	r := queryReader{b: b[5:]}
	switch b[4] {
	case a2sChallenge:
		if len(r.b) != 4 {
			return nil, Info{}, ErrQueryShort
		}
		return append([]byte(nil), r.b...), Info{}, nil
	case a2sInfoReply:
	default:
		return nil, Info{}, fmt.Errorf("%w 0x%02x", ErrQueryType, b[4])
	}
	info.Protocol = r.byte()
	info.Name = r.string()
	info.Map = r.string()
	info.Folder = r.string()
	info.Game = r.string()
	if lo, hi := r.byte(), r.byte(); r.err == nil {
		info.AppID = uint16(lo) | uint16(hi)<<8
	}
	info.Players = int(r.byte())
	info.MaxPlayers = int(r.byte())
	info.Bots = int(r.byte())
	info.ServerType = r.byte()
	info.OS = r.byte()
	info.Password = r.byte() != 0
	info.VAC = r.byte() != 0
	// The version and the extra data after it are optional for ESM: a
	// server that ends here still answered.
	if len(r.b) > 0 {
		info.Version = r.string()
	}
	if r.err != nil {
		return nil, Info{}, r.err
	}
	return nil, info, nil
}

// queryReader reads fields of a reply; the first problem sticks.
type queryReader struct {
	b   []byte
	err error
}

func (r *queryReader) byte() byte {
	if r.err != nil {
		return 0
	}
	if len(r.b) < 1 {
		r.err = ErrQueryShort
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

func (r *queryReader) string() string {
	if r.err != nil {
		return ""
	}
	i := bytes.IndexByte(r.b[:min(len(r.b), maxQueryString+1)], 0)
	if i < 0 {
		if len(r.b) <= maxQueryString {
			r.err = ErrQueryShort
		} else {
			r.err = ErrQueryString
		}
		return ""
	}
	s := strings.ToValidUTF8(string(r.b[:i]), "�")
	r.b = r.b[i+1:]
	return s
}

// QueryInfo asks the server behind the query port for A2S_INFO, answering
// up to two challenges, and returns what it says. A server on all
// interfaces (host empty, 0.0.0.0 or ::) is asked on 127.0.0.1. The whole
// exchange ends with ctx (one second when ctx has no deadline).
func QueryInfo(ctx context.Context, host string, port int) (Info, error) {
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Second)
	}
	var d net.Dialer
	if la := LoopbackLocalAddr(host); la != nil {
		d.LocalAddr = la
	}
	c, err := d.DialContext(ctx, "udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return Info{}, err
	}
	defer c.Close()
	if err := c.SetDeadline(deadline); err != nil {
		return Info{}, err
	}
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
	defer stop()
	buf := make([]byte, MaxQueryPacket+1)
	var challenge []byte
	for range maxChallenges + 1 {
		if _, err := c.Write(InfoRequest(challenge)); err != nil {
			return Info{}, err
		}
		n, err := c.Read(buf)
		if err != nil {
			return Info{}, err
		}
		ch, info, err := ParseQueryReply(buf[:n])
		if err != nil {
			return Info{}, err
		}
		if ch == nil {
			return info, nil
		}
		challenge = ch
	}
	return Info{}, ErrQueryChallenges
}

// Probe reports whether the server behind the query port answers A2S_INFO
// (see QueryInfo). It is ESM's last way to see that a server is ready, after
// the status file and the ready log line, and how an adopted server is
// checked again.
func Probe(ctx context.Context, host string, port int) error {
	_, err := QueryInfo(ctx, host, port)
	return err
}
