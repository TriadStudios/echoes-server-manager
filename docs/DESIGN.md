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
- **Three front ends.** The command line, the web page, and your own
  scripts with API tokens. The command line reaches the service
  over the unix socket `<root>/esm.sock` by default, or any ESM with `--url`
  and a token.
- **Standard library only.** No third-party Go modules.
- **Plain files.** State is JSON files under the root, written atomically
  (temporary file, sync, rename). Every file ESM writes in the root, and
  most it reads, goes through an `os.Root` handle on the root, so such a
  path can never resolve outside it; local build sources are read through
  one on the import folder. Plain paths are used for the socket and for
  creating the import folder, and for a few reads: the read-only checks of
  `esm doctor` and the config read without a running service, a server's
  status file, the save files a backup copies (each checked to be a
  regular file), and the process checks under `/proc`.

The web page is plain HTML made by the service on the same origin as the
API (see [The web page](#the-web-page)); it may be rebuilt with
[Den](https://github.com/TriadStudios/Den) later. Unknown `/api/` paths
return the JSON 404 error.

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
    steam.json                 branch passwords of the Steam sources (0600)
  steam/linux64/               the 64-bit Steam client libraries (put there
                               by the box owner; see steam_runtime_dir)
  builds/
    <version>/                 one installed build, read-only, shared by
                               every instance that runs it
      manifest.json
      .../steam_appid.txt      next to the server binary, when
                               steam_app_id is set
      ...
    .info/<version>.json       install time, file count, size
  instances/
    <name>/
      instance.json            settings
      secrets.json             server and admin passwords (0600)
      data/                    the server's own folder (-UserDir)
        Saved/SaveGames/...    its world saves (what backups copy)
      logs/
        server.log             the server log (-abslog), new at every start
        server.previous.log    the log of the start before
      console.log              the server's stdout and stderr
      run/
        pid.json               process supervisor record
        state.json             last known state
        server-secrets.json    what the running server was started with
                               (0600; only while a server runs)
  backups/
    <name>/<id>/               save files and meta.json
  steam-cache/
    <source>/app/              SteamCMD's install folder of the source's app
                               (kept, so downloads are deltas)
    <source>/home/             the home folder SteamCMD runs with
    <source>.state.json        build id last seen, when, last check, last error
    <source>.log               SteamCMD's output of the last run (masked)
  tmp/                         install work folders
```

## Configuration

`<root>/esm.json` is written with defaults on first start. Unknown fields
are refused, so typos are reported.

| Field                  | Default           | Meaning |
|------------------------|-------------------|---------|
| `listen`               | `127.0.0.1:8740`  | API address. Anything but loopback needs TLS. |
| `socket`               | `<root>/esm.sock` | Unix socket path, at most 107 bytes on Linux (103 on macOS), in a folder only the ESM account can write to. |
| `tls_cert`, `tls_key`  | (none)            | TLS certificate and key files. |
| `behind_tls_proxy`     | `false`           | Browsers reach ESM only through a TLS reverse proxy: the session cookie is marked Secure. |
| `public_hosts`         | (none)            | Further host names (`host` for any port, or `host:port`) the API and the page answer to; see [Logins and exposure](#logins-and-exposure). |
| `build_import_dir`     | `<root>/imports`  | The only folder local build sources are read from (created 0700 at start; a relative path is taken from the root). |
| `game_ports`           | `7777-7786`       | Range for game ports. |
| `query_ports`          | `27015-27024`     | Range for query ports. |
| `supervisor`           | `auto`            | `auto`, `systemd` or `process`. |
| `backoff`              | `5s`, `5m`, `10m`, `5` | Crash restarts: `base`, `max`, `window`, `max_crashes`. |
| `stop_timeout`         | `120s`            | How long a stop waits for the server to exit after its one stop signal. ESM never sends a second one (see [Life cycle](#life-cycle)). |
| `ready_timeout`        | `120s`            | How long a start may take to become ready. |
| `keep_previous_builds` | `2`               | Previous builds per instance that pruning keeps. |
| `backup_save_wait`     | `2m`              | How long a backup of a running server waits for a world save. |
| `log_level`            | `info`            | `debug`, `info`, `warn` or `error`. |
| `steam_app_id`         | (none)            | The game's Steam app id (digits). Written to `steam_appid.txt` next to the server binary of every build installed from then on. No default; `esm doctor` warns while it is unset. |
| `steam_runtime_dir`    | `<root>/steam/linux64` | Folder with the 64-bit `steamclient.so` and `libsteamwebrtc.so`; put first on every server's `LD_LIBRARY_PATH`. A relative path is taken from the root. |
| `update_check`         | `true`            | Let the service look up the newest ESM release (at most every 6 hours, only while someone looks at the box). It never updates anything by itself. |
| `steamcmd`             | `<root>/steamcmd/steamcmd.sh` | SteamCMD (`steamcmd.exe` on Windows). A relative path is taken from the root. |
| `steam_sources`        | (none)            | Steam apps builds come from: `name`, `app_id`, `branch` (empty for the default branch), `executable` (per system, a path inside the app's files), `default_map`, `channel`, `check_every` (default `15m`, `"0"` off, else at least `1m`). See [Builds from Steam](#builds-from-steam). |
| `keep_steam_builds`    | `3`               | Builds of each Steam source kept, the newest by build id. After each install from a source, its older builds that no instance uses are removed. At least `1`. |

## Instances

| Field              | Meaning |
|--------------------|---------|
| `name`             | `^[a-z][a-z0-9-]{0,31}$`, fixed. |
| `display_name`     | Name shown to players (the server name). |
| `cluster`          | Optional label. |
| `channel`          | `live` or `test`; must match the build. |
| `build`            | Current build. Changed only by an update. |
| `map`              | Map to load; empty means the build's `default_map`. A start without either is refused (409 `no_map`). |
| `world`            | World (save) name; defaults to the instance name. |
| `ports`            | `game` and `query`. Picked from the ranges when not given. |
| `bind_ip`          | Where ESM checks that the ports are free and probes the query port; empty means all interfaces. It is **not** given to the server (no `-MULTIHOME`), which listens on every address: the box's firewall decides where its query port is reachable. |
| `max_players`      | 1 to 200. |
| `rules`            | Server settings; each becomes `-Key=Value` on the command line. |
| `listed`           | Whether the server shows in the in-game server list (default: true for live, false for test); passed as `-ListInBrowser=True` or `False`. |
| `autostart`        | Start when ESM starts. |
| `restart_on_crash` | Restart after a crash, with back-off (default true). |
| `schedule.restart_at` | Daily restart times, `HH:MM` local time. |
| `backup.every`, `backup.keep` | Scheduled backups (0 is off) and how many backups to keep (default 10). |
| `limits`           | `memory_max` (like `8G`) and `cpu_quota_percent`; enforced by systemd only. |
| `extra_args`       | Extra server arguments, each starting with `-`. |
| `secret_delivery`  | How the server gets its passwords: `file` (default) or `command_line` (unsafe on a shared box). See [Passwords](#passwords). |
| `previous_builds`  | Builds this instance ran before, newest first. |

Rules and extra arguments may not repeat a switch ESM sets itself (ports,
names, passwords, the secrets file, `-UserDir`, `-abslog` and so on), may
not be `-MULTIHOME` (see [The real server](#the-real-server)), and may not
name a secret (see [Passwords](#passwords)). Values may not contain control
characters. The command line is always built as an argument list, never as
a shell string, and ESM never writes it anywhere: not into its log, an
error, the state or an audit entry.

The server joins its arguments back into one command line and searches it
for switches, so separate arguments are no boundary. Every setting that
goes onto the command line (the display name, world, map, rule keys and
values, extra arguments) is refused with 422 `unsafe_argument` when it
holds a double quote, the name `MULTIHOME` in any case, or white space
followed by `-` and more (a lone ` - ` is fine): any of those could add a
switch of its own. The answer names the setting, never its value.

The command line, in this order: the map, `-unattended`, `-Port=`,
`-QueryPort=`, `-ServerName=` (the display name, or the instance name),
`-WorldName=`, `-MaxPlayers=`, `-ListInBrowser=`, `-UserDir=<instance>/data`,
`-abslog=<instance>/logs/server.log`, then the rules (`-Key=Value`, sorted by
key), the password switch or switches, and the extra arguments.

Ports: a new instance gets the first game/query port pair from the ranges
that no other instance uses and that can be bound right now (UDP and TCP).
Ports given explicitly are checked the same way, and all ports are checked
again before every start. The game port is reserved and given to the server
although the server opens no socket on it (players join through Steam's
relay); only the query port is reachable from outside.

## Passwords

An instance has two secrets: the server password (needed to join) and the
admin password. They are set with `PUT /api/v1/instances/{name}/secrets`
and nowhere else, stored in `secrets.json` (0600) and never returned: the
API only says whether each is set. A change reaches the server with its
next start.

A password has at least 8 characters (422 `password_too_short`; a choice
made for now, it can only be raised later). A password may not occur, in
any case, in a setting of its instance that every role sees and the server
may print: the name, display name, world, map, cluster, a rule key or value,
or an extra argument. Setting such a password, or changing a setting so
that it holds one, is refused with 422 `password_in_settings`, which names
the password (server or admin) and the setting, never the value. The output
filter (below) masks passwords, so such a setting would show as a mask and
give the password away.

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
to `PUT .../secrets` when it

- has any character that is not plain ASCII (a letter from another script
  can stand in for one of the words below), or, compared without case and
  without `_`, `-` and `.`,
- contains `password`, `passwd`, `passphrase`, `pwd`, `secret`, `token`,
  `apikey`, `privatekey` or `credential`,
- ends with `key` or `pw`, or
- has a word that starts with `pass` or is `pw`. Words are split at
  anything but letters and digits, at a change from small to capital
  letters, between letters and digits, and before the last capital of a
  run of capitals followed by a small letter.

The switch name of an extra argument is what follows its dashes, up to
`=`, `:` or white space; every run of letters, digits, `_`, `.` and `-`
before the `=` is checked for the words as well. So `JoinPassword`,
`Rcon_Pwd`, `ApiKey`, `AdminKey`, `AuthToken`, `ServerPass`, `SERVER_PASS`,
`Passw0rd`, `RconPw`, `AdminPW` and `-Section:Game:JoinPassword=x` are
refused, while `Difficulty`, `Keyboard`, `KeyBindings`, `Compass`,
`BypassLimit` and `Pwned` pass. The rule would rather refuse too much:
`Hotkey` ends with `key`, and `Passive` (a word starting with `pass`) is
refused on purpose, since it cannot be told apart from a password switch by
its name. A name in one case without separators has no words, so
`serverpass` passes (`serverpassword` and `rconpw` do not). Which switches of the real server take
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
characters is not masked as text (passwords set now have at least 8); a value the server prints in another
shape (in upper case, split up) is not recognised; and a mask shows where a
value was, so a password that is an ordinary word of the output can be
guessed from the masks. What ESM says itself (state reasons, errors) holds
no password and is never matched against one.

## Life cycle

States: `stopped`, `starting`, `running`, `stopping`, `crashed`, `failed`,
`updating`. The state, a reason and timestamps are kept in memory, written
to `run/state.json` and published as events.

**Start.** ESM checks the build, the map and the ports, writes the secrets
file, moves the last server log to `logs/server.previous.log` (every start
begins a new log), builds the command line, and asks the supervisor to start
the server: with the build's top folder as its working folder and the Steam
runtime folder first on `LD_LIBRARY_PATH`. The instance is `starting` until
it is ready. A start returns once the server is launched; with `wait=true`
(the command line always waits) it returns once the server is running or has
failed.

**Ready.** The first of these sources that confirms wins, checked in this
order: (1) the server's status file, written by the process that is running
now, says `ready` (a real server writes none today); (2) the server log has
the ready line; (3) the query port answers ESM's probe. If none confirms
within `ready_timeout`, ESM stops the server and the instance is `failed`.

The probe speaks A2S_INFO, the public Source server query protocol, which
the real server answers on its query port: the request (`0xFFFFFFFF 'T'
"Source Engine Query\0"`), the challenge step (a `0x41` reply is answered
by sending the request again with its 4-byte challenge, at most twice), and
the `0x49` info reply, of which ESM reads the name, map, players and max
players. Every field is bounds-checked; a reply longer than one packet,
split over packets, cut short or of another type is no answer. The probe
asks the instance's `bind_ip`, or 127.0.0.1 when that is empty, and is
bounded by a short timeout (200 ms by default). It is also how a server
that ESM finds running at its start is checked again.

The ready line is the server saying it registered its dedicated session
(`[SESSION] Dedicated session '<name>' registered (map <map>, <n> slots)`):
from then on it is in the server list and can be joined. The Steam logon
line comes a moment earlier but is not taken: logged on without a session,
the server cannot be joined yet. Backups wait for a world-saved line: the
server wrote its save (`Wrote save ...`, at start, at every autosave and at
a stop) or confirmed its last write at a stop (`... Synchronous write after
the flush succeeded.`). The logon line names the server's Steam id, which
is new at every start; ESM shows it in the instance's `live` view while the
server runs and never stores it.

Each of these lines counts only when its words start the message, right
after the engine's prefix, which the line must have in full: the time and
frame in square brackets, exactly one log category, and at most one
verbosity word (`Fatal`, `Error`, `Warning`, `Display`, `Log`, `Verbose`,
`VeryVerbose`), as in `[2026.01.02-03.04.05:678][  0]LogExample: Display: `.
A chat line, which has the player's name or a tag between the category and
the text, never counts; neither do the same words anywhere else in a line.
The flush line counts only with its `[SHUTDOWN]` or `[EXIT]` tag. The
server id of a run is the first one its log names. A server repeats text that users chose (its command
line with the server name, rule values and extra arguments; names; chat),
but none of it can hold a line break, so none of it can stand at the start
of a line. ESM reads the log in whole lines, as the server wrote them and
before anything is masked or cut; the rest of an unfinished or over-long
line is dropped, never taken for a line of its own. The patterns live in
`internal/gameserver`, and nowhere else.

**Stop.** On Linux a stop is exactly **one SIGINT**: the server saves its
world and exits (exit code 130) within seconds. ESM then waits up to
`stop_timeout` (default 120 s). It never sends a second signal and never
kills the server: a second signal while the server shuts down may end it
before its save is written, and a kill certainly does. A server still
running when the wait ends is left running; the instance becomes `failed`
with a reason that says so, and the stop answers 422 `stop_timeout`. A
later stop of the same server waits again without sending anything (ESM
records the signal it sent, in `run/pid.json` or `run/stop-signal.json`, so
this holds across ESM restarts). Ending such a server by force is left to
the box owner, who knows it may lose the last minutes of the world. Restart,
update, restore, a ready timeout and the crash back-off all stop the same
way. On Windows (process mode, for development) ESM writes a stop request
file and kills the server after `stop_timeout`.

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
  marked with its root in the description. Every unit has
  `KillSignal=SIGINT`, `KillMode=mixed`, `SendSIGKILL=no` and
  `TimeoutStopSec=infinity`, and ESM stops a unit with `systemctl --user
  stop --no-block` (the main process gets the one SIGINT) and then waits
  `stop_timeout` itself. The unit is then deactivating, so a `systemctl
  --user stop` from outside ESM, before or after ESM's own, sends nothing
  more; with a stop timeout on the unit, systemd would send the kill signal
  again when it ran out, even with `SendSIGKILL=no`. (So an outside
  `systemctl --user stop` waits for as long as the server takes.) A stop by
  ESM while the unit is already being stopped sends nothing. The
  check, the record and the signal of a stop happen under one lock per
  instance, so stops at the same time send one signal between them.
  The box owner turns on lingering once (`loginctl enable-linger
  <account>`) so units survive logouts.
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
  "default_map": "/Levels/Example/Example",
  "executables": { "linux": "Game/Binaries/Linux/GameServer", "windows": "Game/Binaries/Win64/GameServer.exe" },
  "files": { "Game/Binaries/Linux/GameServer": "<sha256>", "...": "..." }
}
```

Every file except the manifest must be listed with its SHA-256, and the
server binary for the box's operating system must be listed.
`executables` names the server binary for each system, wherever the game
keeps it inside the build; ESM writes no game path of its own. The server
runs with the build's top folder as its working folder. `default_map`
(optional) is the map an instance loads when it names none. A build is
installed from:

- a folder inside the import folder (copied; links and special files
  refused);
- a `.zip`, `.tar.gz` or `.tgz` archive inside the import folder (unpacked
  with path checks: no absolute paths, no `..`, no links, no devices, no
  duplicate names, size and file-count limits);
- an `https` URL to such an archive, with a required SHA-256 (plain http is
  refused; redirects must stay on https). A failed download names only the
  class of the server's status; the details go to the service log.

The import folder is `<root>/imports` unless `build_import_dir` says
otherwise. A relative source is taken from it. ESM opens local sources
through an `os.Root` on that folder, so neither `..` nor a link leads out of
it, and any other path is refused with `source_not_allowed`, an answer that
says nothing about the path (whether it exists, what it is, who may read
it).

**A build runs with ESM's own rights.** A build is a program, and its
server runs as the ESM account: it can read every file in the root, the
secrets files with users, token hashes and every instance's passwords
included, and connect to the unix socket, which acts as admin. Installing
a build is therefore as much as being admin, and only the `admin` role may
do it (`POST /api/v1/builds`). The `operator` role can still update an
instance to a build an admin installed. Keeping game servers away from
ESM's files is an open item (see [Open items](#open-items)).

Everything is unpacked into `tmp/`, the manifest and every hash are checked,
and only then is the folder moved into `builds/<version>/`. Files are made
read-only. An existing version is never replaced. One build is shared by
every instance that runs it; nothing an instance writes goes into it.

**`steam_appid.txt`.** A real server writes this file next to its binary
when it starts (the Steam app id, in builds that are not Shipping builds).
That would be a write into a shared, read-only build. So when `steam_app_id`
is set, ESM writes the file there itself at install, after the hashes are
checked and before the build is moved into place, read-only like the rest
(a build that brings the file keeps its own). Builds installed while it was
unset get none; `esm doctor` names them. Whether the server accepts a file
that is already there and read-only, instead of writing its own, is
UNCONFIRMED.

**Update** switches an instance to another build of its channel: back up
the saves, stop the server if it runs, switch the build, start, and wait for
ready. If the new build does not become ready, ESM stops it, switches back
and starts the old build again; the answer says the update failed and was
rolled back. A rollback puts the previous build back, not the saves: what
the new build already wrote to them stays. To go back to the saves from
before the update, restore the backup the update made (see
[Backups](#backups)).

**Pruning** removes every build that no instance uses as its current build,
as one of its newest `keep_previous_builds` previous builds, or as the
build an unproven automatic update goes back to (`update_rollback_build`,
see [Automatic updates](#automatic-updates)). A single build can be
deleted when no instance uses it. Builds of a Steam source are also pruned
after each install from that source (see
[Builds from Steam](#builds-from-steam)).

**Making a manifest.** `esm build manifest <folder> --version V
--executable linux=<path> ...` writes the manifest of a build folder: the
SHA-256 of every file, under the rules of an install (no links or special
files, clean names that differ in more than case, the file-count limit),
checked for every system it names a binary for.

### Builds from Steam

A Steam source (`steam_sources`) is a Steam app (a dedicated server tool
app) and, usually, a password-protected beta branch. `esm build install
steam:<name>` (`POST /api/v1/builds` with that source; admin) and the
source's own checks every `check_every` do the same:

1. One SteamCMD run at a time. ESM writes a script into a fresh folder
   `steam-cache/.run-<random>/` (0700, the file 0600):
   `force_install_dir`, `login anonymous`, `app_update <app> -beta <branch>
   -betapassword <password>` (plus `validate` when a person asked), `quit`,
   and runs `steamcmd +runscript <file>` with `HOME` set to
   `steam-cache/<name>/home`. The folder is removed after the run. The
   password is never on a command line, in the environment, in a log or
   in the audit log; SteamCMD's output is masked before ESM keeps any of
   it.
2. A wrong password (SteamCMD says "Invalid Password") is `wrong_password`;
   any other failure is `steamcmd_failed`, with the last error line.
3. ESM reads `steamapps/appmanifest_<app>.acf` with a strict, bounded
   parser (quoted KeyValues text only, at most 1 MiB, depth 16, strings of
   4 KiB): the app id must match, `StateFlags` must say "fully installed",
   and `buildid` gives the version `steam-<name>-<buildid>`.
4. When that build is installed already, nothing more happens. Otherwise
   ESM adds up the size of the app's files (without `steamapps/`) and
   reads the free space of the file system that holds `builds/`. With less
   than twice that size free, the install is refused (409
   `not_enough_space`, with both sizes; also in the service log and the
   source's state) and nothing is copied; the next check or install tries
   again. Where the free space cannot be read, the install goes ahead and
   the service log says so once. Then the app's files are copied into the
   install work folder (links and special files refused), a manifest is
   made for the copy (channel, default map and executables from the
   source), and the install finishes like any other: hashes checked,
   `steam_appid.txt` placed, read-only, moved into `builds/`.
5. After a new build is installed, the source's builds are ordered by
   build id (as numbers) and those beyond the newest `keep_steam_builds`
   (default 3) are removed when no instance uses them (see Pruning above).
   The build just installed is always kept, even when the branch went back
   to an older build id. Builds of other sources and builds not from Steam
   are never touched. A removal is in the service log and in the audit log
   (`build.prune`, by the system).

The cache keeps the app's files, so a check when nothing changed is quick
and a new build is a delta download. A box holds each build twice (the
cache and `builds/`). The source's state (`GET /api/v1/steam`, `esm steam
list`) says what it last saw and whether the last run failed; an automatic
check that fails is written to the audit log when its error changes.

### Automatic updates

An instance follows the Steam source its current build came from (a build
`steam-<source>-<buildid>`); an instance on any other build follows none.
Its update target is the source's last seen build, when that is installed,
of the instance's channel and not its own build (so a build id set live
again on Steam is followed too). `auto_update`:

- `off` (default) and `install`: the target shows as available
  (`update_available`, `update` in the instance, the dashboard and the
  overview); nothing else happens. The source's checks install builds
  whatever the instances are set to.
- `restart`: the scheduler (every 15 seconds) updates a running instance
  with the usual update (backup, one stop signal, start, wait for ready,
  roll back) as soon as its server answers the query port (A2S_INFO) with
  0 players. While players are on, the count is not known, or the instance
  is starting, stopping, crashed or failed, it waits and says why. After
  `update_max_wait` (default 6h) it raises one warning (an `update` event
  and an audit entry) and keeps waiting. The update asks the player count
  again before its backup, and once more right before the stop (after the
  backup); a player who joined meanwhile puts it off, like an update
  refused because the instance is busy, and the scheduler tries again
  later. A backup made by an update that is then put off is removed again,
  and an automatic update prunes older backups (to the instance's `keep`)
  only once it goes ahead, so updates put off again and again on a busy
  server never push older backups out. An automatic update that failed is
  not tried again for the same build; a newer build, or an update by hand,
  starts over.
- `restart`, stopped instance: an automatic update never starts a server
  that is not running (an operator may have stopped it on purpose). It
  backs up the saves and switches the build (the old one goes to the
  previous builds), and starts nothing. The instance is marked
  `update_unverified` (with `update_rollback_build`, the build it ran
  before; over several such updates that stays the build that last ran),
  kept in `instances/<name>/run/autoupdate.json`. The next start, by API,
  command line, page, a restart or autostart, starts the new build and
  waits for ready like an update does, whatever `wait` says. If it does
  not get ready, ESM stops it, switches back to the old build and starts
  that, with the same events and audit entries as a failed automatic
  update, and records the failed build so automatic updates do not try it
  again; the start answers 422 `update_failed`. When it gets ready the
  mark goes. While the mark is set, the build to go back to counts as in
  use, so pruning keeps it. An update by hand of a stopped instance still
  starts the new build to prove it and stops it again, as before.

An update by hand to an older build of the same source (a lower build id)
pauses the instance's automatic updates until the source's newest build
has a build id above the one the instance left; any other update by hand
ends a pause. The failed build and the pause are kept in
`instances/<name>/run/autoupdate.json`, so they hold across restarts of
ESM. When there is nothing to update to any more, the last failure is
forgotten.

Each step is an `update` event (`available`, `started`, `updated`,
`failed`, `postponed`, `overdue`, `paused`); each automatic update, each
put-off one and each overdue warning is in the audit log as actor kind
`system`.

## Trust

**Releases.** A release is trusted because of its signature, not because
of where it comes from. The release workflow signs `SHA256SUMS` with an
Ed25519 key whose public half is built into ESM; the private half is the
secret `ESM_RELEASE_KEY` of the GitHub environment `release` (deployments
from `v*` tags only) and a copy the project owner keeps offline. The
workflow builds, tests and runs govulncheck (a known vulnerability stops
the release) in a job without secrets and with a read-only token, signs in
a job that runs only the `esm-sign` binary the build job made, and
publishes in a third job with the write token and no key.
`SHA256SUMS` starts with `version <tag>`, so the signature covers which
release the binaries belong to. ESM fetches the release list, the binaries and the two files over
https from GitHub (redirects on https only, time limits, size caps: 1 MiB
for the release list, 64 KiB for `SHA256SUMS`, 256 MiB for a binary), and
takes a binary only when the signature verifies, the signed version is
the release's tag and newer than the running ESM (unless forced), and the
binary's SHA-256 is the one the signed list names. A release whose files
lack the signature is refused. So GitHub, a proxy or a mirror cannot change
a release's files or pass an older signed release off as a newer one; it
can still withhold releases, or keep offering an old one (which a box does
not take, as it is not newer). The release check that only shows whether
a newer release is out reads the unsigned tag; a self-update checks the
signed version. Whoever holds the private key can make releases every ESM
accepts; a new key needs a release, signed with the old one, that carries
the new public key. `esm-sign` checks the key against the built-in public
key before signing, so a wrong secret fails in the workflow and not on the
boxes. Updates never happen on their own: a person runs `esm update-self`
or the admin-only `POST /api/v1/box/self-update`.

`esm update-self` replaces the binary and ends nothing; the running
service goes on with the old binary until it is restarted. The API call
ends the service a moment after it answers, so that its service manager
starts the new binary, and does that only when something will: on Linux
when systemd runs ESM as a service (systemd sets `INVOCATION_ID`; the unit
of `esm service install` has `Restart=always`). Otherwise, and always on
Windows, where ESM has no service manager, the call is refused with 409
`no_service_manager` and nothing changes, unless the caller sets
`exit_anyway`; ESM then updates, ends, and has to be started by hand (the
page asks before it sends that). An update holds the lock file
`<binary>.update.lock` next to the binary while it runs (made with
`O_EXCL`), so updates from the command line and the service never replace
the binary at the same time: a second one gets 409 `busy`, or the same
error on the command line. A lock older than 15 minutes was left by an
update that did not finish (its downloads end within 11 minutes) and is
taken over. Once an update through the API has put a new binary in place,
that service takes no other until it has ended.

**Steam.** A Steam build is trusted as far as Steam and the accounts that
upload builds to the app are: ESM does not check a signature of its own on
it. SteamCMD (Valve's program) runs with ESM's rights. The manifest of a
Steam build is made by ESM from what SteamCMD left in the cache, so its
hashes prove only that the installed build is what the cache held. An
install a person asks for runs SteamCMD with `validate`, which checks the
cache against Steam; the automatic checks skip that, to stay cheap. The
branch password keeps a test branch to the people who know it; ESM keeps
it like the other secrets.

**ESM as a service.** `esm service install` writes a systemd user unit
(`~/.config/systemd/user/esm.service`, `Restart=always`) and runs nothing:
not `systemctl`, not `loginctl`. It uses `KillMode=process`, so stopping
ESM stops ESM alone: game servers of the process supervisor are ESM's
children, and systemd would otherwise end them too.

## Ports and the firewall

ESM never changes firewall rules. It shows the ports of every instance
(create, list, status, the API) and the configured ranges (doctor, box
status), and `GET /api/v1/box/firewall` (the Box screen's "Show firewall
commands") gives the ufw and firewalld commands that open the **query**
port range for UDP. Only the query ports need to be reachable from outside:
a server opens no socket on its game port, and players join through Steam's
relay over outbound UDP. A server is never given a bind address, so its
query socket listens on every address of the box; the firewall decides
where it is reachable. Opening the query ports, and only them, is up to the
box owner.

To find out whether a port is free, ESM binds it for UDP and TCP for a
moment on the instance's `bind_ip`, or on all interfaces when that is
empty. In a test binary ESM binds loopback addresses only (its API listener
too) and refuses anything else without binding it: tests never make a
desktop firewall ask about them.

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

A stream lasts only as long as the login it was opened with. Before every
event and every keep-alive it checks that its token still exists, or that
its session is still open (not logged out, not expired), with the same
role; if not, the stream ends without sending the event, and a reconnect
needs a valid login.

## Backups

A backup copies the server's whole save folder (every file below
`data/Saved/SaveGames/`: the world saves, their metadata and the game's own
older copies) into
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
backup's save files in place (files below the save folder that the backup
does not have are removed), and starts the server again if it was running.
A restore refuses a backup that names any file outside the save folder.

Scheduled restarts (`schedule.restart_at`) restart running instances at
the given local times; a time that passes while an instance is stopped is
skipped.

## API overview

- JSON in and out, snake_case field names, no polymorphic fields, no
  integers above 2^53.
- Reads are GET. Every change is POST, PUT, PATCH or DELETE with a JSON body
  (an empty body counts as `{}`), at most 1 MiB.
- One error shape everywhere: `{"error": {"code": "...", "message": "..."}}`
  with 400, 401, 403, 404, 405, 408, 409, 413, 415, 421, 422, 429 or 500. (The
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
  Calls that try to change something are recorded whether they work, fail
  or are refused, and so is every call refused for lack of a role (403); a
  read refused for a missing login (401) or another host name (421) is
  not. Audit entries and logs never contain
  passwords or tokens. Each text field of an entry is cut to 256 bytes, and
  reading the audit log skips a line it cannot read.
- An instance carries `warnings`, a list of codes: `secrets_on_command_line`,
  `secrets_unconfirmed` (both in [Passwords](#passwords)) and
  `limits_not_enforced` (it has resource limits and the process supervisor
  does not apply them).
- An instance also carries `live`, read when it is asked for and never
  stored: the server's memory (its systemd unit's cgroup under systemd,
  the resident memory of the process otherwise; not measured on Windows
  yet), the player count and last save from its status file, and the
  Steam server id the running server logged (`steam_server_id`, new at
  every start). A value
  that cannot be known is null, and `unknown` says why. `scheduled` names
  the next timed restart and scheduled backup.
- `GET /api/v1/box` also reports the box's memory, CPUs, load average and
  the disk that holds the root, as the system reports them, with the same
  null-and-reason rule, and the time zone that restart times use.
- Downloads: `GET .../logs/download` gives a log as text with the rules of
  the log tail (at most the newest 16 MiB, read into memory, so at most two
  run at once and another gets 429 `busy`); `GET .../backups/{id}/download`
  gives a backup as a tar.gz (operator role). Both write in pieces, each
  with its own write deadline, so only a client that stops reading is cut
  off.

- Steam and updates: `GET /api/v1/steam` lists the Steam sources (never a
  password); `PUT /api/v1/steam/{name}/password` sets one (admin); `POST
  /api/v1/builds` with `{"source": "steam:<name>"}` runs a source (admin;
  200 when its build is installed already). `GET /api/v1/box` carries
  `update_available` and `release`; `POST /api/v1/box/self-update` (admin)
  puts the newest release in place and ends the service for its service
  manager to start again (refused with 409 `no_service_manager` when
  nothing would, unless `exit_anyway` is set; see [Trust](#trust)).

See [openapi.yaml](openapi.yaml) for every endpoint.

## Logins and exposure

- **Loopback by default.** The API listens on `127.0.0.1:8740`. ESM refuses
  to start on any other address unless `tls_cert` and `tls_key` are set. A
  reverse proxy with TLS in front of a loopback ESM is the expected way to
  reach it from elsewhere.
- **Host names.** On TCP, ESM answers only requests that name one of its
  own hosts in the `Host` header: `localhost`, `127.0.0.1` and `[::1]`
  with the listen port, the listen address itself when it is a specific
  address, and the names in `public_hosts`. Anything else, the page
  included, gets 421 `misdirected_request`. A page of another site that
  points its own name at 127.0.0.1 (DNS rebinding) sends its own name, so
  it reaches nothing. The unix socket is not checked.
- **Unix socket.** `<root>/esm.sock` (mode 0600) acts as admin; it is how
  the command line on the box works. It is made under a umask that leaves
  it to its owner, so it is never open to other accounts, not even before
  its mode is set, and ESM refuses to start when other accounts may write
  to its folder (they could remove or replace it). A socket path longer
  than the system takes (107 bytes on Linux, 103 on macOS) is refused at
  start with a message that says so; set `socket` to a shorter path. A
  stale socket file is removed at start; a socket that still answers means
  another ESM already serves the root.
- **Setup code.** While no admin exists, ESM makes a one-time setup code
  (24 characters, valid for 24 hours), prints it once and keeps only its
  hash. `POST /api/v1/setup` with the code creates the first admin.
- **Users and sessions.** Passwords need at least 12 characters and are
  stored as PBKDF2-SHA256 with 600,000 iterations. Login and setup take a
  JSON body only (`Content-Type: application/json`, else 415), which a form
  on another site cannot send, and refuse a request whose `Origin` header
  names another origin (403). The origin must match in scheme too: `http`
  on a plain listener, `https` when ESM has TLS or `behind_tls_proxy` is
  set; so an `https` page is another origin to a plain-http ESM on the same
  host, and a plain-http page is another origin behind a TLS proxy. A request without `Origin` is accepted, so
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
  when present, must match the scheme and the host. Failed logins are
  limited to 5 per 5 minutes per user name, and per client address for a
  client that connects from an address of its own (see the next point).
  Every failed login is answered after a pause of one second. The audit
  log keeps the name typed at login or setup only when it is an existing
  user's name; otherwise it says `(unknown)`, or `(invalid)` for text that
  cannot be a user name, so a password typed into the name field is never
  written down.
- **Behind a reverse proxy.** The proxy terminates TLS and forwards to ESM
  on loopback. It must pass the `Host` header through unchanged (the
  `Origin` checks compare against it), that name must be listed in
  `public_hosts`, and `behind_tls_proxy` should be set
  so the session cookie is Secure and an `Origin` must say `https` (with it
  set, browser logins work only through the proxy). Do not set it without such a proxy: a
  Secure cookie is never sent over plain http, so browser logins would
  stop working. ESM takes the client address from the connection, never
  from forwarding headers: those can be forged unless ESM is told which
  proxies to trust, and it has no such setting. Behind a proxy or an SSH
  tunnel every client has the same address, so a limit per address would
  let anyone lock every user out with a few wrong logins. So when
  `behind_tls_proxy` is set, or a client connects from a loopback address,
  failed logins count per user name only, each is answered after the
  pause, and setup codes tried that way share one limit. A client that
  connects directly from an address of its own is also limited per
  address.
- **Tokens and roles.** API tokens (`esm_` + 43 characters) are shown once
  and stored as SHA-256. Roles: `read` sees everything except tokens, the
  audit log, backup downloads, the health check (`/box/doctor`, which binds
  every free port of both ranges for a moment; admins only, one at a time),
  and the server output and state reasons of instances that pass their
  passwords on the command line; `operator` sees those too and also starts,
  stops, restarts, updates, backs up, restores and downloads backups;
  `admin` does everything else (install builds, create, change, delete,
  secrets, tokens, pruning). Tokens are never accepted in URLs.
- **Page headers.** Every reply ESM writes, of the page and of the API,
  errors and the router's own redirects included, carries a strict
  Content-Security-Policy (no inline scripts or styles, nothing from other
  origins, `frame-ancestors 'none'`), `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: no-referrer` and `X-Frame-Options: DENY`.
- **Page files.** The page's templates, stylesheet and script are compiled
  into the binary, never read from the disk. Only the stylesheet and the
  script are served as files, under `/static/`; every other path is a
  screen or the page's 404. A folder is never listed. The router redirects
  a path with plain `.` or `..` segments or doubled slashes to its clean
  form; after that, a request path that has a `.` or `..` segment (written
  `%2e%2e`, say), a backslash (also `%5c`), a colon (a drive letter such as
  `/C:/Windows/win.ini`, or a Windows stream name), a segment that starts
  with a dot, or a control character gets 404, whatever files there are.

## The web page

- **Plain HTML from the service.** Each screen is an `html/template` file
  (one per screen, plus a shared layout) in `internal/page`, made on the
  server. One stylesheet (`/static/esm.css`) and one small script
  (`/static/esm.js`), no framework, no build step, nothing loaded from
  another origin. No inline script or style, no `style` or event handler
  attributes: the Content-Security-Policy above holds for every page.
- **A client of the API, nothing more.** A screen gets its data by calling
  the API inside the process, as the browser's user (the same cookie or
  token, so the same roles and the same refusals), and decodes the same
  answers any client gets. It never reads ESM's state another way. What a
  screen needs and the API did not give was added to the API first.
- **Changes.** The script makes every change with `fetch` to the API,
  sending JSON and the `X-CSRF-Token` header. The CSRF token is in a
  `<meta name="csrf-token">` tag of each page; it is the session's own
  random value, never the session id, which stays in the HttpOnly cookie
  and is never part of a page. After a change the page reloads with a
  notice from a fixed list.
- **Live.** Screens follow the API's event stream (state, and log lines
  where a log is shown; audit entries on an admin's dashboard). A browser's
  EventSource gives up after an HTTP error (a 401 when the login ended, a
  502 from a proxy while ESM restarts), so the script closes the stream on
  every error and opens it again itself, waiting longer each time (1 s
  doubling to 30 s, with jitter); when the API says the login is gone
  (401) it goes to the login. After a gap it reads the state and the log
  again, since events missed meanwhile are not sent again. A log on the
  page keeps at most 2000 lines.
- **Logging in.** Without a login every screen sends the browser to
  `/setup`, which is the first-run form while no admin exists (setup code,
  name, password twice) and the login after that. Where the browser goes
  after logging in is checked to be one of the page's own paths.
- **Roles.** Every screen shows what the caller's role may see and offers
  only what it may do. The read role sees the API's notice in place of the
  server output of an instance that passes its passwords on the command
  line; every role sees that instance's UNSAFE warning.
- **Honest gaps.** The page shows only what the API returns and says where
  ESM cannot know something yet: CPU use per server, memory on Windows, the
  player list (only the count from the status file), rule defaults and
  allowed values (rules are shown as name and value; there is no rules
  description from the game yet, so no presets), the Steam runtime files,
  whether players can reach the box from outside, and a budget for all
  game servers together.
- **Health check on demand.** The Box screen runs the health check
  (`GET /api/v1/box/doctor`) only when an admin asks, because it test-binds
  every free port of the ranges; other roles are told it needs the admin
  role. The Builds screen offers installing only to admins.
- **Fonts.** The system's own fonts: a narrow sans-serif for headings, a
  plain one for text, a monospace for numbers and logs. No font files are
  shipped. Fonts under an open licence can be added later as `.woff2`
  files next to the stylesheet, with their licence texts, and an
  `@font-face` rule; the policy already allows fonts from the same origin.
- **Phones.** Every control is at least 44 pixels tall; the side menu moves
  above the content, card grids reflow, and wide tables scroll inside their
  box.

## Command line

```
esm [--root DIR] [--json] [--url URL [--token TOKEN]] <command> [arguments]

serve | doctor [--bind IP] | version [--check] | update-self [--force]
service install [--force]
list | status [name]
create <name> [--build V] [--channel C] [--map M] [--world W] [--display-name N]
       [--cluster C] [--game-port P] [--query-port P] [--bind IP] [--max-players N]
       [--set Key=Value ...] [--arg -X=Y ...] [--secret-delivery file|command_line]
start | stop | restart <name>
logs <name> [-n N] [-f] [--console]
build install <folder|archive|https-url|steam:<source>> [--sha256 HEX] | build list | build prune
build manifest <folder> --version V [--channel C] --executable linux=<path> [--default-map M] [--commit C]
steam list | steam password <name> [--clear]   (the password on standard input)
update <name> --to <version>
backup <name>
restore <name> <backup-id>
token create --role read|operator|admin [--name NAME]
```

With `--url` the command line talks to an ESM elsewhere with an API token,
best given in `ESM_TOKEN` (a `--token` argument shows in the process
list). `--url` must be https, except to this machine (`localhost`,
127.0.0.1, `[::1]`): over plain http the token would cross the network in
clear.

Every command prints one result line, or one JSON object with `--json`, and
exits 0 or 1. Errors go to stderr (with `--json`, to stdout as an error
object). `logs` prints the log lines themselves. `create`, `list` and
`status` print the warnings of an instance in words; one that passes its
passwords on the command line is marked UNSAFE. `doctor` works without a
running service: it checks the root, the config, the listen rule, free
ports, disk space, the supervisor (with systemd lingering), the socket and
which instances pass passwords on the command line, changes nothing, and
adds the service's own view when a service answers. It checks free ports by
binding each one for a moment on all interfaces, or with `--bind IP` on
that address only (the tests use 127.0.0.1).

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

## The real server

Everything ESM assumes about the Echoes of Durinn dedicated server is in
[`internal/gameserver`](../internal/gameserver/gameserver.go); values there
are marked CONFIRMED or UNCONFIRMED. A first real server ran on a Linux host
(one server on the box) and confirmed the following.

**Confirmed**

- The command line: the map first, then exactly `-unattended -Port=
  -QueryPort= -ServerName= -WorldName= -MaxPlayers= -ListInBrowser=
  -UserDir= -abslog=`, with the build folder as working folder. Every game
  setting also works as `-Key=Value`.
- **Never `-MULTIHOME`.** With it the server advertises its bind address
  where its Steam identity belongs; players see it in the server list but
  cannot join. ESM never passes it, refuses it as a rule or extra argument,
  and does not give a configured `bind_ip` to the server. Without it the
  query socket listens on every address; the firewall filters.
- Ports: only the query port (UDP) needs to be reachable from outside. The
  game port has no socket; joins go through Steam's relay, outbound.
- The Steam client libraries (`steamclient.so`, 64-bit, and
  `libsteamwebrtc.so`) are not part of a server build; the server finds
  them through `LD_LIBRARY_PATH` (`steam_runtime_dir`).
- The server logs on to Steam anonymously and logs a server id that is
  new at every start.
- A non-Shipping server writes `steam_appid.txt` next to its binary at
  start, so a shared read-only build needs the file put there first.
- `-UserDir` is honoured: saves go below `<UserDir>/Saved/SaveGames/`, with
  the game's own older copies beside them, and an autosave every 5
  minutes. A new build never touches them.
- One SIGINT: the server saves and exits cleanly within 2 to 3 seconds
  (exit code 130), with or without players; the next start resumes the
  world, players' characters included.
- The ready (session registered), save and logon lines described in
  [Life cycle](#life-cycle).
- `systemctl --user` works for the server's account, but user units stop at
  logout unless lingering is on.
- Memory and files: about 0.6 GB resident while idle and up to about 2.2 GB
  at peak with one player; about 60 open files. Plan at least 3 GB of memory
  per server (a `limits.memory_max` well below that will end it), and more
  for more players: how it grows with players is not measured yet.

**Still UNCONFIRMED**

- What SIGTERM does (ESM never sends it).
- Whether the server accepts a pre-placed, read-only `steam_appid.txt`.
- Two or more servers on one box (told apart by their ports only), and a
  query port other than the default.
- Whether a loose per-server config file next to the build is read; ESM
  passes every setting on the command line instead.
- The admin and password switches, and which other switches take secrets
  (the test server ran with neither password).
- The secrets file: the real server does not read one yet, so the switch
  that names it and the format of the file are ESM's proposal for the game
  side. Until the game has it, an instance whose server needs a password
  must use `secret_delivery: command_line`, which is unsafe on a shared box
  (see [Passwords](#passwords)).
- The exact place of the save line's words in their line, and the log
  categories of the lines ESM reads.
- The status file (the real server writes none; ESM relies on the log
  line, then the query probe).
- Whether the server answers A2S_INFO before it has registered its session
  (ESM checks the log line first), and whether it answers on the box's
  loopback address (seen answering from outside the box).
- How to stop a server politely on Windows, and Windows hosts at all.
- Long uptimes, more than one player, admin login, and an automatic warning
  to players before a stop (there is none; a stop is immediate).

The fake server used by the tests follows these assumptions, with one
difference on purpose: it binds the query port on 127.0.0.1 only, where a
real server listens on every address. When a real host proves an assumption
wrong, it changes in the adapter and the fake, and nowhere else.

## Phases

- **Phase 0 (this):** the service, the command line, the API, builds,
  instances, backups, and tests against the fake server.
- **Phase 1:** the web page (server-rendered HTML over the API).
- **Phase 2:** the real server: the adapter follows the facts confirmed on
  a real host (see [The real server](#the-real-server)).
- **Phase 3:** signed releases and self-update, ESM as a systemd user
  service, builds from Steam with SteamCMD, automatic updates.
- **Later:** player controls, Windows hosts (planned for the first public
  release).

## Known limits

- **Passwords can be confirmed by an admin.** A setting that contains an
  instance's password is refused (422 `password_in_settings`), so an admin
  can test a guess by putting it into, say, the display name. Admins set
  the passwords anyway; the API still never shows them.
- **Users and sessions.** There is no way yet to change or remove a user,
  or to end other sessions; restarting ESM ends every session.
- **The socket on Windows.** Windows has no mode 0600 for the socket file
  and ESM sets no ACLs, so the socket is as private as its folder's ACL.
- **A server that does not stop is left running.** After `stop_timeout` ESM
  reports `stop_timeout` and does nothing more; ending the server by force
  is the box owner's call. While it runs, a new start of its instance is
  refused.
- **The status file is the fake's.** A real server is seen ready through
  its log line or, failing that, its answer to A2S_INFO.
- **A stop from outside ESM counts as a crash.** A server stopped with
  `systemctl --user stop` (or ended any other way ESM did not ask for) has
  crashed as far as ESM can tell, and with `restart_on_crash` it is started
  again.
- **Nothing watches a server after a stop timeout.** The instance stays
  `failed` until someone stops it again, even if the server exits on its
  own in the meantime.
- **`bind_ip` limits ESM's port checks only.** A port free on that address
  may still be taken on another one, where the server also listens.
- **Separate arguments are not a boundary for the real server.** ESM hands
  a server each setting as its own argument, but an Unreal server joins its
  command line back together and searches it for switches. ESM refuses
  quotes, `MULTIHOME` and white space followed by `-` in every setting that
  goes onto the command line (see [Instances](#instances)); a value that
  holds another switch's name without a space before it (`x-Port=1`) is
  left to the server's own parsing.
- **Chat that imitates a log line.** ESM only takes lines that carry the
  engine's full prefix with the words right after the category (and an
  optional verbosity). Should the game ever log what a player types right
  after the category, such text could pass for one of those lines.

- **The player check and the stop are still two steps.** An automatic
  update asks the query port again right before the stop; a player who
  joins in the moment between that answer and the stop is disconnected.
- **Automatic updates are not instant.** A new Steam build is seen at the
  source's next check (`check_every`), and an instance acts on it at the
  scheduler's next tick (15 seconds).
- **SteamCMD's words are assumed.** That a wrong branch password shows as
  "Invalid Password", that `StateFlags` bit 4 means fully installed, and
  that a quoted `force_install_dir` path with spaces is read as one word
  are UNCONFIRMED on a real host (the fake SteamCMD follows them).
- **Links in a Steam app are refused.** A depot that holds symbolic links
  cannot be installed, under the same rule as any build.
- **Self-update leftovers.** A self-update that is killed while it
  downloads leaves a `.esm-update-*` file next to the binary, and its lock
  file `<binary>.update.lock`, which keeps other self-updates out until it
  is 15 minutes old. On Windows a second self-update fails while the
  `.previous` binary of the first one still runs (it cannot be replaced);
  end that process first.

## Open items

- **Keep game servers away from ESM's files.** Today a game server runs as
  the ESM account, so a compromised server (it faces the internet) can read
  ESM's secrets and act as admin through the socket. Planned: run game
  servers under a separate account, or at least deny them `<root>/secrets`,
  the socket and the other instances' folders (systemd
  `InaccessiblePaths=`), and move the socket out of their reach.

## Open questions

- The UNCONFIRMED items in [The real server](#the-real-server).
- Whether the server can save on request, so backups need not wait for the
  next periodic save.
- When the game server reads its passwords from the secrets file. That is
  what lets the command-line compatibility mode be removed.
- Whether a server that does not confirm its passwords should be refused
  instead of only flagged.
- How Steam branches map to ESM channels (today each source names its
  channel).
- Whether several boxes should be managed from one page, and how.
