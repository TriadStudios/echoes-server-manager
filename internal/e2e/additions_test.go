package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
	"github.com/TriadStudios/echoes-server-manager/internal/web"
)

// get sends a GET with a bearer token and returns the whole answer.
func (h *harness) get(path, token string) (*http.Response, []byte) {
	h.t.Helper()
	req := newRequest(h.t, "GET", h.base+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp, b
}

// GET /box names the host, the time zone and what the box has and uses;
// every value it cannot read is null with a reason.
func TestBoxFactsAndFirewall(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	read := h.token("read")
	var box api.Box
	if st := h.call("GET", "/api/v1/box", read, nil, &box); st != http.StatusOK {
		t.Fatalf("box: %d", st)
	}
	u := box.Usage
	if box.TimeZone == "" || u.CPUCount < 1 || u.Unknown == nil {
		t.Fatalf("box %+v", box)
	}
	for _, x := range u.Unknown {
		if x.Field == "" || x.Reason == "" {
			t.Fatalf("unknown without a reason: %+v", u.Unknown)
		}
	}
	if (u.MemoryTotalBytes == nil) == (u.MemoryAvailableBytes != nil) || (u.DiskTotalBytes == nil) == (u.DiskFreeBytes != nil) {
		t.Fatalf("usage %+v", u)
	}
	var fw api.Firewall
	if st := h.call("GET", "/api/v1/box/firewall", read, nil, &fw); st != http.StatusOK {
		t.Fatalf("firewall: %d", st)
	}
	game := fmt.Sprintf("%d:%d/udp", h.gameLo, h.gameLo+9)
	query := fmt.Sprintf("%d-%d/udp", h.queryLo, h.queryLo+9)
	if len(fw.UFW) != 2 || fw.UFW[0] != "sudo ufw allow "+game ||
		len(fw.Firewalld) != 3 || fw.Firewalld[1] != "sudo firewall-cmd --permanent --add-port="+query ||
		fw.Firewalld[2] != "sudo firewall-cmd --reload" || !strings.Contains(fw.Note, "never runs") {
		t.Fatalf("firewall %+v", fw)
	}
}

// An instance view carries what ESM reads about its server right now, and
// the next scheduled restart and backup.
func TestInstanceLiveFacts(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	h.must(http.StatusOK, "PATCH", "/api/v1/instances/alpha", map[string]any{"schedule": map[string]any{"restart_at": []string{"04:00"}}}, nil)
	var v instances.View
	h.must(http.StatusOK, "GET", "/api/v1/instances/alpha", nil, &v)
	if v.Live.Players != nil || v.Live.MemoryBytes != nil || len(v.Live.Unknown) < 2 || v.Scheduled.Restart.IsZero() {
		t.Fatalf("stopped: live %+v scheduled %+v", v.Live, v.Scheduled)
	}
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	testkit.Eventually(t, 10*time.Second, func() bool {
		h.must(http.StatusOK, "GET", "/api/v1/instances/alpha", nil, &v)
		return v.Live.Players != nil
	}, "no player count")
	var list api.InstanceList
	h.must(http.StatusOK, "GET", "/api/v1/instances", nil, &list)
	if len(list.Instances) != 1 || list.Instances[0].Live.Players == nil {
		t.Fatalf("list: %+v", list)
	}
}

// A log downloads as text, with the same rules as the log tail: the read
// role gets nothing of an instance that passes its passwords on the
// command line.
func TestLogDownload(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	h.must(http.StatusCreated, "POST", "/api/v1/instances", map[string]any{"name": "beta", "bind_ip": "127.0.0.1",
		"rules": fastRules(nil), "secret_delivery": "command_line"}, nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	read := h.token("read")
	var body []byte
	testkit.Eventually(t, 10*time.Second, func() bool {
		resp, b := h.get("/api/v1/instances/alpha/logs/download", read)
		body = b
		return resp.StatusCode == http.StatusOK && bytes.Count(b, []byte("\n")) > 1
	}, "no log download")
	resp, _ := h.get("/api/v1/instances/alpha/logs/download?source=console", h.admin)
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if resp.StatusCode != http.StatusOK || err != nil || params["filename"] != "alpha-console.log" ||
		resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || resp.Header.Get("Content-Security-Policy") != web.CSP {
		t.Fatalf("console download: %d %v", resp.StatusCode, resp.Header)
	}
	if !bytes.Contains(body, []byte("ready")) {
		t.Fatalf("server log download: %.200q", body)
	}
	for _, source := range []string{"server", "console"} {
		var eb api.ErrorBody
		if st := h.call("GET", "/api/v1/instances/beta/logs/download?source="+source, read, nil, &eb); st != http.StatusForbidden ||
			eb.Error.Code != "hidden_unsafe_secrets" {
			t.Fatalf("read role, command-line instance, %s: %d %+v", source, st, eb)
		}
		if st := h.call("GET", "/api/v1/instances/beta/logs/download?source="+source, h.admin, nil, nil); st != http.StatusOK {
			t.Fatalf("admin, command-line instance, %s: %d", source, st)
		}
	}
	var eb api.ErrorBody
	if st := h.call("GET", "/api/v1/instances/alpha/logs/download?source=other", h.admin, nil, &eb); st != http.StatusUnprocessableEntity {
		t.Fatalf("unknown source: %d %+v", st, eb)
	}
}

// A backup downloads as a tar.gz for operators; the read role may not.
func TestBackupDownload(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	var b instances.Backup
	h.must(http.StatusCreated, "POST", "/api/v1/instances/alpha/backup", struct{}{}, &b)
	oper, read := h.token("operator"), h.token("read")
	path := "/api/v1/instances/alpha/backups/" + b.ID + "/download"
	resp, body := h.get(path, oper)
	_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/gzip" || params["filename"] != "alpha-"+b.ID+".tar.gz" {
		t.Fatalf("download: %d %v", resp.StatusCode, resp.Header)
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	names := []string{}
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hd.Name)
	}
	if len(names) != 1+len(b.Files) || names[0] != "alpha-"+b.ID+"/meta.json" || names[1] != "alpha-"+b.ID+"/"+b.Files[0] {
		t.Fatalf("archive holds %v", names)
	}
	var eb api.ErrorBody
	if st := h.call("GET", path, read, nil, &eb); st != http.StatusForbidden || eb.Error.Code != "forbidden" {
		t.Fatalf("read role: %d %+v", st, eb)
	}
	for _, p := range []string{
		"/api/v1/instances/alpha/backups/20200101T000000Z-manual/download",
		"/api/v1/instances/ghost/backups/" + b.ID + "/download",
		"/api/v1/instances/alpha/backups/..%2f..%2fesm.json/download",
	} {
		if st := h.call("GET", p, oper, nil, &eb); st != http.StatusNotFound {
			t.Errorf("%s: %d %+v", p, st, eb)
		}
	}
}

// Under systemd a server's memory is its unit's: the server and everything
// it started.
func TestInstanceMemorySystemd(t *testing.T) {
	h := newSystemdESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	name := unitSafeName("sdm")
	h.create(name, "", nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/"+name+"/start?wait=true", struct{}{}, nil)
	var v instances.View
	testkit.Eventually(t, 10*time.Second, func() bool {
		h.must(http.StatusOK, "GET", "/api/v1/instances/"+name, nil, &v)
		return v.Live.MemoryBytes != nil
	}, "no memory for a server under systemd: %+v", &v.Live)
	if *v.Live.MemoryBytes <= 0 || v.Live.MemorySource != "cgroup" {
		t.Fatalf("memory under systemd: %+v", v.Live)
	}
	h.must(http.StatusOK, "POST", "/api/v1/instances/"+name+"/stop", struct{}{}, nil)
}
