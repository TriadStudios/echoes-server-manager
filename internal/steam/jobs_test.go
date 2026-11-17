package steam

import (
	"bufio"
	"strings"
	"testing"
)

// SteamCMD's progress lines give the bytes of a download; the lines of other
// states count other things and are not read.
func TestDownloadProgress(t *testing.T) {
	for _, c := range []struct {
		line        string
		done, total int64
		ok          bool
	}{
		{" Update state (0x61) downloading, progress: 25.69 (123456 / 480123)", 123456, 480123, true},
		{"Update state (0x61) downloading, progress: 0.00 (0 / 1048576)", 0, 1048576, true},
		{" Update state (0x61) downloading, progress: 100.00 (2 / 2)", 2, 2, true},
		// More than the whole is cut to it.
		{" Update state (0x61) downloading, progress: 100.00 (9 / 8)", 8, 8, true},
		{" Update state (0x5) verifying install, progress: 50.00 (10 / 20)", 0, 0, false},
		{" Update state (0x101) committing, progress: 1.00 (1 / 100)", 0, 0, false},
		{" Update state (0x61) downloading, progress: 50.00 (1 / 0)", 0, 0, false},
		{"Success! App '1000001' fully installed.", 0, 0, false},
		{"", 0, 0, false},
		{" Update state (0x61) downloading, progress: 99999999999999999999.00 (99999999999999999999 / 5)", 0, 0, false},
	} {
		done, total, ok := downloadProgress(c.line)
		if ok != c.ok || done != c.done || total != c.total {
			t.Errorf("downloadProgress(%q) = %d, %d, %v; want %d, %d, %v", c.line, done, total, ok, c.done, c.total, c.ok)
		}
	}
}

// Lines end at a carriage return as well as at a line feed, so a progress
// line that SteamCMD writes over the one before is read when it is written.
func TestScanLinesEndsAtCarriageReturns(t *testing.T) {
	in := "first\nprogress 1\rprogress 2\rprogress 3\r\nlast"
	sc := bufio.NewScanner(strings.NewReader(in))
	sc.Split(scanLines)
	var got []string
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			got = append(got, sc.Text())
		}
	}
	if want := "first|progress 1|progress 2|progress 3|last"; strings.Join(got, "|") != want {
		t.Fatalf("lines %q, want %q", got, want)
	}
}
