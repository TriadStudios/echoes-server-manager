package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/doctor"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
)

func (e *env) serve(args []string) error {
	fs := e.flags("serve")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("serve takes no arguments")
	}
	root, err := config.ResolveRoot(e.g.root)
	if err != nil {
		return err
	}
	ctx, stop := interruptible()
	defer stop()
	svc, err := service.Start(service.Options{Root: root, Version: e.version, Stderr: e.errOut})
	if err != nil {
		return err
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-svc.Err():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return errors.Join(runErr, svc.Shutdown(sctx))
}

// DoctorResult is what doctor prints with --json.
type DoctorResult struct {
	Local   doctor.Report  `json:"local"`
	Service *doctor.Report `json:"service"` // nil when no service answered
}

func (e *env) doctor(args []string) int {
	fs := e.flags("doctor")
	if _, err := parse(fs, args); err != nil {
		return e.fail(err)
	}
	root, err := config.ResolveRoot(e.g.root)
	if err != nil {
		return e.fail(err)
	}
	res := DoctorResult{Local: doctor.Run(doctor.Options{Root: root})}
	if c, err := e.client(); err == nil {
		var rep doctor.Report
		ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
		if err := c.do(ctx, "GET", "/api/v1/box/doctor", nil, &rep); err == nil {
			res.Service = &rep
		}
		cancel()
	}
	problems, warnings, ok := 0, 0, 0
	var notes []string
	add := func(r doctor.Report, prefix string) {
		for _, c := range r.Checks {
			switch c.Status {
			case doctor.Fail:
				problems++
				notes = append(notes, prefix+c.Name+": "+c.Message)
			case doctor.Warn:
				warnings++
				notes = append(notes, prefix+c.Name+": "+c.Message)
			default:
				ok++
			}
		}
	}
	add(res.Local, "")
	view := "no service answered"
	if res.Service != nil {
		add(*res.Service, "service ")
		view = "service checked too"
	}
	line := fmt.Sprintf("doctor: %d ok, %s, %s (%s)", ok, plural(warnings, "warning", "warnings"), plural(problems, "problem", "problems"), view)
	if len(notes) > 0 {
		line += "; " + strings.Join(notes, "; ")
	}
	if err := e.result(res, line); err != nil {
		return e.fail(err)
	}
	if problems > 0 {
		return 1
	}
	return 0
}

func describeStatus(st instances.Status) string {
	s := st.Name + " " + string(st.State)
	extra := joinNonEmpty(
		cond(st.Build != "", "build "+st.Build),
		cond(st.PID != 0, "pid "+strconv.Itoa(st.PID)),
		cond(st.ReadySource != "" && st.State == instances.Running, "ready via "+st.ReadySource),
		cond(st.Crashes > 0, plural(st.Crashes, "crash", "crashes")),
		cond(st.GaveUp, "gave up"),
		cond(st.RestartRequired, "restart required"),
	)
	if extra != "" {
		s += " (" + extra + ")"
	}
	if st.Reason != "" && st.Reason != string(st.State) && st.State != instances.Running {
		s += ": " + st.Reason
	}
	return s
}

func cond(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}

// warn appends the warnings of an instance to a result line, in words.
func warn(line string, warnings []string) string {
	for _, w := range warnings {
		line += "; warning: " + instances.WarningText(w)
	}
	return line
}

// unsafeNote is the short mark, in lists, of an instance that passes its
// passwords on the server command line.
const unsafeNote = "UNSAFE: passwords on the command line"

func (e *env) list(args []string) error {
	fs := e.flags("list")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("list takes no arguments")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var out api.InstanceList
	if err := c.do(e.ctx, "GET", "/api/v1/instances", nil, &out); err != nil {
		return err
	}
	if len(out.Instances) == 0 {
		return e.result(out, "no instances")
	}
	parts := make([]string, len(out.Instances))
	for i, v := range out.Instances {
		note := cond(v.OutputHidden(), ", "+unsafeNote)
		parts[i] = fmt.Sprintf("%s %s (%d/%d, build %s%s)", v.Name, v.Status.State, v.Ports.Game, v.Ports.Query, v.Build, note)
	}
	return e.result(out, plural(len(out.Instances), "instance", "instances")+": "+strings.Join(parts, ", "))
}

