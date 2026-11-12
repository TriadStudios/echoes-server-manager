# ESM design

This document describes how ESM (Echoes Server Manager) works and why. The
HTTP API is specified in [openapi.yaml](openapi.yaml).

## What ESM is, and what it is not

ESM is a manager for Echoes of Durinn dedicated servers on **one machine**.
It installs server builds, runs any number of server instances, restarts
them when they crash, updates them with automatic rollback, backs up their
worlds, and shows their state and logs. One service owns one folder (the
root) and everything in it.

ESM is not:

- a fleet manager: it does not coordinate several machines (several ESMs can
  sit behind one front end later);
- a firewall or network tool: it never opens ports or edits firewall rules;
- a game server: it only starts the real server program and watches it;
- a public web site: it listens on loopback by default and expects a TLS
  reverse proxy, or its own TLS, before it is reachable from elsewhere.

## Words

| Word     | Meaning |
|----------|---------|
| box      | The machine ESM runs on. |
| root     | The folder ESM owns (`--root`, `$ESM_ROOT`, default `~/durinn`). |
| build    | One version of the game server files, installed read-only under `builds/<version>/`. |
| instance | One configured game server: settings, data folder, backups. It runs one build at a time. |
| cluster  | A label that groups instances (stored and listed; no behaviour yet). |
| channel  | `live` or `test`. Every build and every instance has one, and an instance only runs builds of its channel. |

## Shape

- **One binary.** `esm serve` is the service. Every other `esm` command is a
  thin client of the service's HTTP API.
- **One API.** Everything the service can do is an endpoint under
  `/api/v1`. There are no private paths for the command line.
- **Three front ends.** The command line (now), the web page (later), and
  your own scripts with API tokens. The command line reaches the service
  over the unix socket `<root>/esm.sock` by default, or any ESM with `--url`
  and a token.
- **Standard library only.** No third-party Go modules.
- **Plain files.** State is JSON files under the root, written atomically
  (temporary file, sync, rename). Every file access goes through an
  `os.Root` handle on the root, so a path can never resolve outside it.

