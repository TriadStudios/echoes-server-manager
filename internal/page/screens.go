package page

import (
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/doctor"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
)

// --- dashboard and server list ---

type dashData struct {
	Servers  []instances.View
	Running  int
	Players  int
	Unknown  int // running servers whose player count is not known
	Budget   []budgetRow
	Activity []audit.Entry
}

// budgetRow is one bar of the box budget.
type budgetRow struct {
	Label, Value, Note string
	Known              bool
	Step               int // the bar's length in steps of 5 percent
}

func (p *Page) dashboard(v *view) {
	v.Title, v.Nav, v.Screen = "Dashboard", "dashboard", "dashboard"
	var list api.InstanceList
	if err := v.c.get("/api/v1/instances", &list); err != nil {
		p.fail(v.w, v.r, statusOf(err), err.Error())
		return
	}
	d := dashData{Servers: list.Instances, Budget: budget(v.Box, list.Instances)}
	for _, s := range list.Instances {
		if s.Status.State != instances.Running {
			continue
		}
		d.Running++
		if s.Live.Players != nil {
			d.Players += *s.Live.Players
		} else {
			d.Unknown++
		}
	}
	v.Events = "types=state"
	if v.Admin() {
		var al api.AuditList
		if err := v.c.get("/api/v1/audit?limit=8", &al); err == nil {
			d.Activity = al.Entries
			slices.Reverse(d.Activity) // newest first
		}
		v.Events = "types=state,audit"
	}
	v.Data = d
	p.render(v, "dashboard", http.StatusOK)
}

// budget makes the box budget bars from the box facts and the instances.
func budget(b api.Box, list []instances.View) []budgetRow {
	u := b.Usage
	var ceilings int64
	n := 0
	for _, s := range list {
		if m, err := instances.ParseMemory(s.Limits.MemoryMax); err == nil && m > 0 {
			ceilings += m
			n++
		}
	}
	mem := budgetRow{Label: "Memory"}
	switch {
	case u.MemoryTotalBytes != nil && *u.MemoryTotalBytes > 0:
		used := *u.MemoryTotalBytes - *u.MemoryAvailableBytes
		mem.Known, mem.Step = true, step(used, *u.MemoryTotalBytes)
		mem.Value = sizeOf(used) + " / " + sizeOf(*u.MemoryTotalBytes)
		mem.Note = "In use on the whole box. "
		if n == 0 {
			mem.Note += "No server has a memory ceiling."
		} else {
			mem.Note += "Memory ceilings of servers: " + sizeOf(ceilings) + " in all (" + plural(n, "server") + ")."
			if !b.LimitsEnforced {
				mem.Note += " Not enforced in " + b.Supervisor + " mode."
			}
		}
	default:
		mem.Value, mem.Note = "unknown", unknownWhy(u.Unknown, "memory_total_bytes")
	}
	cpu := budgetRow{Label: "CPU", Value: strconv.Itoa(u.CPUCount) + " CPUs"}
	if u.Load1 != nil {
		cpu.Known = true
		cpu.Value = "load " + strconv.FormatFloat(*u.Load1, 'f', 2, 64) + " on " + strconv.Itoa(u.CPUCount) + " CPUs"
		cpu.Step = step(int64(*u.Load1*1000), int64(u.CPUCount)*1000)
		cpu.Note = "Load average over the last minute."
	} else {
		cpu.Note = "Load unknown: " + unknownWhy(u.Unknown, "load_1m") + "."
	}
	disk := budgetRow{Label: "Disk"}
	if u.DiskTotalBytes != nil && *u.DiskTotalBytes > 0 {
		used := *u.DiskTotalBytes - *u.DiskFreeBytes
		disk.Known, disk.Step = true, step(used, *u.DiskTotalBytes)
		disk.Value = sizeOf(used) + " / " + sizeOf(*u.DiskTotalBytes)
		disk.Note = sizeOf(*u.DiskFreeBytes) + " free where the root folder is (builds, worlds, backups)."
	} else {
		disk.Value, disk.Note = "unknown", unknownWhy(u.Unknown, "disk_total_bytes")
	}
	return []budgetRow{mem, cpu, disk}
}

