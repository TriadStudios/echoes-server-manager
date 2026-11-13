// Command esm is the Echoes Server Manager: it installs, runs, updates and
// watches Echoes of Durinn dedicated servers on one box.
//
// "esm serve" runs the service; every other verb is a thin client of its
// HTTP API (see esm help).
package main

import (
	"os"

	"github.com/TriadStudios/echoes-server-manager/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr, version))
}
