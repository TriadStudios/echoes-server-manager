package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
)

// TokenEnv names the environment variable that holds the API token for
// --url.
const TokenEnv = "ESM_TOKEN"

// client is a thin HTTP client for the ESM API: over the unix socket of
// the local root by default, or to --url with a bearer token.
type client struct {
	base  string
	http  *http.Client
	token string
	where string // for error messages
}

// remoteError is an error answer from the service.
type remoteError struct {
	Status  int
	Code    string
	Message string
}

func (e *remoteError) Error() string { return e.Message }

func newClient(g *globals) (*client, error) {
	if g.url != "" {
		u, err := url.Parse(g.url)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, usageErr("--url must be an http or https URL")
		}
		// The token goes with every request: over plain http it would cross
		// the network in clear, so http is only for this machine.
		if u.Scheme == "http" && !config.IsLoopback(u.Hostname()) {
			return nil, usageErr("--url must be https for another machine (plain http would send the token in clear); http is accepted only for localhost")
		}
		tok := g.token
		if tok == "" {
			tok = os.Getenv(TokenEnv)
		}
		if tok == "" {
			return nil, usageErr("--url needs --token or " + TokenEnv)
		}
		return &client{base: strings.TrimRight(g.url, "/"), http: &http.Client{}, token: tok, where: g.url}, nil
	}
	root, err := config.ResolveRoot(g.root)
	if err != nil {
		return nil, err
	}
	sock := (&config.Config{Root: root}).SocketPath()
	if cfg, _, err := config.Peek(root); err == nil {
		sock = cfg.SocketPath()
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &client{base: "http://esm", http: &http.Client{Transport: tr}, where: sock}, nil
}

func (c *client) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

func (c *client) send(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("cannot reach the ESM service at %s (is `esm serve` running?): %w", c.where, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var eb api.ErrorBody
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(b, &eb) == nil && eb.Error.Code != "" {
			return nil, &remoteError{Status: resp.StatusCode, Code: eb.Error.Code, Message: eb.Error.Message}
		}
		return nil, &remoteError{Status: resp.StatusCode, Code: "http_error", Message: fmt.Sprintf("HTTP %s", resp.Status)}
	}
	return resp, nil
}

// do sends a JSON request and decodes the JSON answer into out (when not
// nil).
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// doStatus is do that also returns the answer's status code.
func (c *client) doStatus(ctx context.Context, method, path string, body, out any) (int, error) {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return 0, err
	}
	resp, err := c.send(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}
