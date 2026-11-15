package config

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The API listener is off unless esm.json has an api_listener object, and
// ESM never writes one: neither the defaults of a first start nor a file
// without it get one, so an older ESM still reads the file.
func TestAPIListenerOffByDefault(t *testing.T) {
	st := openStore(t)
	cfg, _, err := Load(st)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIListener != nil {
		t.Fatalf("defaults have an API listener: %+v", cfg.APIListener)
	}
	if b, _ := st.ReadFile(FileName); strings.Contains(string(b), "api_listener") {
		t.Fatalf("the default file names api_listener:\n%s", b)
	}
	// A file without the key is left as it is, and saved again without it.
	st = openStore(t)
	const body = `{"listen":"127.0.0.1:9000","public_hosts":["esm.example.com"]}`
	if err := st.WriteFile(FileName, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, created, err := Load(st)
	if err != nil || created || cfg.APIListener != nil {
		t.Fatalf("load: %+v created=%v err=%v", cfg.APIListener, created, err)
	}
	if b, _ := st.ReadFile(FileName); string(b) != body {
		t.Fatalf("the file was changed:\n%s", b)
	}
	if err := Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.ReadFile(FileName); strings.Contains(string(b), "api_listener") {
		t.Fatalf("saving wrote api_listener:\n%s", b)
	}
	// null is off as well.
	st = openStore(t)
	st.WriteFile(FileName, []byte(`{"api_listener":null}`), 0o600)
	if cfg, _, err := Load(st); err != nil || cfg.APIListener != nil {
		t.Fatalf("null: %+v %v", cfg, err)
	}
}

// An api_listener is read with every field, and the command line's Peek
// reads it the same way.
func TestAPIListenerLoads(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	os.MkdirAll(root, 0o700)
	body := `{"api_listener":{"listen":"192.0.2.10:8743","tls_cert":"certs/esm.crt","tls_key":"/etc/esm/esm.key",
		"host_names":["esm.example.com","panel.example.org:8443","192.0.2.11","[2001:db8::5]"],"page":true}}`
	os.WriteFile(filepath.Join(root, FileName), []byte(body), 0o600)
	cfg, found, err := Peek(root)
	if err != nil || !found {
		t.Fatalf("peek: %v %v", found, err)
	}
	l := cfg.APIListener
	if l == nil || l.Listen != "192.0.2.10:8743" || !l.Page || l.AllAddresses || len(l.HostNames) != 4 {
		t.Fatalf("api_listener %+v", l)
	}
	// "/etc/esm/esm.key" is absolute on Linux only.
	wantKey := "/etc/esm/esm.key"
	if !filepath.IsAbs(wantKey) {
		wantKey = filepath.Join(root, wantKey)
	}
	if cert, key := l.CertFiles(root); cert != filepath.Join(root, "certs", "esm.crt") || key != wantKey {
		t.Fatalf("cert files %q %q", cert, key)
	}
	dns, ips := l.CertNames()
	if !slices.Equal(dns, []string{"esm.example.com", "panel.example.org"}) {
		t.Errorf("dns names %q", dns)
	}
	var got []string
	for _, ip := range ips {
		got = append(got, ip.String())
	}
	if !slices.Equal(got, []string{"192.0.2.10", "192.0.2.11", "2001:db8::5"}) {
		t.Errorf("ip addresses %q", got)
	}
	if c, k := (&APIListener{}).CertFiles(root); c != "" || k != "" {
		t.Errorf("unset cert files %q %q", c, k)
	}
}

// listen has no default and must name one address of this box, unless
// all_addresses says that every address is meant; it may not be the main
// listener's address; tls_cert and tls_key go together; host_names are host
// names or addresses with an optional port.
func TestAPIListenerRules(t *testing.T) {
	cases := []struct {
		name string
		l    APIListener
		main string
		want string // "" for accepted, else a part of the error
	}{
		{"one address", APIListener{Listen: "192.0.2.10:8743"}, "", ""},
		{"loopback", APIListener{Listen: "127.0.0.1:0"}, "", ""},
		{"ipv6", APIListener{Listen: "[2001:db8::5]:8743"}, "", ""},
		{"no listen", APIListener{}, "", "listen is required"},
		{"no listen but names", APIListener{HostNames: []string{"esm.example.com"}}, "", "listen is required"},
		{"bad listen", APIListener{Listen: "192.0.2.10"}, "", "listen"},
		{"bad port", APIListener{Listen: "192.0.2.10:99999"}, "", "port"},
		{"empty host", APIListener{Listen: ":8743"}, "", "every address"},
		{"0.0.0.0", APIListener{Listen: "0.0.0.0:8743"}, "", "every address"},
		{"::", APIListener{Listen: "[::]:8743"}, "", "every address"},
		{"::0", APIListener{Listen: "[::0]:8743"}, "", "every address"},
		{"all addresses without names", APIListener{Listen: "0.0.0.0:8743", AllAddresses: true}, "", "host_names must name"},
		{"all addresses", APIListener{Listen: "0.0.0.0:8743", AllAddresses: true, HostNames: []string{"esm.example.com"}}, "", ""},
		{"all addresses, ipv6", APIListener{Listen: "[::]:8743", AllAddresses: true, HostNames: []string{"192.0.2.10"}}, "", ""},
		{"all addresses, empty host", APIListener{Listen: ":8743", AllAddresses: true, HostNames: []string{"192.0.2.10"}}, "", ""},
		{"all addresses with one address", APIListener{Listen: "192.0.2.10:8743", AllAddresses: true}, "", "all_addresses is set"},
		{"host name", APIListener{Listen: "esm.example.com:8743"}, "", "IP address"},
		{"localhost", APIListener{Listen: "localhost:8743"}, "", "IP address"},
		{"main address", APIListener{Listen: "127.0.0.1:8740"}, "127.0.0.1:8740", "main listener"},
		{"main address as localhost", APIListener{Listen: "127.0.0.1:8740"}, "localhost:8740", "main listener"},
		{"main on every address", APIListener{Listen: "192.0.2.10:8740"}, "0.0.0.0:8740", "main listener"},
		{"every address over the main port", APIListener{Listen: "0.0.0.0:8740", AllAddresses: true, HostNames: []string{"a.example"}}, "127.0.0.1:8740", "main listener"},
		{"main address, other port", APIListener{Listen: "127.0.0.1:8743"}, "127.0.0.1:8740", ""},
		{"other address, main port", APIListener{Listen: "192.0.2.10:8740"}, "127.0.0.1:8740", ""},
		{"port 0 twice", APIListener{Listen: "127.0.0.1:0"}, "127.0.0.1:0", ""},
		{"cert alone", APIListener{Listen: "192.0.2.10:8743", TLSCert: "/c.pem"}, "", "together"},
		{"key alone", APIListener{Listen: "192.0.2.10:8743", TLSKey: "/k.pem"}, "", "together"},
		{"cert and key", APIListener{Listen: "192.0.2.10:8743", TLSCert: "/c.pem", TLSKey: "/k.pem"}, "", ""},
		{"control character", APIListener{Listen: "192.0.2.10:8743", TLSCert: "/c\n.pem", TLSKey: "/k.pem"}, "", "control"},
		{"names", APIListener{Listen: "192.0.2.10:8743", HostNames: []string{"esm.example.com", "ESM.Example.org.", "esm.example.com:8443",
			"192.0.2.11", "192.0.2.11:443", "2001:db8::1", "[2001:db8::1]", "[2001:db8::1]:8443", "localhost", "box"}}, "", ""},
		{"name with scheme", APIListener{Listen: "192.0.2.10:8743", HostNames: []string{"https://esm.example.com"}}, "", "host_names"},
		{"name with path", APIListener{Listen: "192.0.2.10:8743", HostNames: []string{"esm.example.com/api"}}, "", "host_names"},
		{"wildcard", APIListener{Listen: "192.0.2.10:8743", HostNames: []string{"*.example.com"}}, "", "host_names"},
		{"empty name", APIListener{Listen: "192.0.2.10:8743", HostNames: []string{""}}, "", "host_names"},
		{"name with space", APIListener{Listen: "192.0.2.10:8743", HostNames: []string{"esm example"}}, "", "host_names"},
		{"name with bad port", APIListener{Listen: "192.0.2.10:8743", HostNames: []string{"esm.example.com:0"}}, "", "host_names"},
		{"name with a dash first", APIListener{Listen: "192.0.2.10:8743", HostNames: []string{"-esm.example.com"}}, "", "host_names"},
		{"every address as a name", APIListener{Listen: "192.0.2.10:8743", HostNames: []string{"0.0.0.0"}}, "", "every address"},
		{"too many names", APIListener{Listen: "192.0.2.10:8743", HostNames: slices.Repeat([]string{"a.example"}, MaxHostNames+1)}, "", "at most"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Defaults(t.TempDir())
			if c.main != "" {
				cfg.Listen = c.main
			}
			l := c.l
			cfg.APIListener = &l
			err := cfg.Validate()
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && err == nil:
				t.Fatalf("accepted %+v", c.l)
			case c.want != "" && (!strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "config: api_listener: ")):
				t.Fatalf("error %q, want one with %q", err, c.want)
			}
		})
	}
	// Through the file as well: an object without listen, and an unknown
	// field, are refused.
	for _, body := range []string{`{"api_listener":{}}`, `{"api_listener":{"listen":"192.0.2.10:8743","pages":true}}`,
		`{"api_listener":{"listen":"0.0.0.0:8743"}}`} {
		st := openStore(t)
		st.WriteFile(FileName, []byte(body), 0o600)
		if _, _, err := Load(st); err == nil {
			t.Errorf("%s accepted", body)
		}
	}
}

// The names of a certificate: every host name once and in lower case,
// without ports or a final dot, and every address once, the listen address
// first unless it is every address.
func TestAPIListenerCertNames(t *testing.T) {
	l := APIListener{Listen: "0.0.0.0:8743", AllAddresses: true,
		HostNames: []string{"ESM.example.com.", "esm.example.com:8443", "192.0.2.4", "192.0.2.4:443", "[2001:db8::1]:443"}}
	dns, ips := l.CertNames()
	if !slices.Equal(dns, []string{"esm.example.com"}) || len(ips) != 2 || !ips[0].Equal(net.ParseIP("192.0.2.4")) ||
		!ips[1].Equal(net.ParseIP("2001:db8::1")) {
		t.Fatalf("names %q %v", dns, ips)
	}
	l = APIListener{Listen: "127.0.0.1:0"}
	if dns, ips := l.CertNames(); len(dns) != 0 || len(ips) != 1 || !ips[0].Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("loopback names %q %v", dns, ips)
	}
}
