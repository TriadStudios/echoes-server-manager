package page

import (
	"slices"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/steam"
)

// updateState is the one status of a server's update: its word, and its
// tone, which is the colour of its pill (ok, avail, wait, fail or info).
type updateState struct{ Tone, Word string }

// updateStatus picks the one status that a server's update has, from what
// its view says. The first of these that holds wins:
//
//   - the last automatic update failed (Update.LastError);
//   - a newer build waits for something (Update.Waiting: players on, a
//     pause after an update by hand);
//   - a newer build of the Steam source it follows is installed;
//   - an automatic update switched the build while the server was stopped
//     and the next start has not proven it yet (Status.UpdateUnverified);
//   - nothing of that: the server is up to date.
//
// A failed update also sets Update.Waiting (it is not tried again), so the
// failure is looked at first.
func updateStatus(v instances.View) updateState {
	switch {
	case v.Update.LastError != "":
		return updateState{"fail", "Update failed"}
	case v.UpdateAvailable && v.Update.Waiting != "":
		return updateState{"wait", "Waiting"}
	case v.UpdateAvailable:
		return updateState{"avail", "Update available"}
	case v.Status.UpdateUnverified:
		return updateState{"info", "New build, checked at next start"}
	}
	return updateState{"ok", "Up to date"}
}

// updateOption is one build in the update form of a server.
type updateOption struct {
	Version, Channel, Commit string
	// Running says that the server runs this build now: it is shown and
	// marked, but cannot be chosen.
	Running bool
	// Newest says that it is the build of the server's channel that was
	// installed last.
	Newest bool
	// Selected says that the form starts on it.
	Selected bool
}

// updateChoice is what the update form of a server offers.
type updateChoice struct {
	// Options are the installed builds of the server's channel, newest
	// first, the one it runs among them.
	Options []updateOption
	// Others counts the options that can be chosen (all but the running
	// build).
	Others int
	// Selected says that an option is selected; when none is, the form asks
	// for a choice.
	Selected bool
	// Behind is how many builds of the server's channel are newer than its
	// own, when that is known: 0 when none is, and also when it cannot be
	// told (see buildsBehind).
	Behind int
}

// updateChoices makes the update form of s from the installed builds. The
// form starts on the build that s follows to (Update.Target) or else on the
// newest build, but only when that is not the build s runs already: then
// nothing is chosen for the operator.
func updateChoices(s instances.View, all []api.BuildView) updateChoice {
	var c updateChoice
	list := newestFirst(all)
	var same []api.BuildView
	for _, b := range list {
		if b.Channel == s.Channel {
			same = append(same, b)
		}
	}
	for i, b := range same {
		o := updateOption{Version: b.Version, Channel: b.Channel, Commit: b.Commit, Running: b.Version == s.Build, Newest: i == 0}
		if !o.Running {
			c.Others++
		}
		c.Options = append(c.Options, o)
	}
	pick := s.Update.Target
	if !c.can(pick) && len(c.Options) > 0 {
		pick = c.Options[0].Version
	}
	if c.can(pick) {
		for i := range c.Options {
			if c.Options[i].Version == pick {
				c.Options[i].Selected, c.Selected = true, true
			}
		}
	}
	c.Behind = buildsBehind(s.Build, same)
	return c
}

// can reports whether version is an option that may be chosen.
func (c updateChoice) can(version string) bool {
	for _, o := range c.Options {
		if o.Version == version && !o.Running {
			return true
		}
	}
	return false
}

// buildsBehind counts the builds of list that are newer than build. It is
// told only for a build made from a Steam source, whose build id says
// which of the source's builds is newer: the builds of that source with a
// greater id. For any other build there is no order but the order of
// installing, which says nothing about versions the operator named, so 0
// is given.
func buildsBehind(build string, list []api.BuildView) int {
	src, id, ok := steam.SourceOf(build)
	if !ok {
		return 0
	}
	n := 0
	for _, b := range list {
		if s, i, ok := steam.SourceOf(b.Version); ok && s == src && idLess(id, i) {
			n++
		}
	}
	return n
}

// idLess compares two build ids (digits) as numbers.
func idLess(a, b string) bool {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// newestFirst is the builds, the one installed last first; builds installed
// at the same time stay in the order the API gave.
func newestFirst(list []api.BuildView) []api.BuildView {
	out := append([]api.BuildView(nil), list...)
	slices.SortStableFunc(out, func(a, b api.BuildView) int { return b.Installed.Compare(a.Installed) })
	return out
}
