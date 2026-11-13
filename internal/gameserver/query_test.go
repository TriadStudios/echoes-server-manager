package gameserver

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleInfo() Info {
	return Info{Protocol: 17, Name: "Example Server", Map: "TestMap", Folder: "example", Game: "Example", AppID: 0,
		Players: 3, MaxPlayers: 10, Bots: 0, ServerType: 'd', OS: 'l', Password: true, VAC: false, Version: "1.0.0.0"}
}

func TestInfoRoundTrip(t *testing.T) {
	want := sampleInfo()
	ch, got, err := ParseQueryReply(InfoReply(want))
	if err != nil || ch != nil || got != want {
		t.Fatalf("got %+v %v %v, want %+v", got, ch, err, want)
	}
	ch, _, err = ParseQueryReply(ChallengeReply([4]byte{1, 2, 3, 4}))
	if err != nil || !bytes.Equal(ch, []byte{1, 2, 3, 4}) {
		t.Fatalf("challenge %v %v", ch, err)
	}
	if b := InfoReply(Info{Name: "a\x00b", Players: 999, MaxPlayers: -5}); bytes.Contains(b[6:9], []byte{'a', 0, 'b'}) {
		t.Fatal("a zero byte in a string was written")
	} else if _, info, err := ParseQueryReply(b); err != nil || info.Name != "ab" || info.Players != 255 || info.MaxPlayers != 0 {
		t.Fatalf("clamped reply: %+v %v", info, err)
	}
}

