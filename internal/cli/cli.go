// Package cli is the esm command line. Apart from serve, every verb is a
// thin client of the HTTP API: by default over the unix socket of the local
// ESM root, or with --url and a token against a remote ESM.
//
// Every verb prints exactly one result line, or one JSON object with
// --json, and exits 0 on success and 1 on error. Errors go to stderr, or to
// stdout as {"error": {...}} with --json. The exceptions are logs, which
// prints the log lines, and help.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// globals are the flags every verb accepts.
type globals struct {
	root  string
	url   string
	token string
	json  bool
}

// register adds the global flags to fs, keeping values already parsed
// (they may come before or after the verb).
func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.root, "root", g.root, "ESM root folder (default $ESM_ROOT or ~/durinn)")
	fs.StringVar(&g.url, "url", g.url, "URL of a remote ESM (needs --token or $ESM_TOKEN)")
	fs.StringVar(&g.token, "token", g.token, "API token for --url")
	fs.BoolVar(&g.json, "json", g.json, "print one JSON object")
}

// env is one run of the command.
type env struct {
	g       globals
	out     io.Writer
	errOut  io.Writer
	version string
	ctx     context.Context
}

// usageError is a mistake on the command line.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErr(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

const usage = `esm - Echoes Server Manager

Usage: esm [--root DIR] [--json] [--url URL --token TOKEN] <command> [arguments]

Service:
  serve                         run the ESM service (game servers keep running when it stops)
  doctor [--bind IP]            check this box (read-only); --bind checks free ports
                                on that address instead of all interfaces
  version                       print the version

Instances:
  list                          list instances
  status [name]                 show the box, or one instance
  create <name> [--build V] [--channel live|test] [--map M] [--world W]
         [--display-name N] [--cluster C] [--game-port P] [--query-port P]
         [--bind IP] [--max-players N] [--set Key=Value ...] [--arg -X=Y ...]
         [--secret-delivery file|command_line]
  start|stop|restart <name>
  logs <name> [-n N] [-f] [--console]
  update <name> --to <version>
  backup <name>
  restore <name> <backup-id>

Builds:
  build install <folder|archive|https-url> [--sha256 HEX]   (local sources from the import folder)
  build list
  build prune

Tokens:
  token create --role read|operator|admin [--name NAME]
`

// Main runs the command line with args (without the program name) and
// returns the exit code.
func Main(args []string, stdout, stderr io.Writer, version string) int {
	e := &env{out: stdout, errOut: stderr, version: version, ctx: context.Background()}
	top := flag.NewFlagSet("esm", flag.ContinueOnError)
	top.SetOutput(io.Discard)
	e.g.register(top)
	if err := top.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		return e.fail(usageErr("%v", err))
	}
	rest := top.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, usage)
		return 1
	}
	verb, rest := rest[0], rest[1:]
	var err error
	switch verb {
	case "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version":
		err = e.versionCmd(rest)
	case "serve":
		err = e.serve(rest)
	case "doctor":
		return e.doctor(rest)
	case "list":
		err = e.list(rest)
	case "status":
		err = e.status(rest)
	case "create":
		err = e.create(rest)
	case "start", "stop", "restart":
		err = e.lifecycle(verb, rest)
	case "logs":
		err = e.logs(rest)
	case "update":
		err = e.update(rest)
	case "backup":
		err = e.backup(rest)
	case "restore":
		err = e.restore(rest)
	case "build":
		err = e.build(rest)
	case "token":
		err = e.token(rest)
	default:
		err = usageErr("unknown command %q (see esm help)", verb)
	}
	if err != nil {
		return e.fail(err)
	}
	return 0
}

// flags makes a flag set for a verb that also knows the global flags.
func (e *env) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	e.g.register(fs)
	return fs
}

// parse parses flags and positional arguments in any order.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, usageErr("see esm help")
			}
			return nil, usageErr("%s: %v", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// want checks the number of positional arguments.
func want(name string, pos []string, n int, what string) error {
	if len(pos) != n {
		return usageErr("%s needs %s", name, what)
	}
	return nil
}

// result prints v as one JSON object, or line.
func (e *env) result(v any, line string) error {
	if e.g.json {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s\n", b)
		return err
	}
	_, err := fmt.Fprintln(e.out, line)
	return err
}

// fail reports err and returns the exit code 1.
func (e *env) fail(err error) int {
	code := "error"
	var re *remoteError
	var ue *usageError
	switch {
	case errors.As(err, &re):
		code = re.Code
	case errors.As(err, &ue):
		code = "usage"
	}
	if e.g.json {
		b, _ := json.Marshal(map[string]map[string]string{"error": {"code": code, "message": err.Error()}})
		fmt.Fprintf(e.out, "%s\n", b)
		return 1
	}
	fmt.Fprintf(e.errOut, "esm: %v\n", err)
	return 1
}

func (e *env) client() (*client, error) { return newClient(&e.g) }

func (e *env) versionCmd(args []string) error {
	fs := e.flags("version")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	return e.result(map[string]string{"version": e.version}, "esm "+e.version)
}

// interruptible returns a context that ends on Ctrl-C or SIGTERM.
func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}