The web page will be built with [Den](https://github.com/TriadStudios/Den)
and compiled into the binary. The service already serves an embedded static
folder at `/`, on the same origin as the API, with a placeholder page; any
path that is not a file returns `index.html` so the page can route on the
client side. Unknown `/api/` paths return the JSON 404 error instead.

## Folder layout

```
<root>/
  esm.json                     config
  esm.sock                     unix socket (only while the service runs)
  audit.log                    append-only audit log, one JSON object per line
  secrets/                     0700
    setup.json                 hash and expiry of the one-time setup code
    users.json                 users with PBKDF2 password hashes
    tokens.json                API tokens (SHA-256 only)
  builds/
    <version>/                 one installed build, read-only
      manifest.json
      ...
    .info/<version>.json       install time, file count, size
  instances/
    <name>/
      instance.json            settings
      secrets.json             server and admin passwords (0600)
      data/                    the server's data folder (saves, logs)
      console.log              the server's stdout and stderr
      run/
        pid.json               process supervisor record
        state.json             last known state
        server-secrets.json    what the running server was started with
                               (0600; only while a server runs)
  backups/
    <name>/<id>/               save files and meta.json
  tmp/                         install work folders
```

## Configuration

`<root>/esm.json` is written with defaults on first start. Unknown fields
are refused, so typos are reported.

| Field                  | Default           | Meaning |
|------------------------|-------------------|---------|
| `listen`               | `127.0.0.1:8740`  | API address. Anything but loopback needs TLS. |
| `socket`               | `<root>/esm.sock` | Unix socket path. |
| `tls_cert`, `tls_key`  | (none)            | TLS certificate and key files. |
| `behind_tls_proxy`     | `false`           | Browsers reach ESM only through a TLS reverse proxy: the session cookie is marked Secure. |
| `game_ports`           | `7777-7786`       | Range for game ports. |
| `query_ports`          | `27015-27024`     | Range for query ports. |
| `supervisor`           | `auto`            | `auto`, `systemd` or `process`. |
| `backoff`              | `5s`, `5m`, `10m`, `5` | Crash restarts: `base`, `max`, `window`, `max_crashes`. |
| `stop_timeout`         | `30s`             | Polite stop wait before a forced stop. |
| `ready_timeout`        | `120s`            | How long a start may take to become ready. |
| `keep_previous_builds` | `2`               | Previous builds per instance that pruning keeps. |
| `backup_save_wait`     | `2m`              | How long a backup of a running server waits for a world save. |
| `log_level`            | `info`            | `debug`, `info`, `warn` or `error`. |

## Instances

| Field              | Meaning |
|--------------------|---------|
| `name`             | `^[a-z][a-z0-9-]{0,31}$`, fixed. |
| `display_name`     | Name shown to players (the server name). |
| `cluster`          | Optional label. |
| `channel`          | `live` or `test`; must match the build. |
| `build`            | Current build. Changed only by an update. |
| `map`              | Map to load; empty means the server's default. |
| `world`            | World (save) name; defaults to the instance name. |
| `ports`            | `game` and `query`. Picked from the ranges when not given. |
| `bind_ip`          | Address to bind; empty means all interfaces. |
| `max_players`      | 1 to 200. |
| `rules`            | Server settings; each becomes `-Key=Value` on the command line. |
| `listed`           | Whether the server should be publicly listed (default: true for live, false for test). Stored; not yet passed to the server. |
| `autostart`        | Start when ESM starts. |
| `restart_on_crash` | Restart after a crash, with back-off (default true). |
| `schedule.restart_at` | Daily restart times, `HH:MM` local time. |
| `backup.every`, `backup.keep` | Scheduled backups (0 is off) and how many backups to keep (default 10). |
| `limits`           | `memory_max` (like `8G`) and `cpu_quota_percent`; enforced by systemd only. |
| `extra_args`       | Extra server arguments, each starting with `-`. |
| `secret_delivery`  | How the server gets its passwords: `file` (default) or `command_line` (unsafe on a shared box). See [Passwords](#passwords). |
| `previous_builds`  | Builds this instance ran before, newest first. |

Rules and extra arguments may not repeat a switch ESM sets itself (ports,
names, passwords, the secrets file, the data folder and so on), and may not
name a secret (see [Passwords](#passwords)). Values may not contain control
characters. The command line is always built as an argument list, never as
a shell string, and ESM never writes it anywhere: not into its log, an
error, the state or an audit entry.

Ports: a new instance gets the first game/query port pair from the ranges
that no other instance uses and that can be bound right now (UDP and TCP).
Ports given explicitly are checked the same way, and all ports are checked
again before every start.

## Passwords

An instance has two secrets: the server password (needed to join) and the
admin password. They are set with `PUT /api/v1/instances/{name}/secrets`
and nowhere else, stored in `secrets.json` (0600) and never returned: the
API only says whether each is set. A change reaches the server with its
next start.

### In a file (the default)

Before each start ESM writes the passwords to
`instances/<name>/run/server-secrets.json` and gives the server one switch
that names the file, `-SecretsFile=<absolute path>`. The file is one JSON
object on one line, UTF-8:

```json
{"server_password":"...","admin_password":"..."}
```

A password that is not set is left out; with none set the file is `{}`.
The file has mode 0600 inside folders with mode 0700, is written afresh at
every start, and is removed once the server has stopped: after a stop, a
crash, a failed start, or at the next ESM start that finds the server gone.

So with this way no password is on a command line, and nothing the server
repeats from its command line holds one. There is nothing to scrub: the
process list, the server's log and output, and everything ESM says have no
password in them to begin with.

**This needs the game server to read the file, and the real server does
not do that yet.** A per-instance config file that the server reads its
passwords from is a planned item on the game side, and it is the one
dependency that removes the compatibility mode below. The switch name
and the file format above are ESM's proposal and are marked UNCONFIRMED in
the adapter. A server build that does not read the file does not get the
passwords and runs *without* them. ESM cannot prevent that, but it shows
it: the server's status file says which passwords the server took
(`server_password_set`, `admin_password_set`, never the values), and an
instance whose running server does not confirm the passwords that are set
carries the warning `secrets_unconfirmed`. Until the game reads the file,
an instance that needs a password on a real server has to use the
compatibility mode below.

### On the command line (compatibility mode)

`secret_delivery: "command_line"` passes the passwords as
`-ServerPassword=` and `-AdminPassword=` switches instead. It is off unless
an admin sets it for an instance in so many words, at create or with a
change. (The secrets file is written in this mode too. The server is not
told about it; it is ESM's record of what the running server was started
with, and goes when the server has stopped.)

**It is unsafe on a shared box.** Every account on the machine can read
the command line of a running process, and the server writes its command
line into its own log and output, which stay on disk. ESM says so wherever
such an instance shows: the instance carries the warning
`secrets_on_command_line`, `esm create`, `list` and `status` print UNSAFE,
`doctor` and the box facts name the instances, and the service log warns
at every start.

What ESM does about it is not masking. While an instance is set to
`command_line`:

- the `read` role gets none of its server output: `GET .../logs` answers
  403 with the code `hidden_unsafe_secrets`, for both sources, and its
  `log` events are not sent to that role;
- the `read` role gets a fixed notice in place of every state reason of the
  instance (a reason can repeat what a server or a tool said): in the
  instance, its status, the list and its `state` events;
- `operator` and `admin` see the output and the reasons. The output filter
  (below) masks the passwords it knows in the output and in start errors,
  without any promise.

This is decided when an event is published and stays with the event: a
client that asks later for remembered events gets the same.

Going back to `file` is refused while the server runs (409
`instance_running`). With the server stopped, the change first sets the
output files aside, because they hold the command line: `console.log` and
the server log get the suffix `.unsafe`, replacing what was set aside
before. It also replaces the state reason. ESM never shows the files set
aside; they go when the instance is deleted. From then on the read role
sees the instance's output again, and there is no password in it.

The compatibility mode does not make the passwords safe. Whoever can list
processes on the box can read them while the server runs, and whoever can
read the instance's files can read them in its logs. The mode is there
only until the game server reads its passwords from the per-instance
secrets file, and is removed then.

### No side doors

Rules and extra arguments are shown to every role and go on the server
command line, so no secret belongs there. A rule key, or the switch name
of an extra argument, is refused with 422 `looks_like_secret` and a pointer
to `PUT .../secrets` when, compared without case and without `_`, `-` and
`.`, it

- contains `password`, `passwd`, `passphrase`, `pwd`, `secret`, `token`,
  `apikey`, `privatekey` or `credential`, or
- ends with `key`.

The switch name of an extra argument is what follows its dashes, up to the
first character that is not a letter, a digit, `_`, `.` or `-`; everything
before the `=` is checked for the words as well. So `JoinPassword`,
`Rcon_Pwd`, `ApiKey`, `AdminKey`, `AuthToken` and
`-Section:Game:JoinPassword=x` are refused, while `Difficulty`, `Keyboard`,
`KeyBindings` and `Compass` pass. The rule would rather refuse too much:
`Hotkey` ends with `key` as well. Which switches of the real server take
secrets is not known yet; a name that is refused wrongly gets an exception
in the adapter. A refusal never repeats a value: an extra argument is named
by its position, a rule by its key.

### The output filter

ESM still filters what a server wrote (log tails and `log` events) and
what a supervisor's tool printed (start errors). It masks the passwords now
set and the ones the running server was started with, in the forms they
take on the way (as they are, with systemd's doubled `$`, JSON and Go
quoting); the value of anything that looks like a password switch
(`-...Password=`, up to the next switch) or pair (`password=`); and API
tokens. A command line that a supervisor's tool repeats whole is cut out of
its message.

This is a second line of defence, for a server that prints a password
itself. It is not the guarantee, and it has limits: a value shorter than 4
characters is not masked as text; a value the server prints in another
shape (in upper case, split up) is not recognised; and a mask shows where a
value was, so a password that is an ordinary word of the output can be
guessed from the masks. What ESM says itself (state reasons, errors) holds
no password and is never matched against one.

## Life cycle

States: `stopped`, `starting`, `running`, `stopping`, `crashed`, `failed`,
`updating`. The state, a reason and timestamps are kept in memory, written
to `run/state.json` and published as events.

**Start.** ESM checks the build and the ports, writes the secrets file,
builds the command line, and asks the supervisor to start the server. The
instance is `starting` until it is ready. A start returns once the server is
launched; with `wait=true` (the command line always waits) it returns once
the server is running or has failed.

**Ready.** The first of these sources that confirms wins, checked in this
order: (1) the server's status file, written by the process that is running
now, says `ready`; (2) the server log has the ready line; (3) the query port
answers. If none confirms within `ready_timeout`, ESM stops the server and
the instance is `failed`.

The ready line, like the world-saved line that backups wait for, is a fixed
marker of the adapter (`Server is ready`, `World saved`) that must stand at
the very start of a log line, after nothing but the server's own time prefix
(`[2026-01-02T03:04:05.678Z] `). The same words anywhere else in a line do
not count. A server repeats text that users chose (its command line with
the server name, rule values and extra arguments; names; chat), but none of
it can hold a line break, so none of it can stand at the start of a line.
ESM reads the log in whole lines, as the server wrote them and before
anything is masked or cut; the rest of an unfinished or over-long line is
dropped, never taken for a line of its own.

**Stop.** A polite stop (SIGTERM on Linux; a stop request file on Windows),
then a forced stop after `stop_timeout`. Restart is stop then start.

**Crash.** A server that goes away without a stop request has crashed. With
`restart_on_crash`, ESM restarts it after `base * 2^(n-1)` (capped at
`max`), where n counts crashes inside `window`. At `max_crashes` crashes
inside the window ESM gives up: the instance is `failed` with `gave_up`, and
nothing restarts it until someone starts it by hand (which clears the
count).

**Supervisors.**

- *systemd* (Linux, preferred): each server is a transient user unit
  `esm-<name>` started with `systemd-run --user`, which applies memory and
  CPU limits. ESM only touches units named for instances in its root and
  marked with its root in the description. The box owner turns on lingering
  once (`loginctl enable-linger <account>`) so units survive logouts.
- *process* (Linux and Windows): a detached child process. ESM records its
  pid, start time and executable, and checks all three before it signals or
  kills anything, so it never touches a process that reused the pid. Limits
  are not enforced in this mode.
- *auto* picks systemd when `systemctl --user` works, otherwise process.

**ESM restarts.** Stopping the service leaves game servers running. When it
starts again it finds them (pid records or units) and watches them again.
A live server ESM started earlier is `running` at once, with ready source
`adopted`. ESM looks for its ready signals again only to show which one
confirms; if none does within `ready_timeout`, the reason says so and the
server keeps running. An adopted server is never stopped for that. Then ESM
starts every `autostart` instance that is not running.

On a box shared with other services, ESM runs as its own non-root account.

## Builds

A build folder has a `manifest.json`:

```json
{
  "version": "1.2.0",
  "commit": "abc1234",
  "channel": "live",
  "executables": { "linux": "server/linux/start", "windows": "server/windows/start.exe" },
  "files": { "server/linux/start": "<sha256>", "...": "..." }
}
```

Every file except the manifest must be listed with its SHA-256, and the
executable for the box's operating system must be listed. A build is
installed from:

- a local folder (copied; links and special files refused);
- a local `.zip`, `.tar.gz` or `.tgz` archive (unpacked with path checks:
  no absolute paths, no `..`, no links, no devices, no duplicate names,
  size and file-count limits);
- an `https` URL to such an archive, with a required SHA-256 (plain http is
  refused; redirects must stay on https).

Everything is unpacked into `tmp/`, the manifest and every hash are checked,
and only then is the folder moved into `builds/<version>/`. Files are made
read-only. An existing version is never replaced.

**Update** switches an instance to another build of its channel: back up
the saves, stop the server if it runs, switch the build, start, and wait for
ready. If the new build does not become ready, ESM stops it, switches back
and starts the old build again; the answer says the update failed and was
rolled back. World data is not touched by a rollback.

**Pruning** removes every build that no instance uses as its current build
or as one of its newest `keep_previous_builds` previous builds. A single
build can be deleted when no instance uses it.

Later, builds will also come from Steam, with the app ids as configuration.

## Ports and the firewall

ESM never changes firewall rules. It shows the ports of every instance
(create, list, status, the API) and the configured ranges (doctor, box
status). Opening them, and only them, is up to the box owner.

## Watching

The service keeps an in-memory event bus. Every event has an increasing id,
a time, a type (`state`, `log`, `build`, `audit`), an optional instance and
JSON data. `GET /api/v1/events` streams them as Server-Sent Events; a client
that reconnects with `Last-Event-ID` receives what it missed while the
events are still in memory. New server log and console lines are published
as `log` events, which is how `esm logs -f` follows a server.

An event can be restricted when it is published: the `read` role then gets
a public form of it, or does not get it at all, both live and when it asks
for remembered events. That is how the output of an instance with passwords
on the command line is kept from that role (see [Passwords](#passwords)).

Publishing never waits for a subscriber. Each subscriber has a buffer
(1024 events for a stream); a subscriber whose buffer is full is dropped,
and the others go on getting every event. A stream also gives each write
to its client a deadline (a minute). So a client that stops reading, or
reads too slowly, can hold up neither a state change nor another client:
its stream ends, and it reconnects with `Last-Event-ID`.

## Backups

A backup copies the server's save files (the adapter's save glob) into
`backups/<name>/<UTC time>-<reason>/` with a `meta.json` (time, reason,
build, files, size). If the server runs, ESM first waits up to
`backup_save_wait` for its next world-saved log line (the fixed marker at
the start of a line, see [Life cycle](#life-cycle)), so the copy is a
complete save; if none comes, it copies anyway and marks the backup
`save_confirmed: false`.

Backups are made on demand, on a schedule (`backup.every`), before every
update (`pre-update`) and before every restore (`pre-restore`). After each
backup the oldest ones beyond `backup.keep` are removed; the backup being
restored is always kept.

**Restore** stops a running server, backs up the current saves, puts the
backup's save files in place, and starts the server again if it was
running.

Scheduled restarts (`schedule.restart_at`) restart running instances at
the given local times; a time that passes while an instance is stopped is
skipped.

## API overview

- JSON in and out, snake_case field names, no polymorphic fields, no
  integers above 2^53.
- Reads are GET. Every change is POST, PUT, PATCH or DELETE with a JSON body
  (an empty body counts as `{}`), at most 1 MiB.
- One error shape everywhere: `{"error": {"code": "...", "message": "..."}}`
  with 400, 401, 403, 404, 405, 408, 409, 413, 415, 422, 429 or 500. (The
  HTTP server itself answers malformed requests and request headers over
  the cap with a plain 400 or 431, without that shape or ESM's headers.)
- **Limits.** Request headers: at most 64 KiB, within 10 seconds. A request
  body: at most 1 MiB (413), within 30 seconds (408). A reply: the client
  has a minute to read it, counted from its first byte; in an event stream,
  a minute for each event. An idle connection is closed after 2 minutes.
  A log tail: at most 5000 lines (more is refused with 422), each cut to
  4096 bytes, and at most 4 MiB of lines in all (counted as JSON strings,
  the oldest left out first). The HTTP servers have no server-wide read or
  write timeout, because one would also end the calls that rightly take
  long (a start that waits for ready, a build download, an event stream);
  the API sets those deadlines per request instead.
- Every change writes an audit entry (who, what, target, success, error).
  Refused calls are audited too. Audit entries and logs never contain
  passwords or tokens.
- An instance carries `warnings`, a list of codes: `secrets_on_command_line`,
  `secrets_unconfirmed` (both in [Passwords](#passwords)) and
  `limits_not_enforced` (it has resource limits and the process supervisor
  does not apply them).

See [openapi.yaml](openapi.yaml) for every endpoint.

## Logins and exposure

- **Loopback by default.** The API listens on `127.0.0.1:8740`. ESM refuses
  to start on any other address unless `tls_cert` and `tls_key` are set. A
  reverse proxy with TLS in front of a loopback ESM is the expected way to
  reach it from elsewhere.
- **Unix socket.** `<root>/esm.sock` (mode 0600) acts as admin; it is how
  the command line on the box works. A stale socket file is removed at start;
  a socket that still answers means another ESM already serves the root.
- **Setup code.** While no admin exists, ESM makes a one-time setup code
  (24 characters, valid for 24 hours), prints it once and keeps only its
  hash. `POST /api/v1/setup` with the code creates the first admin.
- **Users and sessions.** Passwords need at least 12 characters and are
  stored as PBKDF2-SHA256 with 600,000 iterations. Login and setup take a
  JSON body only (`Content-Type: application/json`, else 415), which a form
  on another site cannot send, and refuse a request whose `Origin` header
  names another origin (403). A request without `Origin` is accepted, so
  the command line, curl and scripts work; browsers send `Origin` with
  every POST. A login sets the `esm_session` cookie: HttpOnly (scripts
  cannot read it), SameSite=Strict, Path=/, Max-Age 43200, and Secure when
  ESM has TLS or `behind_tls_proxy` is set. A session lasts 12 hours from
  login, however much it is used, and ends at logout, which clears the
  cookie (Max-Age=0); sessions live in memory, so a restart of ESM ends
  them all. The session id is only ever in the cookie, never in a reply
  body, a log or the audit log. Every non-GET call with the cookie must
  send `X-CSRF-Token` with the session's CSRF token, a random value of its
  own (given at login and by `GET /api/v1/session`), and an `Origin` header,
  when present, must match the host. Failed logins are limited to 5 per 5
  minutes per client address and per user name.
- **Behind a reverse proxy.** The proxy terminates TLS and forwards to ESM
  on loopback. It must pass the `Host` header through unchanged (the
  `Origin` checks compare against it), and `behind_tls_proxy` should be set
  so the session cookie is Secure. Do not set it without such a proxy: a
  Secure cookie is never sent over plain http, so browser logins would
  stop working. ESM takes the client address from the connection, never
  from forwarding headers, so behind a proxy the login rate limit per
  client address counts all clients together (the limit per user name
  still applies).
- **Tokens and roles.** API tokens (`esm_` + 43 characters) are shown once
  and stored as SHA-256. Roles: `read` sees everything except tokens, the
  audit log, and the server output and state reasons of instances that pass
  their passwords on the command line; `operator` sees those too and also
  starts, stops, restarts, updates, backs up, restores and installs builds;
  `admin` does everything else (create, change, delete, secrets, tokens,
  pruning). Tokens are never accepted in URLs.
- **Page headers.** Every reply ESM writes, of the page and of the API,
  errors and the router's own redirects included, carries a strict
  Content-Security-Policy (no inline scripts or styles, nothing from other
  origins, `frame-ancestors 'none'`), `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: no-referrer` and `X-Frame-Options: DENY`.
- **Page files.** The page is served from files compiled into the binary,
  never from the disk. A folder is never listed. The router redirects a
  path with plain `.` or `..` segments or doubled slashes to its clean
  form; after that, a request path that has a `.` or `..` segment (written
  `%2e%2e`, say), a backslash (also `%5c`), a colon (a drive letter such as
  `/C:/Windows/win.ini`, or a Windows stream name), a segment that starts
  with a dot, or a control character gets 404, whatever files there are.

## Command line

```
esm [--root DIR] [--json] [--url URL --token TOKEN] <command> [arguments]

serve | doctor | version
list | status [name]
create <name> [--build V] [--channel C] [--map M] [--world W] [--display-name N]
       [--cluster C] [--game-port P] [--query-port P] [--bind IP] [--max-players N]
       [--set Key=Value ...] [--arg -X=Y ...] [--secret-delivery file|command_line]
start | stop | restart <name>
logs <name> [-n N] [-f] [--console]
build install <folder|archive|https-url> [--sha256 HEX] | build list | build prune
update <name> --to <version>
backup <name>
restore <name> <backup-id>
token create --role read|operator|admin [--name NAME]
```

Every command prints one result line, or one JSON object with `--json`, and
exits 0 or 1. Errors go to stderr (with `--json`, to stdout as an error
object). `logs` prints the log lines themselves. `create`, `list` and
`status` print the warnings of an instance in words; one that passes its
passwords on the command line is marked UNSAFE. `doctor` works without a
running service: it checks the root, the config, the listen rule, free
ports, disk space, the supervisor (with systemd lingering), the socket and
which instances pass passwords on the command line, changes nothing, and
adds the service's own view when a service answers.

Passwords are set through the API (`PUT /api/v1/instances/{name}/secrets`).
The command line has no verb for them yet: a password given as an argument
would end up in the shell history and in the process list.

`serve` logs JSON to stderr. On SIGINT or SIGTERM it stops accepting
requests, ends event streams, stops watching and scheduling, flushes the
audit log, removes the socket, and leaves game servers running.

## Safety rules

1. Never run as root.
2. Never write outside the root.
3. Never touch a process or unit ESM did not start.
4. Never put secrets in responses, logs, audit entries or URLs, and never
   on a command line unless an instance is set to that in so many words.
5. Never listen beyond loopback without TLS.
6. Never install a build whose files do not match its manifest.
7. Never change the firewall.
8. Stopping ESM never stops game servers.

## What is still unknown about the real server

Everything ESM assumes about the Echoes of Durinn dedicated server is in
[`internal/gameserver`](../internal/gameserver/gameserver.go). None of it is
confirmed on a real host yet:

- the switch names on the command line, the position of the map argument,
  and the default map;
- the switch that points the server at its data folder;
- the secrets file: the real server does not read one yet, so the switch
  that names it and the format of the file are ESM's proposal for the game
  side, and until the game has it a real server gets passwords only in the
  compatibility mode;
- the switches that carry the passwords in the compatibility mode, and
  which other switches take secrets;
- the log file location, the "ready" and "world saved" log lines and the
  time prefix of a log line (a real server must write those two lines so
  that no player-chosen text can stand in front of them or imitate them);
- whether the server writes a status file at all (ESM then relies on the
  log line and the query port), and whether it says there which passwords
  it took;
- the query protocol used to probe the query port;
- where save files live (the backup glob);
- how to ask the server politely to stop on Windows;
- how a server is marked as publicly listed.

The fake server used by the tests follows exactly these assumptions; when a
real host proves one wrong, it changes in the adapter and the fake, and
nowhere else.

## Phases

- **Phase 0 (this):** the service, the command line, the API, builds,
  instances, backups, and tests against the fake server.
- **Phase 1:** the web page (built with Den) and the real server facts
  confirmed on a real host.
- **Later:** player controls, Steam installs, Windows hosts (planned for
  the first public release).

## Open questions

- Which of the adapter's assumptions hold for the real server, and what the
  real ready and save signals are.
- Whether the server can save on request, so backups need not wait for the
  next periodic save.
- When the game server reads its passwords from the secrets file. That is
  what lets the command-line compatibility mode be removed.
- Whether a server that does not confirm its passwords should be refused
  instead of only flagged.
- How public listing is switched on and off.
- How Steam app ids and branches map to ESM channels.
- Whether several boxes should be managed from one page, and how.