func (p *Page) servers(v *view) {
	v.Title, v.Nav, v.Screen, v.Events = "Servers", "servers", "servers", "types=state"
	var list api.InstanceList
	if err := v.c.get("/api/v1/instances", &list); err != nil {
		p.fail(v.w, v.r, statusOf(err), err.Error())
		return
	}
	v.Data = list.Instances
	p.render(v, "servers", http.StatusOK)
}

// --- one server ---

// serverData is what every server screen has, plus the screen's own data.
type serverData struct {
	S      instances.View
	Tab    string
	Hidden bool // the caller may not see this server's output
	More   any
}

// server loads the instance named in the path for a server screen.
func (p *Page) server(v *view, tab string) (*serverData, bool) {
	name := v.r.PathValue("name")
	var s instances.View
	if err := v.c.get("/api/v1/instances/"+url.PathEscape(name), &s); err != nil {
		p.fail(v.w, v.r, statusOf(err), err.Error())
		return nil, false
	}
	title := s.DisplayName
	if title == "" {
		title = s.Name
	}
	v.Title, v.Nav, v.Screen = title, "servers", tab
	d := &serverData{S: s, Tab: tab, Hidden: !v.Operator() && slices.Contains(s.Warnings, instances.WarnSecretsOnCommandLine)}
	v.Data = d
	return d, true
}

// logLines gets the last n lines of a server's output, or why they are not
// shown.
func (v *view) logLines(d *serverData, source string, n int) (lines []logLine, why string) {
	if d.Hidden {
		return nil, instances.HiddenNotice
	}
	var l api.Logs
	err := v.c.get("/api/v1/instances/"+url.PathEscape(d.S.Name)+"/logs?lines="+strconv.Itoa(n)+"&source="+url.QueryEscape(source), &l)
	if err != nil {
		return nil, err.Error()
	}
	for _, t := range l.Lines {
		lines = append(lines, logLine{Text: t, Level: lineLevel(t)})
	}
	return lines, ""
}

type logLine struct {
	Text  string
	Level string // "error", "warning" or ""
}

type overviewData struct {
	Updates   []api.BuildView // builds the server can update to
	Log       []logLine
	LogWhy    string
	Events    []audit.Entry
	EventsWhy string
}

func (p *Page) overview(v *view) {
	d, ok := p.server(v, "overview")
	if !ok {
		return
	}
	o := overviewData{}
	o.Log, o.LogWhy = v.logLines(d, instances.SourceServer, 20)
	if v.Operator() {
		var bl api.BuildList
		if err := v.c.get("/api/v1/builds", &bl); err == nil {
			for _, b := range bl.Builds {
				if b.Channel == d.S.Channel && b.Version != d.S.Build {
					o.Updates = append(o.Updates, b)
				}
			}
		}
	}
	if v.Admin() {
		var al api.AuditList
		if err := v.c.get("/api/v1/audit?limit=500", &al); err == nil {
			for i := len(al.Entries) - 1; i >= 0 && len(o.Events) < 8; i-- {
				if e := al.Entries[i]; targets(e.Target, d.S.Name) {
					o.Events = append(o.Events, e)
				}
			}
		} else {
			o.EventsWhy = err.Error()
		}
	} else {
		o.EventsWhy = "Earlier events are in the audit log, which needs the admin role. State changes appear here while this page is open."
	}
	d.More = o
	v.Events = "instance=" + url.QueryEscape(d.S.Name) + "&types=state,log"
	p.render(v, "overview", http.StatusOK)
}

// targets reports whether an audit target is about instance name.
func targets(target, name string) bool {
	return target == name || strings.HasPrefix(target, name+"/") || strings.HasPrefix(target, name+" ")
}

type settingsData struct {
	Rules []rule
}

type rule struct{ Key, Value string }

