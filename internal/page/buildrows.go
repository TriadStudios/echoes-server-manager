package page

import (
	"slices"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/steam"
)

// What a build is to the servers.
const (
	buildRunning = "running" // a server runs it now
	buildKept    = "kept"    // no server runs it; one keeps it for a rollback
	buildUnused  = "unused"  // no server uses it
)

// buildRow is one installed build on the Builds screen.
type buildRow struct {
	api.BuildView
	Current []string // instances running it now
	Kept    []string // instances keeping it for a rollback
	// Status is what the build is to the servers: buildRunning (also when
	// another server keeps it for a rollback), buildKept or buildUnused.
	Status string
	// Newest says that it is the build of its channel that was installed
	// last.
	Newest bool
}

type buildsData struct {
	Builds []buildRow // the one installed last first
	Total  int64
	// Unused counts the builds no server uses, and Freed is their size
	// together: what "Remove unused builds" frees.
	Unused int
	Freed  int64
	// Sources are the Steam sources as GET /api/v1/steam lists them, and
	// SourcesWhy says why they are not shown.
	Sources    []steam.SourceView
	SourcesWhy string
}

// buildRows makes the rows of the Builds screen from the installed builds
// and the servers. The builds are put newest first (see newestFirst).
func buildRows(list []api.BuildView, servers []instances.View) buildsData {
	d := buildsData{}
	seen := map[string]bool{}
	for _, b := range newestFirst(list) {
		row := buildRow{BuildView: b, Newest: !seen[b.Channel]}
		seen[b.Channel] = true
		for _, s := range servers {
			if s.Build == b.Version {
				row.Current = append(row.Current, s.Name)
			}
		}
		for _, name := range b.UsedBy {
			if !slices.Contains(row.Current, name) {
				row.Kept = append(row.Kept, name)
			}
		}
		switch {
		case len(row.Current) > 0:
			row.Status = buildRunning
		case len(b.UsedBy) > 0:
			row.Status = buildKept
		default:
			row.Status = buildUnused
			d.Unused++
			d.Freed += b.SizeBytes
		}
		d.Builds = append(d.Builds, row)
		d.Total += b.SizeBytes
	}
	return d
}