// Every reply that is cut short, too long, split, of another type or with
// an endless string is refused.
func TestParseQueryReplyBounds(t *testing.T) {
	full := InfoReply(sampleInfo())
	// Without the version (the rest is optional) the reply ends after the
	// VAC byte.
	minimal := len(full) - len("1.0.0.0") - 2
	for n := 0; n < minimal; n++ {
		if _, _, err := ParseQueryReply(full[:n]); err == nil {
			t.Errorf("a reply cut to %d of %d bytes was accepted", n, len(full))
		}
	}
	if _, info, err := ParseQueryReply(full[:minimal]); err != nil || info.Version != "" || info.MaxPlayers != 10 {
		t.Errorf("a reply without the version: %+v %v", info, err)
	}
	if _, _, err := ParseQueryReply(full[:len(full)-2]); err == nil {
		t.Error("a version without its terminator was accepted")
	}
	long := strings.Repeat("n", maxQueryString)
	if _, info, err := ParseQueryReply(InfoReply(Info{Name: long})); err != nil || info.Name != long {
		t.Errorf("a name of %d bytes: %v", len(long), err)
	}
	for name, c := range map[string]struct {
		b    []byte
		want error
	}{
		"empty":           {nil, ErrQueryShort},
		"three bytes":     {[]byte{0xff, 0xff, 0xff}, ErrQueryShort},
		"header only":     {[]byte(a2sHeader), ErrQueryShort},
		"wrong header":    {append([]byte("\xff\xff\xff\x00I"), full[5:]...), ErrQueryHeader},
		"split packet":    {append([]byte(a2sSplit), full[4:]...), ErrQuerySplit},
		"oversized":       {append(append([]byte(nil), full...), make([]byte, MaxQueryPacket)...), ErrQueryLong},
		"other type":      {[]byte(a2sHeader + "m"), ErrQueryType},
		"short challenge": {[]byte(a2sHeader + "A\x01\x02\x03"), ErrQueryShort},
		"long challenge":  {[]byte(a2sHeader + "A\x01\x02\x03\x04\x05"), ErrQueryShort},
		"endless name":    {[]byte(a2sHeader + "I\x11" + strings.Repeat("n", maxQueryString+1) + "\x00"), ErrQueryString},
		"unterminated":    {[]byte(a2sHeader + "I\x11" + strings.Repeat("n", 40)), ErrQueryShort},
	} {
		if _, _, err := ParseQueryReply(c.b); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

func FuzzParseQueryReply(f *testing.F) {
	f.Add(InfoReply(sampleInfo()))
	f.Add(ChallengeReply([4]byte{9, 8, 7, 6}))
	f.Add([]byte(a2sHeader + "I"))
	f.Add([]byte(a2sSplit + "I"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		ch, info, err := ParseQueryReply(b)
		if err != nil {
			if ch != nil || info != (Info{}) {
				t.Fatalf("an error with a result: %v %+v", ch, info)
			}
			return
		}
		if len(b) > MaxQueryPacket || ch != nil && len(ch) != 4 {
			t.Fatalf("accepted %d bytes, challenge %v", len(b), ch)
		}
		for _, s := range []string{info.Name, info.Map, info.Folder, info.Game, info.Version} {
			if len(s) > 3*maxQueryString {
				t.Fatalf("a string of %d bytes", len(s))
			}
		}
	})
}

func TestParseInfoRequest(t *testing.T) {
	if ch, ok := ParseInfoRequest(InfoRequest(nil)); !ok || ch != nil {
		t.Fatalf("plain request: %v %v", ch, ok)
	}
	if ch, ok := ParseInfoRequest(InfoRequest([]byte{1, 2, 3, 4})); !ok || !bytes.Equal(ch, []byte{1, 2, 3, 4}) {
		t.Fatalf("request with a challenge: %v %v", ch, ok)
	}
	req := InfoRequest(nil)
	for _, b := range [][]byte{nil, []byte("PING"), req[:len(req)-1], append(append([]byte(nil), req...), 1, 2), append(append([]byte(nil), req...), 1, 2, 3, 4, 5)} {
		if _, ok := ParseInfoRequest(b); ok {
			t.Errorf("%q taken for a request", b)
		}
	}
	if string(req) != "\xff\xff\xff\xffTSource Engine Query\x00" {
		t.Fatalf("request %q", req)
	}
}

// queryServer answers on a loopback UDP port with answer(request, n), n
// counting requests from 1. It returns the port and the requests seen.
func queryServer(t *testing.T, answer func(req []byte, n int) []byte) (int, func() [][]byte) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var mu sync.Mutex
	var seen [][]byte
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			mu.Lock()
			seen = append(seen, append([]byte(nil), buf[:n]...))
			k := len(seen)
			mu.Unlock()
			if a := answer(buf[:n], k); a != nil {
				pc.WriteTo(a, addr)
			}
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func queryCtx(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// The probe answers the server's challenge by sending the request again
// with the challenge appended.
func TestQueryInfoChallenge(t *testing.T) {
	want := sampleInfo()
	challenge := [4]byte{0xde, 0xad, 0xbe, 0xef}
	port, seen := queryServer(t, func(req []byte, n int) []byte {
		if ch, ok := ParseInfoRequest(req); ok && bytes.Equal(ch, challenge[:]) {
			return InfoReply(want)
		}
		return ChallengeReply(challenge)
	})
	got, err := QueryInfo(queryCtx(t, 2*time.Second), "", port)
	if err != nil || got != want {
		t.Fatalf("info %+v %v", got, err)
	}
	reqs := seen()
	if len(reqs) != 2 || !bytes.Equal(reqs[0], InfoRequest(nil)) || !bytes.Equal(reqs[1], InfoRequest(challenge[:])) {
		t.Fatalf("requests %q", reqs)
	}
	if err := Probe(queryCtx(t, 2*time.Second), "127.0.0.1", port); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestQueryInfoWithoutChallenge(t *testing.T) {
	port, seen := queryServer(t, func([]byte, int) []byte { return InfoReply(sampleInfo()) })
	if got, err := QueryInfo(queryCtx(t, 2*time.Second), "127.0.0.1", port); err != nil || got.Map != "TestMap" || got.MaxPlayers != 10 {
		t.Fatalf("info %+v %v", got, err)
	}
	if n := len(seen()); n != 1 {
		t.Fatalf("%d requests", n)
	}
}

func TestQueryInfoFailures(t *testing.T) {
	// A server that only ever asks for a challenge is given up on.
	port, seen := queryServer(t, func(_ []byte, n int) []byte { return ChallengeReply([4]byte{byte(n), 0, 0, 0}) })
	if _, err := QueryInfo(queryCtx(t, 2*time.Second), "127.0.0.1", port); !errors.Is(err, ErrQueryChallenges) {
		t.Fatalf("endless challenges: %v", err)
	}
	if n := len(seen()); n != maxChallenges+1 {
		t.Fatalf("%d requests for endless challenges", n)
	}
	// Garbage, a reply longer than a packet, and silence.
	port, _ = queryServer(t, func([]byte, int) []byte { return []byte("PONG") })
	if _, err := QueryInfo(queryCtx(t, 2*time.Second), "127.0.0.1", port); err == nil {
		t.Fatal("garbage accepted")
	}
	port, _ = queryServer(t, func([]byte, int) []byte {
		return append(InfoReply(sampleInfo()), make([]byte, MaxQueryPacket)...)
	})
	if _, err := QueryInfo(queryCtx(t, 2*time.Second), "127.0.0.1", port); err == nil {
		t.Fatal("an oversized reply was accepted")
	}
	port, _ = queryServer(t, func([]byte, int) []byte { return nil })
	start := time.Now()
	if err := Probe(queryCtx(t, 300*time.Millisecond), "127.0.0.1", port); err == nil {
		t.Fatal("a silent server answered")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the probe took %s", el)
	}
}
