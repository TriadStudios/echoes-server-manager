package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
)

// ESM's HTTP servers cap request headers and time out slow headers and idle
// connections. They have no server-wide read or write timeout: the API sets
// those per request (see api.Server.guard), so calls that rightly take long
// and event streams keep working.
func TestHTTPServerLimits(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler(), nil)
	if srv.MaxHeaderBytes != api.MaxHeaderBytes || api.MaxHeaderBytes != 64<<10 {
		t.Errorf("MaxHeaderBytes %d", srv.MaxHeaderBytes)
	}
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 2*time.Minute {
		t.Errorf("ReadHeaderTimeout %s, IdleTimeout %s", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("ReadTimeout %s, WriteTimeout %s: these would end long calls and event streams", srv.ReadTimeout, srv.WriteTimeout)
	}
}
