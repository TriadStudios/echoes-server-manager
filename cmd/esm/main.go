// Command esm is the Echoes Server Manager: it installs, runs, updates and
// watches Echoes of Durinn dedicated servers on one box.
//
// Only the version verb exists so far; the service and the other verbs follow.
package main

import (
	"fmt"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] != "version" && os.Args[1] != "--version" {
		fmt.Fprintf(os.Stderr, "esm: unknown command %q (only \"version\" is available so far)\n", os.Args[1])
		os.Exit(1)
	}
	fmt.Println("esm", version)
}