func (e *env) status(args []string) error {
	fs := e.flags("status")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	switch len(pos) {
	case 0:
		var b api.Box
		if err := c.do(e.ctx, "GET", "/api/v1/box", nil, &b); err != nil {
			return err
		}
		limits := "limits enforced"
		if !b.LimitsEnforced {
			limits = "limits not enforced"
		}
		line := fmt.Sprintf("esm %s at %s: %s supervisor (%s), %s (%d running), %s, listening on %s",
			b.Version, b.Root, b.Supervisor, limits, plural(b.Instances, "instance", "instances"), b.Running,
			plural(b.Builds, "build", "builds"), b.Listen)
		if n := len(b.SecretsOnCommandLine); n > 0 {
			line += fmt.Sprintf("; %s (%s)", unsafeNote, strings.Join(b.SecretsOnCommandLine, ", "))
		}
		return e.result(b, line)
	case 1:
		// The instance, not only its status: the warnings belong to it.
		var v instances.View
		if err := c.do(e.ctx, "GET", "/api/v1/instances/"+url.PathEscape(pos[0]), nil, &v); err != nil {
			return err
		}
		return e.result(v, warn(describeStatus(v.Status), v.Warnings))
	}
	return usageErr("status takes at most one instance name")
}

// multi collects a repeatable flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, " ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func (e *env) create(args []string) error {
	fs := e.flags("create")
	var (
		req                         instances.CreateRequest
		channel, mapName, world     string
		display, cluster, bind      string
		delivery                    string
		gamePort, queryPort, maxPly int
		sets, extra                 multi
	)
	fs.StringVar(&req.Build, "build", "", "build version (default: newest of the channel)")
	fs.StringVar(&channel, "channel", "", "live or test")
	fs.StringVar(&mapName, "map", "", "map (default: the server's default)")
	fs.StringVar(&world, "world", "", "world name (default: the instance name)")
	fs.StringVar(&display, "display-name", "", "name shown to players")
	fs.StringVar(&cluster, "cluster", "", "cluster name")
	fs.IntVar(&gamePort, "game-port", 0, "game port (default: first free)")
	fs.IntVar(&queryPort, "query-port", 0, "query port (default: first free)")
	fs.StringVar(&bind, "bind", "", "IP address to bind (default: all)")
	fs.IntVar(&maxPly, "max-players", 0, "player limit")
	fs.Var(&sets, "set", "rule Key=Value (repeatable)")
	fs.Var(&extra, "arg", "extra server argument like -X=Y (repeatable)")
	fs.StringVar(&delivery, "secret-delivery", "", "how the server gets its passwords: file (default) or command_line (unsafe on a shared box)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := want("create", pos, 1, "one instance name"); err != nil {
		return err
	}
	req.Name = pos[0]
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	str := func(flag, v string) *string {
		if given[flag] {
			return &v
		}
		return nil
	}
	req.Channel = str("channel", channel)
	req.Map = str("map", mapName)
	req.World = str("world", world)
	req.DisplayName = str("display-name", display)
	req.Cluster = str("cluster", cluster)
	req.BindIP = str("bind", bind)
	req.SecretDelivery = str("secret-delivery", delivery)
	if given["max-players"] {
		req.MaxPlayers = &maxPly
	}
	if given["game-port"] || given["query-port"] {
		req.Ports = &instances.Ports{Game: gamePort, Query: queryPort}
	}
	if len(sets) > 0 {
		rules := map[string]string{}
		for _, kv := range sets {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return usageErr("--set needs Key=Value, got %q", kv)
			}
			rules[k] = v
		}
		req.Rules = &rules
	}
	if len(extra) > 0 {
		args := []string(extra)
		req.ExtraArgs = &args
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var out api.InstanceChange
	if err := c.do(e.ctx, "POST", "/api/v1/instances", req, &out); err != nil {
		return err
	}
	line := fmt.Sprintf("created %s: build %s (%s), game port %d, query port %d", out.Name, out.Build, out.Channel, out.Ports.Game, out.Ports.Query)
	return e.result(out, warn(line, out.Warnings))
}