func (p *Page) settings(v *view) {
	d, ok := p.server(v, "settings")
	if !ok {
		return
	}
	s := settingsData{}
	for _, k := range slices.Sorted(maps.Keys(d.S.Rules)) {
		s.Rules = append(s.Rules, rule{Key: k, Value: d.S.Rules[k]})
	}
	d.More = s
	v.Events = "instance=" + url.QueryEscape(d.S.Name) + "&types=state"
	p.render(v, "settings", http.StatusOK)
}

type logData struct {
	Source string
	Lines  []logLine
	Why    string
	Shown  int
}

// logScreenLines is how many lines the log screen starts with.
const logScreenLines = 500

func (p *Page) logScreen(v *view) {
	d, ok := p.server(v, "log")
	if !ok {
		return
	}
	src := v.r.URL.Query().Get("source")
	if src != instances.SourceConsole {
		src = instances.SourceServer
	}
	l := logData{Source: src, Shown: logScreenLines}
	l.Lines, l.Why = v.logLines(d, src, logScreenLines)
	d.More = l
	v.Events = "instance=" + url.QueryEscape(d.S.Name) + "&types=state,log"
	p.render(v, "log", http.StatusOK)
}

func (p *Page) players(v *view) {
	if _, ok := p.server(v, "players"); !ok {
		return
	}
	p.render(v, "players", http.StatusOK)
}

type backupsData struct {
	List  []instances.Backup
	Total int64
	Why   string
}

func (p *Page) backups(v *view) {
	d, ok := p.server(v, "backups")
	if !ok {
		return
	}
	b := backupsData{}
	var bl api.BackupList
	if err := v.c.get("/api/v1/instances/"+url.PathEscape(d.S.Name)+"/backups", &bl); err != nil {
		b.Why = err.Error()
	}
	b.List = bl.Backups
	for _, x := range b.List {
		b.Total += x.SizeBytes
	}
	d.More = b
	v.Events = "instance=" + url.QueryEscape(d.S.Name) + "&types=state"
	p.render(v, "backups", http.StatusOK)
}

func (p *Page) schedule(v *view) {
	d, ok := p.server(v, "schedule")
	if !ok {
		return
	}
	v.Events = "instance=" + url.QueryEscape(d.S.Name) + "&types=state"
	p.render(v, "schedule", http.StatusOK)
}

// --- builds, clusters, box, audit ---

type buildRow struct {
	api.BuildView
	Current []string // instances running it now
	Kept    []string // instances keeping it for a rollback
}

type buildsData struct {
	Builds []buildRow
	Total  int64
}

func (p *Page) builds(v *view) {
	v.Title, v.Nav, v.Screen, v.Events = "Builds", "builds", "builds", "types=build"
	var bl api.BuildList
	var il api.InstanceList
	err := v.c.get("/api/v1/builds", &bl)
	if err == nil {
		err = v.c.get("/api/v1/instances", &il)
	}
	if err != nil {
		p.fail(v.w, v.r, statusOf(err), err.Error())
		return
	}
	d := buildsData{}
	for _, b := range bl.Builds {
		row := buildRow{BuildView: b}
		for _, s := range il.Instances {
			switch {
			case s.Build == b.Version:
				row.Current = append(row.Current, s.Name)
			case slices.Contains(b.UsedBy, s.Name):
				row.Kept = append(row.Kept, s.Name)
			}
		}
		d.Builds = append(d.Builds, row)
		d.Total += b.SizeBytes
	}
	v.Data = d
	p.render(v, "builds", http.StatusOK)
}

type clustersData struct {
	Clusters []instances.Cluster
	Names    map[string]string // instance name -> display name
	Alone    []instances.View  // instances in no cluster
}

