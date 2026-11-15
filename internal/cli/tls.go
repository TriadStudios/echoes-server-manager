package cli

import (
	"fmt"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/tlscert"
)

// TLSResult is what tls prints with --json: the API listener as GET
// /api/v1/box shows it, null when there is none.
type TLSResult struct {
	APIListener *api.APIListenerInfo `json:"api_listener"`
}

// tlsCmd prints the API listener and the fingerprints of its certificate,
// for a client that pins it. It only reads.
func (e *env) tlsCmd(args []string) error {
	fs := e.flags("tls")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("tls takes no arguments")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var b api.Box
	if err := c.do(e.ctx, "GET", "/api/v1/box", nil, &b); err != nil {
		return err
	}
	l := b.APIListener
	if l == nil {
		return e.result(TLSResult{}, "no API listener: api_listener is not set in esm.json")
	}
	ct := l.Certificate
	from := "made by ESM, self-signed"
	if ct.Source == tlscert.SourceOperator {
		from = "from tls_cert"
	}
	takes := "API tokens only"
	if l.Page {
		takes = "API tokens and the page"
	}
	line := fmt.Sprintf("API listener on %s with TLS (%s): certificate %s, valid until %s, file %s; SHA-256 %s; public key pin %s",
		l.Listen, takes, from, ct.NotAfter.Format("2006-01-02"), ct.File, ct.SHA256, ct.SPKIPin)
	return e.result(TLSResult{APIListener: l}, line)
}