func (e *env) lifecycle(verb string, args []string) error {
	fs := e.flags(verb)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := want(verb, pos, 1, "one instance name"); err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	path := "/api/v1/instances/" + url.PathEscape(pos[0]) + "/" + verb
	if verb != "stop" {
		path += "?wait=true"
	}
	var st instances.Status
	if err := c.do(e.ctx, "POST", path, struct{}{}, &st); err != nil {
		return err
	}
	return e.result(st, describeStatus(st))
}

func (e *env) logs(args []string) error {
	fs := e.flags("logs")
	n := fs.Int("n", 50, "number of lines")
	follow := fs.Bool("f", false, "keep printing new lines until interrupted")
	console := fs.Bool("console", false, "show the console output instead of the server log")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := want("logs", pos, 1, "one instance name"); err != nil {
		return err
	}
	name := pos[0]
	source := instances.SourceServer
	if *console {
		source = instances.SourceConsole
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var stream *bufio.Scanner
	if *follow {
		ctx, stop := interruptible()
		defer stop()
		e.ctx = ctx
		q := url.Values{"instance": {name}, "types": {events.TypeLog}}
		req, err := c.request(ctx, "GET", "/api/v1/events?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := c.send(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		stream = bufio.NewScanner(resp.Body)
		stream.Buffer(make([]byte, 64<<10), 1<<20)
	}
	var out api.Logs
	q := url.Values{"lines": {strconv.Itoa(*n)}, "source": {source}}
	if err := c.do(e.ctx, "GET", "/api/v1/instances/"+url.PathEscape(name)+"/logs?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	if e.g.json && !*follow {
		return e.result(out, "")
	}
	for _, l := range out.Lines {
		e.printLog(l, source)
	}
	if stream == nil {
		return nil
	}
	for stream.Scan() {
		data, ok := strings.CutPrefix(stream.Text(), "data: ")
		if !ok {
			continue
		}
		var ev events.Event
		var line instances.LogLine
		if json.Unmarshal([]byte(data), &ev) != nil || json.Unmarshal(ev.Data, &line) != nil || line.Source != source {
			continue
		}
		e.printLog(line.Line, source)
	}
	if e.ctx.Err() != nil {
		return nil // interrupted
	}
	return stream.Err()
}

func (e *env) printLog(line, source string) {
	if e.g.json {
		b, _ := json.Marshal(instances.LogLine{Source: source, Line: line})
		fmt.Fprintf(e.out, "%s\n", b)
		return
	}
	fmt.Fprintln(e.out, line)
}

func (e *env) update(args []string) error {
	fs := e.flags("update")
	to := fs.String("to", "", "build version to update to")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := want("update", pos, 1, "one instance name"); err != nil {
		return err
	}
	if *to == "" {
		return usageErr("update needs --to <version>")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var st instances.Status
	if err := c.do(e.ctx, "POST", "/api/v1/instances/"+url.PathEscape(pos[0])+"/update", api.UpdateRequest{To: *to}, &st); err != nil {
		return err
	}
	return e.result(st, fmt.Sprintf("%s updated to %s (%s)", st.Name, st.Build, st.State))
}

func (e *env) backup(args []string) error {
	fs := e.flags("backup")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := want("backup", pos, 1, "one instance name"); err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var b instances.Backup
	if err := c.do(e.ctx, "POST", "/api/v1/instances/"+url.PathEscape(pos[0])+"/backup", struct{}{}, &b); err != nil {
		return err
	}
	note := ""
	if !b.SaveConfirmed {
		note = "; no world save was seen first"
	}
	return e.result(b, fmt.Sprintf("backup %s of %s: %s, %s%s", b.ID, b.Instance, plural(len(b.Files), "file", "files"), byteSize(b.SizeBytes), note))
}

func (e *env) restore(args []string) error {
	fs := e.flags("restore")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := want("restore", pos, 2, "an instance name and a backup id"); err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var st instances.Status
	if err := c.do(e.ctx, "POST", "/api/v1/instances/"+url.PathEscape(pos[0])+"/restore", api.RestoreRequest{Backup: pos[1]}, &st); err != nil {
		return err
	}
	return e.result(st, fmt.Sprintf("%s restored from %s (%s)", st.Name, pos[1], st.State))
}

func (e *env) build(args []string) error {
	fs := e.flags("build")
	sum := fs.String("sha256", "", "expected sha256 of an archive or download")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr("build needs install, list or prune")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	switch pos[0] {
	case "install":
		if err := want("build install", pos[1:], 1, "a folder, archive or https URL"); err != nil {
			return err
		}
		src := pos[1]
		if lower := strings.ToLower(src); !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
			// The service resolves paths on its own; send an absolute one.
			if src, err = filepath.Abs(src); err != nil {
				return err
			}
		}
		var b api.BuildView
		if err := c.do(e.ctx, "POST", "/api/v1/builds", api.InstallRequest{Source: src, SHA256: *sum}, &b); err != nil {
			return err
		}
		return e.result(b, fmt.Sprintf("installed build %s (%s, %s, %s)", b.Version, b.Channel, plural(b.Files, "file", "files"), byteSize(b.SizeBytes)))
	case "list":
		if len(pos) != 1 {
			return usageErr("build list takes no arguments")
		}
		var out api.BuildList
		if err := c.do(e.ctx, "GET", "/api/v1/builds", nil, &out); err != nil {
			return err
		}
		if len(out.Builds) == 0 {
			return e.result(out, "no builds")
		}
		parts := make([]string, len(out.Builds))
		for i, b := range out.Builds {
			use := "unused"
			if len(b.UsedBy) > 0 {
				use = "used by " + strings.Join(b.UsedBy, " ")
			}
			parts[i] = fmt.Sprintf("%s (%s, %s)", b.Version, b.Channel, use)
		}
		return e.result(out, plural(len(out.Builds), "build", "builds")+": "+strings.Join(parts, ", "))
	case "prune":
		if len(pos) != 1 {
			return usageErr("build prune takes no arguments")
		}
		var out api.Removed
		if err := c.do(e.ctx, "POST", "/api/v1/builds/prune", struct{}{}, &out); err != nil {
			return err
		}
		if len(out.Removed) == 0 {
			return e.result(out, "nothing to prune")
		}
		return e.result(out, "removed "+plural(len(out.Removed), "build", "builds")+": "+strings.Join(out.Removed, ", "))
	}
	return usageErr("unknown build command %q (install, list or prune)", pos[0])
}

func (e *env) token(args []string) error {
	fs := e.flags("token")
	role := fs.String("role", "", "read, operator or admin")
	name := fs.String("name", "", "a name to recognise the token by")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] != "create" {
		return usageErr("token needs create --role read|operator|admin [--name NAME]")
	}
	if *role == "" {
		return usageErr("token create needs --role read, operator or admin")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var out api.NewToken
	if err := c.do(e.ctx, "POST", "/api/v1/tokens", api.TokenRequest{Name: *name, Role: *role}, &out); err != nil {
		return err
	}
	return e.result(out, fmt.Sprintf("%s (id %s, role %s; shown only this once)", out.Token, out.ID, out.Role))
}