func (p *Page) clusters(v *view) {
	v.Title, v.Nav, v.Screen = "Clusters", "clusters", "clusters"
	var cl api.ClusterList
	var il api.InstanceList
	err := v.c.get("/api/v1/clusters", &cl)
	if err == nil {
		err = v.c.get("/api/v1/instances", &il)
	}
	if err != nil {
		p.fail(v.w, v.r, statusOf(err), err.Error())
		return
	}
	d := clustersData{Clusters: cl.Clusters, Names: map[string]string{}}
	for _, s := range il.Instances {
		d.Names[s.Name] = s.DisplayName
		if s.Cluster == "" {
			d.Alone = append(d.Alone, s)
		}
	}
	v.Data = d
	p.render(v, "clusters", http.StatusOK)
}

type boxData struct {
	Checked   bool // the health check was asked for and run
	Doctor    doctor.Report
	DoctorWhy string
	Firewall  api.Firewall
	Servers   []instances.View
	GameUsed  []int
	QueryUsed []int
	Ceilings  int64
	Ceilinged int
	Quotas    int
	Tokens    []api.TokenView
}

func (p *Page) box(v *view) {
	v.Title, v.Nav, v.Screen = "Box", "box", "box"
	d := boxData{}
	var il api.InstanceList
	err := v.c.get("/api/v1/instances", &il)
	if err == nil {
		err = v.c.get("/api/v1/box/firewall", &d.Firewall)
	}
	if err != nil {
		p.fail(v.w, v.r, statusOf(err), err.Error())
		return
	}
	// The health check test-binds the free ports of the ranges: it runs
	// only when asked for, not on every look at the box.
	if v.r.URL.Query().Get("check") == "1" {
		d.Checked = true
		if err := v.c.get("/api/v1/box/doctor", &d.Doctor); err != nil {
			d.DoctorWhy = err.Error()
		}
	}
	d.Servers = il.Instances
	for _, s := range il.Instances {
		d.GameUsed = append(d.GameUsed, s.Ports.Game)
		d.QueryUsed = append(d.QueryUsed, s.Ports.Query)
		if m, err := instances.ParseMemory(s.Limits.MemoryMax); err == nil && m > 0 {
			d.Ceilings += m
			d.Ceilinged++
		}
		if s.Limits.CPUQuotaPercent > 0 {
			d.Quotas++
		}
	}
	slices.Sort(d.GameUsed)
	slices.Sort(d.QueryUsed)
	if v.Admin() {
		var tl api.TokenList
		if err := v.c.get("/api/v1/tokens", &tl); err == nil {
			d.Tokens = tl.Tokens
		}
	}
	v.Data = d
	p.render(v, "box", http.StatusOK)
}

func (p *Page) audit(v *view) {
	v.Title, v.Nav, v.Screen = "Audit log", "box", "audit"
	var entries []audit.Entry
	if v.Admin() {
		var al api.AuditList
		if err := v.c.get("/api/v1/audit?limit=200", &al); err != nil {
			p.fail(v.w, v.r, statusOf(err), err.Error())
			return
		}
		// Newest first.
		for i := len(al.Entries) - 1; i >= 0; i-- {
			entries = append(entries, al.Entries[i])
		}
	}
	v.Data = entries
	p.render(v, "audit", http.StatusOK)
}

// --- setup and login ---

type setupData struct {
	SetupNeeded bool
	Next        string
}

// setup is the first-run form while no admin exists, and the login form
// after that. A caller who is logged in already goes on.
func (p *Page) setup(w http.ResponseWriter, r *http.Request) {
	v := &view{w: w, r: r, c: client{h: p.api, r: r}, Bare: true, Screen: "setup"}
	next := safeNext(r.URL.Query().Get("next"))
	err := v.c.get("/api/v1/session", &v.Session)
	if err == nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		p.fail(w, r, statusOf(err), err.Error())
		return
	}
	var st api.SetupStatus
	if err := v.c.get("/api/v1/setup", &st); err != nil {
		p.fail(w, r, statusOf(err), err.Error())
		return
	}
	v.Title = "Log in"
	if st.SetupNeeded {
		v.Title = "Set up this manager"
	}
	v.Data = setupData{SetupNeeded: st.SetupNeeded, Next: next}
	p.render(v, "setup", http.StatusOK)
}
