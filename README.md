# ESM: Echoes Server Manager

ESM installs, runs, updates and watches Echoes of Durinn dedicated servers on
one machine. It is a single Go program: `esm serve` runs the service, and the
same binary is the command line that talks to it, and it serves a web page
for the same work. Everything ESM does is also available through a small
HTTP API, which the command line and the web page use too.

## Status

Early development. **Not ready for real hosts yet.**

- Everything ESM assumes about the real game server (command-line switches,
  log lines, save file locations, how it stops) lives in one place,
  [`internal/gameserver`](internal/gameserver/gameserver.go), and is marked
  UNCONFIRMED until it has been proven on a real host. Until then ESM is
  tested against a fake server that follows those assumptions.
- By default ESM hands a server its passwords in a private file and never
  puts them on a command line. The real game server does not read that
  file yet: a per-instance config file that the server reads its passwords
  from is a planned item on the game side. Until it ships, a real server
  only gets passwords in a compatibility mode that is unsafe on a shared
  box; that game-side file is what removes the mode. See
  [Server passwords](#server-passwords).
- Linux is the first target. Windows hosts are planned for the first public
  release; the code already builds and its tests pass there.
- The web page is plain HTML made by the service, with one stylesheet and
  one small script and nothing loaded from anywhere else. It may be rebuilt
  with [Den](https://github.com/TriadStudios/Den) later. See
  [Web page](#web-page).

## Build

You need Go 1.24 or newer. ESM uses only the Go standard library.

```sh
go build ./cmd/esm
```

## Run

```sh
esm serve --root ~/durinn
```

The root folder holds everything ESM owns: config, builds, instances,
backups, logs and secrets. Without `--root` ESM uses `$ESM_ROOT`, then
`~/durinn`. For a service install, give ESM its own unprivileged account,
let it own a folder such as `/opt/durinn`, and pass `--root /opt/durinn`.
ESM refuses to run as root.

On first start ESM writes `esm.json` with defaults and prints a one-time
**setup code** to stderr. The setup code creates the first admin user for
the web page (`POST /api/v1/setup`). You do not need it for the command
line: on the box, the command line talks to the service over the unix
socket `<root>/esm.sock`, which only the ESM account can open, and has full
rights.

For scripts and other machines, make an API token:

```sh
esm token create --role operator --name deploy
```

The token is shown once. Send it as `Authorization: Bearer <token>`, or use
it with the command line: `esm --url https://<your-host> --token <token> list`.

### Linux: keep servers running

ESM prefers systemd user units for game servers (memory and CPU limits are
then enforced). User units stop when the account logs out unless lingering
is on; the box owner turns it on once:

```sh
sudo loginctl enable-linger <esm-account>
```

Without systemd, ESM starts servers as detached processes (limits are not
enforced in that mode). Either way, stopping ESM leaves the game servers
running; the next `esm serve` finds them again.

## Web page

The page is served on the same address as the API: by default
`http://127.0.0.1:8740`, on the box itself. From another machine, use an
SSH tunnel (`ssh -L 8740:127.0.0.1:8740 <esm-account>@<box>`, then open
`http://127.0.0.1:8740`) or a reverse proxy with TLS (set
`behind_tls_proxy`). The first visit asks for the setup code and creates
the admin login; after that the page is the login.

It shows the box (memory, CPUs, disk, ports, firewall commands, health
checks, tokens), every server (state, players, memory against its ceiling,
settings, live log, backups, timed restarts), builds, clusters and the audit
log, and it starts, stops, restarts, updates, backs up, restores, installs
builds and changes settings. Each screen shows only what the API returns;
where ESM cannot know something yet (CPU use per server, the player list,
rule defaults, whether players can reach the box from outside), the page
says so. Reading works without JavaScript; changes need it. What each role
may do is the same as in the API.

## Command line tour

```sh
esm doctor                                   # read-only checks of this box
esm build install ./build-1.0.0.tar.gz       # or a folder, or an https URL with --sha256
esm build list
esm create alpha --set Difficulty=hard       # ports are picked from the configured ranges
esm start alpha                              # waits until the server is ready
esm status alpha
esm logs alpha -n 100 -f
esm backup alpha
esm update alpha --to 1.1.0                  # rolls back if the new build never gets ready
esm restore alpha 20260101T120000Z-manual
esm stop alpha
esm list --json
```

Every command prints one result line, or one JSON object with `--json`, and
exits 0 on success and 1 on error. `esm help` lists every command.

## Server passwords

An instance has a server password (to join) and an admin password. They are
set only through the API, `PUT /api/v1/instances/<name>/secrets`, kept in a
private file, and never shown. Rules (`--set`) and extra arguments (`--arg`)
are shown to everyone who may look at an instance, so ESM refuses names
there that look like a secret (`JoinPassword`, `RconKey`, `AuthToken`, ...).

**By default the passwords travel in a file.** Before each start ESM writes
them to `instances/<name>/run/server-secrets.json` (readable by the ESM
account only), tells the server where the file is, and removes the file
when the server has stopped. No password is on a command line, so none is
in the process list or in what the server logs, and there is nothing ESM
would have to hide. This needs a game server that reads the file. A server
build that cannot do that runs without passwords; ESM flags that on the
running instance (`secrets_unconfirmed`).

**The compatibility mode puts them on the command line.** It is for server
builds that cannot read the file, has to be chosen per instance
(`esm create <name> --secret-delivery command_line`, or `secret_delivery` in
the API) and is **unsafe on a shared box**: every account on the machine can
read the command line of a running process, and the server writes it into
its own log. ESM says UNSAFE wherever such an instance shows (`create`,
`list`, `status`, `doctor`, the API) and gives the read role none of its
server output and no failure details. Operators and admins still see them.
(The secrets file is written in this mode too, as ESM's record of what the
running server was started with; the server is not told about it.) The
mode does not make the passwords safe. It is there only until the game
server reads its passwords from the per-instance file, and is removed then.

More in [docs/DESIGN.md](docs/DESIGN.md#passwords).

## Safety rules

- The API listens on `127.0.0.1:8740` by default. ESM refuses to listen on
  any other address unless `tls_cert` and `tls_key` are set. To reach it from
  elsewhere, put a reverse proxy with TLS in front, or set up TLS directly.
- Passwords of ESM users are stored as PBKDF2 hashes and API tokens as
  SHA-256 hashes. Server passwords live in a private file per instance, are
  handed to the server in a private file, and are never shown by the API or
  written to ESM's logs. ESM never writes a server command line anywhere.
- Logins are rate limited, and login and setup take only JSON from the same
  origin. Browser sessions use an HttpOnly, SameSite=Strict cookie that
  lasts 12 hours, plus a separate CSRF token on every change. Behind a TLS
  reverse proxy, set `behind_tls_proxy` so the cookie is also Secure.
  Tokens are never accepted in URLs.
- Requests and replies are bounded: headers 64 KiB, bodies 1 MiB, slow
  clients are cut off, a client that stops reading an event stream is
  dropped without holding anything up, and log tails are capped.
- Every change, and every refused request, is written to an append-only
  audit log.
- Builds are checked against their manifest (SHA-256 of every file) before
  they are installed, archives are unpacked with path checks, downloads must
  be https with a known SHA-256, and installed files are read-only.
- ESM only ever signals or stops processes it started itself (it checks the
  process start time and executable before touching a pid).
- ESM never changes the firewall. It tells you which ports each instance
  uses; opening them is up to you.

## Documentation

- [docs/DESIGN.md](docs/DESIGN.md): how ESM works and why.
- [docs/openapi.yaml](docs/openapi.yaml): the HTTP API.

## Development

```sh
gofmt -l .
go vet ./...
go test ./...
```

The tests build a fake game server (`cmd/esm-fakeserver`) and drive whole
services through the API and the command line. Tests that start systemd
user units run only when asked: `ESM_TEST_SYSTEMD=1 go test ./...` on Linux.

Tests listen on 127.0.0.1 only, so a desktop firewall never asks about a
test binary. The fake server binds loopback only; in a test binary ESM's
own listener and port checks refuse any other address (and the run fails
naming it); the tests check the sockets of the fake servers and of the test
process (Linux and Windows); and a test fails on any Go file that binds or
names all interfaces outside a short allowed list. Create test instances
with `bind_ip` 127.0.0.1 and run `esm doctor --bind 127.0.0.1` in tests.

## Contributing

Issues and pull requests are welcome. Please keep changes small, add tests,
and keep the standard-library-only rule. Anything about the real game server
belongs in `internal/gameserver` (and in the fake server, so the tests
follow it).

## License

MIT, see [LICENSE](LICENSE).
