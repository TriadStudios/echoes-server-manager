package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
)

// errPasswdFlags is the one answer to flags that user passwd cannot read:
// the error of the flag package repeats a flag's name or value, which may
// be the password typed in the wrong place.
var errPasswdFlags = usageErr("user passwd was given a flag it does not take, or a value it cannot read; what was given is not repeated, since it may be the password. It takes one login name, and --root and --json")

// namesPasswd reports whether args (of the whole command line, or of the
// user verb) look like user passwd: "passwd" after "user", or "passwd" at
// all when user is the verb already. It errs on the side of yes.
func namesPasswd(args []string, verb bool) bool {
	i := slices.Index(args, "passwd")
	return i >= 0 && (verb || slices.Contains(args[:i], "user"))
}

// user is "esm user list" and "esm user passwd <name>".
func (e *env) user(args []string) error {
	fs := e.flags("user")
	pos, err := parse(fs, args)
	if err != nil {
		if namesPasswd(args, true) {
			return errPasswdFlags
		}
		return err
	}
	if len(pos) == 0 {
		return usageErr("user needs list or passwd <name>")
	}
	switch pos[0] {
	case "list":
		if len(pos) != 1 {
			return usageErr("user list takes no arguments")
		}
		c, err := e.client()
		if err != nil {
			return err
		}
		var out api.UserList
		if err := c.do(e.ctx, "GET", "/api/v1/users", nil, &out); err != nil {
			return err
		}
		return e.result(out, describeUsers(out.Users))
	case "passwd":
		if len(pos) != 2 {
			// What else was given is not repeated: it may be the password.
			return usageErr("user passwd takes one login name and nothing else; the new password is asked for, or read from standard input, never taken from the command line")
		}
		return e.passwd(pos[1])
	}
	return usageErr("user needs list or passwd <name>")
}

func describeUsers(list []api.UserView) string {
	if len(list) == 0 {
		return "no logins yet (the setup code makes the first)"
	}
	parts := make([]string, len(list))
	for i, u := range list {
		last := "never logged in"
		if !u.LastLogin.IsZero() {
			last = "last login " + ago(u.LastLogin)
		}
		parts[i] = fmt.Sprintf("%s (%s, %s, %s open)", u.Username, u.Role, last, plural(u.Sessions, "session", "sessions"))
	}
	return plural(len(list), "login", "logins") + ": " + strings.Join(parts, ", ")
}

// passwd is "esm user passwd <name>": the box owner sets the password of a
// login without the current one, the way back in after a forgotten
// password. It works only over the unix socket, which only ESM's account
// can open; the service ends every session of the login.
func (e *env) passwd(name string) error {
	if e.g.url != "" {
		return usageErr("user passwd works only on the box, over ESM's unix socket: run it there without --url (a login changes its own password on the page, on the Box screen)")
	}
	c, err := e.client()
	if err != nil {
		return e.unrepeated(err)
	}
	// The login is looked up first, so that nothing is asked for in vain.
	var list api.UserList
	if err := c.do(e.ctx, "GET", "/api/v1/users", nil, &list); err != nil {
		return e.unrepeated(err)
	}
	login := ""
	for _, u := range list.Users {
		if strings.EqualFold(u.Username, name) {
			login = u.Username
		}
	}
	if login == "" {
		// The name is not repeated: it may be a password typed in the
		// wrong place.
		return errors.New("there is no login of that name (esm user list shows them)")
	}
	pw, err := e.newPassword(login)
	if err != nil {
		return err
	}
	var out api.PasswordResult
	if err := c.do(e.ctx, "PUT", "/api/v1/users/"+url.PathEscape(login)+"/password", api.PasswordResetRequest{Password: pw}, &out); err != nil {
		return e.unrepeated(err)
	}
	line := "set the password of " + out.Username + " (no session was open)"
	if out.EndedSessions > 0 {
		line = fmt.Sprintf("set the password of %s and ended its %s", out.Username, plural(out.EndedSessions, "session", "sessions"))
	}
	return e.result(out, line)
}

// unrepeated keeps user passwd from repeating text of its arguments in an
// error. An answer of the service repeats none; but an error of reaching
// it names the socket's path, which is made from --root when that was
// given, so then it is said without the path.
func (e *env) unrepeated(err error) error {
	var re *remoteError
	if err == nil || errors.As(err, &re) || e.g.root == "" {
		return err
	}
	return errors.New("cannot reach the ESM service on the unix socket of the root given with --root (is esm serve running for it?); the path is not repeated, since an argument may be the password")
}

// newPassword reads a new password for login. On a terminal it asks twice
// and shows nothing of what is typed; otherwise it reads one line of
// standard input. It never takes one from the command line or the
// environment, where other accounts on the box could read it.
func (e *env) newPassword(login string) (string, error) {
	f, ok := e.in.(*os.File)
	if !ok || !isTerminal(f) {
		pw, err := readLine(e.in)
		if err != nil {
			return "", err
		}
		if pw == "" {
			return "", usageErr("give the new password as one line on standard input, like: esm user passwd <name> < file; in a terminal it is asked for")
		}
		return pw, nil
	}
	first, err := e.askHidden(f, "New password for "+login+": ")
	if err != nil {
		return "", err
	}
	if first == "" {
		return "", errors.New("no password typed; nothing was changed")
	}
	again, err := e.askHidden(f, "The same password again: ")
	if err != nil {
		return "", err
	}
	if first != again {
		return "", errors.New("the two passwords are not the same; nothing was changed")
	}
	return first, nil
}

// hideInput turns the echo of a terminal off (tests replace it).
var hideInput = echoOff

// askHidden writes prompt to stderr and reads a line from the terminal f
// with its echo off. The echo is turned back on also when a signal that
// would end the command arrives while it reads (stopSignals: Ctrl-C, and on
// Linux also Ctrl-\ and a hang-up). Those signals are caught before the
// echo goes off, so that none of them can end the command in between with
// the echo still off.
func (e *env) askHidden(f *os.File, prompt string) (string, error) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, stopSignals...)
	defer signal.Stop(stop)
	fmt.Fprint(e.errOut, prompt)
	restore, err := hideInput(f)
	if err != nil {
		fmt.Fprintln(e.errOut)
		return "", err
	}
	type answer struct {
		line string
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		line, err := readLine(f)
		got <- answer{line, err}
	}()
	var a answer
	select {
	case a = <-got:
	case <-stop:
		a.err = errors.New("interrupted; nothing was changed")
	}
	restore()
	// The Enter that ended the line was not shown either.
	fmt.Fprintln(e.errOut)
	return a.line, a.err
}

// maxPasswordInput is the most a line with a password may hold. The service
// refuses a password longer than 1024 bytes in any case.
const maxPasswordInput = 4096

// readLine reads one line from r a byte at a time, so that nothing after
// it is taken from r, without its line end ("\n" or "\r\n"). The end of
// the input ends a line too.
func readLine(r io.Reader) (string, error) {
	var b []byte
	var one [1]byte
	for {
		n, err := r.Read(one[:])
		if n == 1 {
			if one[0] == '\n' {
				return strings.TrimSuffix(string(b), "\r"), nil
			}
			if len(b) == maxPasswordInput {
				return "", fmt.Errorf("the password line is longer than %d bytes", maxPasswordInput)
			}
			b = append(b, one[0])
		}
		if errors.Is(err, io.EOF) {
			return strings.TrimSuffix(string(b), "\r"), nil
		}
		if err != nil {
			return "", err
		}
	}
}
