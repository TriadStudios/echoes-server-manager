package cli

import (
	"strings"
	"testing"
)

// --url sends the token with every request: plain http is accepted only to
// this machine, where nothing crosses a network.
func TestURLMustBeHTTPSUnlessLoopback(t *testing.T) {
	cases := map[string]bool{
		"https://esm.example.com":     true,
		"https://192.0.2.1:8740/":     true,
		"http://127.0.0.1:8740":       true,
		"http://localhost:8740":       true,
		"http://[::1]:8740":           true,
		"http://192.0.2.1:8740":       false,
		"http://esm.example.com":      false,
		"HTTP://esm.example.com:8740": false,
		"ftp://esm.example.com":       false,
	}
	for url, ok := range cases {
		_, err := newClient(&globals{url: url, token: "esm_x"})
		if (err == nil) != ok {
			t.Errorf("--url %s: err %v, want accepted %v", url, err, ok)
		}
		if !ok && err != nil && strings.HasPrefix(strings.ToLower(url), "http://") && !strings.Contains(err.Error(), "https") {
			t.Errorf("--url %s: the error does not point to https: %v", url, err)
		}
	}
}
