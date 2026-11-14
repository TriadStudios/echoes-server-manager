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
  [`internal/gameserver`](internal/gameserver/gameserver.go). A first real
  server on a Linux host confirmed the command line, the ports, the Steam
  libraries, the save folder, the log lines and the one-signal stop, and
  the adapter follows those facts; what is not proven yet is marked
  UNCONFIRMED (see [docs/DESIGN.md](docs/DESIGN.md#the-real-server)). ESM is
  tested against a fake server that follows the adapter.
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
`go.mod` names the toolchain go1.27.4, which the go command fetches by
itself unless `GOTOOLCHAIN=local` is set. CI and releases build with the
newest patch release of Go 1.27.

```sh
go build ./cmd/esm
```

## Releases and updates

Each release (a tag such as `v0.3.0`) carries `esm-linux-amd64`,
`esm-windows-amd64.exe`, `SHA256SUMS` and `SHA256SUMS.sig`, the Ed25519
signature of `SHA256SUMS`. `SHA256SUMS` starts with the line
`version <tag>` and then lists the SHA-256 of both binaries, so the
signature also says which release they belong to. The public half of the
signing key is built into ESM (`internal/release.PublicKey`):

```
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAEUMNTA3aSwhquvpDj+iD59eFAonmvKFROj/RbZyZksI=
-----END PUBLIC KEY-----
```

To check a download by hand (OpenSSL 3), save that key as
`esm-release.pem`, then:

```sh
base64 -d SHA256SUMS.sig > SHA256SUMS.sig.bin
openssl pkeyutl -verify -pubin -inkey esm-release.pem -rawin -in SHA256SUMS -sigfile SHA256SUMS.sig.bin
head -1 SHA256SUMS                      # must name the release you downloaded
sha256sum --check --ignore-missing SHA256SUMS
```

(`sha256sum` warns that one line, the version line, is not a checksum
line; that is expected.)

`esm version --check` says whether a newer release is out; the service
looks too (at most every 6 hours, only while someone looks at the box) and
shows it on the Box page and in `GET /api/v1/box` (`update_available`).
Set `"update_check": false` in `esm.json` to turn that off.

ESM never updates itself on its own. `esm update-self` (or the Box page's
update button, admins only) downloads the newest release for this system,
checks the signature with the built-in key and the binary's hash against
the signed list, and only then puts the new binary in place; the old one
stays next to it as `esm.previous` (to roll back, move it back). The page's
button (`POST /api/v1/box/self-update`) then ends the service so that its
service manager starts the new binary, and does that only when something
will: on Linux, when systemd runs ESM (the unit of `esm service install`).
Otherwise, and on Windows, the update is refused and nothing changes,
unless the caller sets `exit_anyway` (the page asks first); ESM then
updates, ends, and has to be started again by hand. `esm update-self` on
the command line replaces the binary and ends nothing: restart the service
yourself (`systemctl --user restart esm`). Game servers keep running
either way and the new ESM adopts them.

One self-update runs at a time: while it runs it holds the lock file
`<binary>.update.lock` next to the binary, and another one, from the page
or the command line, is refused. A lock older than 15 minutes was left by
an update that did not finish and is taken over.

### Making a release (maintainers)

The release workflow signs in a job of its own that runs in the GitHub
environment `release`, whose secret `ESM_RELEASE_KEY` is the base64 of the
32-byte private seed. That job runs only the `esm-sign` binary the build
job made; building, testing and publishing run without the key. Set the
environment up once, allowing deployments from `v*` tags only:

```sh
gh api -X PUT repos/TriadStudios/echoes-server-manager/environments/release \
  -F 'deployment_branch_policy[protected_branches]=false' -F 'deployment_branch_policy[custom_branch_policies]=true'
gh api -X POST repos/TriadStudios/echoes-server-manager/environments/release/deployment-branch-policies \
  -f name='v*' -f type=tag
gh secret set ESM_RELEASE_KEY --env release --repo TriadStudios/echoes-server-manager < release-key.txt
```

Two more settings belong with the key: the `release` environment should
require a reviewer (repository settings, Environments), so that no
release is signed before a maintainer approves the run, and a tag
ruleset should limit who may create `v*` tags.

Then push a tag `vX.Y.Z`. Without the secret the sign job fails and
nothing is published.

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

The token is shown once. Send it as `Authorization: Bearer <token>`. For
the command line, put it in the `ESM_TOKEN` environment variable, for
example from a file only you can read, and give the address with `--url`:

```sh
export ESM_TOKEN="$(cat ~/.esm-token)"
esm --url https://<your-host> list
```

(`--token` works too, but other accounts on the machine can read command
lines in the process list.) `--url` must be https unless it points to this
machine: over plain http the token would cross the network in clear.

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

### Linux: run ESM as a service

```sh
esm --root ~/durinn service install
systemctl --user daemon-reload
systemctl --user enable --now esm
```

`esm service install` writes `~/.config/systemd/user/esm.service`
(`Restart=always`, `ExecStart=<this esm binary> serve --root <root>`) and
runs nothing else. It uses `KillMode=process`: stopping or restarting ESM
never stops game servers. Lingering (above) keeps the unit running after
you log out; ESM prints the command but does not run it.

## Real server setup

What a box needs before ESM can run an Echoes of Durinn dedicated server
(Linux):

- **Steam client libraries.** A server build does not include them. Put
  the 64-bit `steamclient.so` (from the `linux64` folder of the Steamworks
  SDK redistributable; the one at its top level is 32-bit) and
  `libsteamwebrtc.so` into `<root>/steam/linux64/`, or set
  `steam_runtime_dir` in `esm.json`. ESM puts that folder first on every
  server's `LD_LIBRARY_PATH`. `esm doctor` checks that both files are there
  and are 64-bit.
- **Steam app id.** Set `steam_app_id` in `esm.json` (the game's app id, in
  digits; there is no default) **before** installing builds. The server
  wants a `steam_appid.txt` next to its binary and writes one at start;
  builds are shared and read-only, so ESM writes the file at install time
  instead. `esm doctor` warns while the app id is unset and names builds
  without the file. (Whether the server is happy with a read-only file is
  not proven yet.)
- **Build layout.** A build is a folder with a `manifest.json` that lists
  every file with its SHA-256. `executables` names the server binary for
  each system, as a path inside the build (wherever the game keeps it), and
  `default_map` names the map instances load unless they set their own
  `map`. The server runs with the build's top folder as its working folder.
  See [docs/DESIGN.md](docs/DESIGN.md#builds).
- **Ports.** Each instance gets a game port and a query port from the
  configured ranges. Only the **query ports** (UDP) need to be open
  inbound: the server opens no socket on its game port, and players join
  through Steam's relay over outbound UDP. `GET /api/v1/box/firewall` (the
  Box screen's "Show firewall commands") prints the commands; ESM never
  runs them. ESM never gives the server a bind address (`-MULTIHOME` breaks
  joins, and ESM refuses it), so its query socket listens on every address
  of the box: the firewall decides where it is reachable.
- **Stopping.** A stop sends the server exactly **one SIGINT**: it saves
  its world and exits within seconds. ESM waits up to `stop_timeout`
  (default 120 s) and never sends a second signal or kills the server; one
  that is still running is left running and reported as `stop_timeout`.
  The same holds for a `systemctl --user stop` of its unit, before or
  after ESM's own stop: ESM stops the unit the same way, and units have no
  stop timeout, so systemd never sends a second signal.
- **Saves.** Each instance's server keeps its saves in
  `<root>/instances/<name>/data/Saved/SaveGames/`; backups and restores
  copy that whole folder. Installing or removing a build never touches it.
  The server log is `<root>/instances/<name>/logs/server.log`.
- **Passwords.** The game server takes its passwords (and its
  announcements and problem report tokens) only on its command line today.
  An instance whose server needs a join or admin password, or a token, must
  be set to `secret_delivery: command_line` until the game reads the
  secrets file, and that mode is unsafe on a shared box (see
  [Server passwords](#server-passwords)).
- **Lingering.** With systemd user units, turn on lingering once (above),
  or every server stops when the ESM account logs out.
- **Memory.** One server used about 0.6 GB idle and up to about 2.2 GB with
  one player. See [docs/DESIGN.md](docs/DESIGN.md#the-real-server).

## Builds from Steam

ESM can take server builds straight from a Steam app (a dedicated server
tool app with a Linux depot), on a password-protected beta branch, with
SteamCMD.

1. Install SteamCMD into `<root>/steamcmd/` (Valve's tarball; on 64-bit
   Ubuntu SteamCMD also needs `lib32gcc-s1`):

   ```sh
   mkdir -p ~/durinn/steamcmd && cd ~/durinn/steamcmd
   curl -sqL "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz" | tar zxvf -
   ```

   Elsewhere? Set `steamcmd` in `esm.json` to its `steamcmd.sh`.
2. Describe the source in `esm.json` (the values here are examples):

   ```json
   "steam_sources": [
     {
       "name": "main",
       "app_id": "1000001",
       "branch": "testing",
       "executable": {"linux": "Server/Binaries/Linux/ServerBinary"},
       "default_map": "MapName",
       "channel": "test",
       "check_every": "15m"
     }
   ]
   ```

   `executable` is the server binary's path inside the app's files.
   `check_every` (default 15m, `"0"` for off) is how often ESM asks
   SteamCMD for a new build; when nothing changed that is a quick check.
3. Give ESM the branch password, on standard input (never as an argument):

   ```sh
   esm steam password main < branch-password.txt
   ```

   It is kept in `<root>/secrets/steam.json` (0600), never shown, logged or
   written to the audit log, and it reaches SteamCMD only in a private
   script file that is removed after each run.
4. `esm build install steam:main` downloads the branch (validating every
   file) and installs its build as `steam-main-<buildid>`, read-only, like
   any build; the regular checks do the same without validation. ESM keeps
   the app's files in `<root>/steam-cache/main/` so later downloads are
   deltas, and runs SteamCMD with `HOME` set to a folder there.
   `esm steam list` shows what each source last did; `esm doctor` checks
   SteamCMD, the sources and the passwords.

A new build is installed only when the disk that holds `builds/` has at
least twice its size free; otherwise the install is refused
(`not_enough_space`, shown by `esm steam list`) and nothing is copied.
After each install from a source, ESM keeps that source's newest
`keep_steam_builds` builds by build id (default 3, at least 1, set in
`esm.json`) and removes its older ones that no instance uses (as its
build, a kept previous build, the build an unproven update goes back to,
or a build an update under way still needs). Builds of other sources and
builds not from Steam are left alone.

Anyone else can fetch the same build once the tool app is released (in the
free dedicated server package):

```sh
steamcmd +login anonymous +app_update <app> -beta <branch> validate +quit
```

A password-protected branch also needs its password. Keep it off the
command line, where other accounts on the machine can read it; ESM puts
it in a private script file that SteamCMD runs with `+runscript`.

### Making a build folder by hand

A build folder needs a `manifest.json`. `esm build manifest` writes it,
with the SHA-256 of every file, under the same rules as an install (no
links, clean names):

```sh
esm build manifest ./ServerBuild --version 1.2.0 --channel test --executable linux=Server/Binaries/Linux/ServerBinary --default-map MapName
```

## Automatic updates

An instance follows the Steam source its build came from (a build named
`steam-<source>-<buildid>`). When the source has a newer build of the
instance's channel, the instance shows it as available. What happens next
is its `auto_update` setting (Settings on the page, or `PATCH` the
instance):

- `off` (default) and `install`: nothing more; update by hand.
- `restart`: ESM updates a running instance itself as soon as its server
  answers the query port with **0 players**: a backup, one stop signal,
  the new build, and a roll back to the previous build when the new build
  does not get ready. A roll back puts the build back, not the saves: what
  the new build already wrote stays, and restoring the backup made before
  the update undoes that.
  The player count is asked again before the backup and once more right
  before the stop; a player who joined meanwhile puts the update off, and
  a backup made for it is removed again (older backups are pruned only
  when an update goes ahead). While players are on it waits and asks
  again; after `update_max_wait` (default 6h) it raises a warning and
  keeps waiting. A failed automatic update is not tried again for that
  build.
- A **stopped** instance set to `restart` is never started by an update:
  ESM backs up its saves and switches its build, and that is all. The
  instance shows `update_unverified` (on the page: "new build, checked at
  next start"). The next start, whoever makes it (API, command line or
  page), starts the new build and waits until it is ready; if it does not
  get ready, ESM stops it, puts the previous build back, starts that, and
  does not try the failed build again. Until then the previous build
  counts as in use, so pruning keeps it. A crashed or failed instance is
  not updated; it waits and says why.

Updating an instance by hand to an older build of its source pauses its
automatic updates until the source has a build newer than the one it
left.

Every step is an `update` event, and each automatic update is in the audit
log.

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
whether players can reach the box from outside), the page says so. A
server's settings screen has a field for every game setting ESM knows by
name (see [Game settings](#game-settings)), grouped, each with its help,
range and default; an empty field leaves the setting to the game. Reading works without JavaScript; changes need it. What each role
may do is the same as in the API.

## Command line tour

```sh
esm doctor                                   # read-only checks of this box
esm build install ~/durinn/imports/build-1.0.0.tar.gz  # or a folder there, or an https URL with --sha256
esm build list
esm game-settings                            # the game settings ESM knows by name
esm create alpha --set HarvestRate=2 --set PvP=True  # ports are picked from the configured ranges
esm secret alpha server-password < join-password.txt # a password or token, on standard input
esm start alpha                              # waits until the server is ready
esm status alpha
esm logs alpha -n 100 -f
esm backup alpha
esm update alpha --to 1.1.0                  # goes back to the old build if the new one never gets ready
esm restore alpha 20260101T120000Z-manual
esm stop alpha
esm list --json
esm build install steam:main                 # a new build of a Steam source, if there is one
esm steam list
esm version --check                          # is a newer ESM out?
esm update-self                              # put it in place (signature checked)
```

Every command prints one result line, or one JSON object with `--json`, and
exits 0 on success and 1 on error (`logs` and `game-settings` print more
lines). `esm help` lists every command.

## Game settings

The game server takes its settings on its command line as `-Key=Value`.
ESM sets the server name, world, player limit, listing, ports and passwords
itself; every other setting the server builds take is in ESM's catalog of
game settings, with its type, range, default and a line of help. See them
with `esm game-settings`, `GET /api/v1/game-settings` or on a server's
settings page.

An instance keeps its game settings in `rules` (`--set Key=Value` at
create, `rules` in the API, the fields of the settings page). A setting
that is not set is not passed, and the server uses its own default. A
setting of the catalog is checked when it is set or changed: a number must
be in its range, a choice one of its values, `True` or `False` for a
switch, a date `YYYY-MM-DD` and not in the future. Keys outside the
catalog are passed as they are, for settings of newer server builds.

The server joins its arguments into one line and finds a switch by its
name and `=` anywhere in it: outside double quotes by its usual lookup,
and inside them too by some of its code (the folder to load plugins from,
`PLUGIN=`, is found that way). It also searches the line for a few names
as plain text. So every value that goes onto the command line (a rule, an
extra argument after its `=`, the server and world names, the map, and
the passwords and tokens in the compatibility mode below) follows the
same rules when it is set or changed:

- It is not empty (leave a setting out for the game's default).
- It has no `=` and no double quote, with or without a space
  (`x;PvP=True` would set PvP, and so could `Hi PvP=True all`).
- It is printable text and the plain space: letters, marks, digits,
  punctuation and symbols of any script. Refused are control characters;
  every other white space (a tab, a line break, a no-break space: any
  character that a Windows or a Linux host of the server counts as white
  space, because after white space the server reads `-Name` or `/Name` as
  a switch); characters that do not show (zero width and direction marks,
  the soft hyphen), private-use and unassigned code points; and the en
  dash, which the server does not start with on its command line (write
  `-`).
- It has no space at its start or end, and no space followed by `-` and
  more (`a - b` is fine, `a -b` is not).
- A `-` right before a letter A-Z or a-z has a letter or digit (A-Z, a-z,
  0-9) right before it: `well-met` and `Alpha-EU` are fine, `-fast` and
  `[EU]-Alpha` could start a switch (write `[EU] - Alpha`). The same goes
  for a `-` after a letter outside A-Z, such as an accented one.
- A value without a space has no `,` or `)`: the server reads it unquoted
  and would end it there. One with a space is quoted whole and may hold
  them (`Welcome, traveller`).
- It holds none of the names the server's engine searches its command
  line for as plain text, in any case of their letters and in the middle
  of a word too: `-ansimalloc` and some two dozen like it, listed in
  [docs/DESIGN.md](docs/DESIGN.md#game-settings).
- A rule key, and the name of an extra argument (`-Name` or
  `-Name=Value`), has ASCII letters and digits only (`x_PvP` would be the
  switch `PvP`), no two of them name the same switch in any case, and none
  holds one of those names unless it is that switch (`-ansimallocx` would
  be read as `-ansimalloc`; `-ansimalloc` itself is passed as what it is).

The name of an instance is in the folders ESM puts on the command line,
so a new instance's name may not hold one of those names either. And the
whole command line of an instance, with everything on it, may have 12000
characters after a change: the server's engine does not start with more
than 16383, and what starts the server may add to the line. A change that
would make it longer is refused (`command_line_too_long`).

A value stored by an earlier version is kept until it is changed. An
empty one (a rule, or an extra argument that ends in `=`) is left off the
command line when the server starts, and the service log says so; any
other that breaks the rules is passed as it is, and the instance carries
the warning `unsafe_stored_settings`, which names the settings (never
their values) to change. One exception: with an en dash anywhere on its
command line the server's engine stops at once, so ESM does not start a
server whose line would hold one (409 `command_line_unsafe`, which names
the setting, a password or token by its field, and never a value). An
instance whose stored command line is longer than 12000 characters
carries the warning `command_line_too_long`; beyond 16127 ESM does not
start its server, and says why (409 `command_line_too_long`): of the
16383 the engine starts with, ESM keeps 256 free for what a build's start
script may put on the line besides (the name of the game's project, say),
since it does not know what a build's script adds.

These rules follow how the engine reads its command line, as far as ESM
knows it from the engine's source; they are not yet checked against a
real server, and they do not cover everything. What ESM puts on the line
from its own configuration is not checked: the folders below ESM's root,
the path of a build, and a build's default map; keep those free of `=`,
of double quotes, of a space followed by `-` and of the names above (an
en dash in the folders is the one thing ESM looks for there: it does not
start the server then, 409 `command_line_unsafe`). The
list of names is the one of the engine the server builds are made with:
a later engine, a plugin or the game itself may search for a name ESM
does not know yet. The rest is in the
[known limits](docs/DESIGN.md#known-limits) of the design.

| Group | Settings |
|-------|----------|
| Identity and access | `MOTD`, `LAN`, `AllowNonSteamClients` (caution), `AdminPlayerIds`, `AdminLoginMaxAttempts`, `AdminLoginLockoutSeconds`, `BossSummonCooldownSeconds`, `LastWipe` |
| World rules | `HarvestRate`, `XPRate`, `TamingRate`, `TamingBiteRate`, `HatchRate`, `MatureRate`, `MatingRate`, `CareRate`, `ImprintRate` (0.1 to 1000), `DayLengthMinutes` (5 to 240), `StructureDecay`, `StructureDecayMultiplier` (0.1 to 10), `PvP`, `PvPStructureDamage`, `OfflineRaidProtection`, `ORPActivationMinutes` (0 to 1440), `ORPStructureDamageTaken`, `ORPTameDamageTaken` (0 to 1), `ORPTurretDamageMultiplier` (0 to 10), `Difficulty` (0, 1, 2), `DeathDropMode` (`Everything`, `InventoryOnly`, `Nothing`) |
| Chat | `ChatLog`, `ChatFilter` (`Off`, `Mask`, `Drop`), `ChatBlockedWords` |
| Announcements and reports | `AnnouncementsUrl`, `AnnouncementsPollSeconds` (at least 10), `ProblemReportUrl`, and the two tokens `AnnouncementsToken` and `ProblemReportToken` |

**World rules apply only when the world is first created.** The game keeps
them with the world from then on, so a change reaches only a new world (for
example under a new world name); an existing world keeps its saved rules.
`LastWipe` is not a world rule: it is read at every start.

The two tokens are secrets: set them like the passwords (below), never as
rules. A list (`AdminPlayerIds`, `ChatBlockedWords`) is items separated by
commas, each without white space, `=`, `)` or a double quote; ESM passes
it with `, ` between the items (also a list stored before), so the server
reads it whole. The two addresses are base addresses, without a query
(and so without `=`). The join password reaches the server as
`-Password=` in the compatibility mode. `max_players` is 1 to 70, the most
the game server takes; an instance stored with more by an earlier version
keeps its value (and a warning) until it is changed. Rules and extra
arguments that override the engine's configuration files (`-ini:...`,
`-iniFile=`, `-CustomConfig=`), or that make it read more of its command
line from a file (`-CmdLineFile=`), are refused. The full table is in
[docs/DESIGN.md](docs/DESIGN.md#game-settings).

## Server passwords

An instance has a server password (to join) and an admin password, and may
have the game's announcements and problem report tokens. They are set only
through the API, `PUT /api/v1/instances/<name>/secrets` (which the page's
password and token fields and `esm secret <name> <which>` use), kept in a
private file, and never shown. Rules (`--set`) and extra arguments (`--arg`)
are shown to everyone who may look at an instance, so ESM refuses names
there that look like a secret (`JoinPassword`, `RconKey`, `AuthToken`, ...).
The tokens travel exactly like the passwords, in the file or on the command
line.

**By default the passwords travel in a file.** Before each start ESM writes
them to `instances/<name>/run/server-secrets.json` (readable by the ESM
account only), tells the server where the file is, and removes the file
when the server has stopped. No password is on a command line, so none is
in the process list or in what the server logs, and there is nothing ESM
would have to hide. This needs a game server that reads the file. A server
build that cannot do that runs without passwords and tokens; ESM flags that on the
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
running server was started with; the server is not told about it.) In
this mode a password or token is a value on the command line like any
other and follows every rule in [Game settings](#game-settings): no `=`
and no double quote, printable characters and the plain space only, no
space at either end or before a `-`, a `-` before a letter only after a
letter or digit, without a space no `,` or `)`, and none of the names
the engine searches for. A secret that breaks one is refused when it is
set, and an instance that holds one is not switched to the mode; the
refusal names the secret and the rule, never the value or anything it
holds. (With the secrets file a password may be anything, as before.) The
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
  reverse proxy, set `behind_tls_proxy` so the cookie is also Secure, and
  list the name clients use in `public_hosts`: ESM answers only requests
  for `localhost` (and the loopback addresses) with its port, its listen
  address, and those names, so a page of another site cannot reach it by
  pointing its own name at your box.
  Tokens are never accepted in URLs.
- Requests and replies are bounded: headers 64 KiB, bodies 1 MiB, slow
  clients are cut off, a client that stops reading an event stream is
  dropped without holding anything up, and log tails are capped.
- Every call that changes something or tries to (whether it works, fails
  or is refused), and every call refused for lack of a role, is written to
  an append-only audit log. A read refused for a missing login (401) or a
  host name that is not ESM's (421) is not.
- Builds are checked against their manifest (SHA-256 of every file) before
  they are installed, archives are unpacked with path checks, downloads must
  be https with a known SHA-256, and installed files are read-only. A local
  build must be in the import folder (`<root>/imports`, or
  `build_import_dir` in `esm.json`); ESM reads no other local path for it.
- **A build runs with ESM's own rights.** A game server build is a program,
  and ESM starts it as its own account: it can read every file ESM keeps
  (the secrets files with users, tokens and server passwords) and reach the
  unix socket, which acts as admin. So only the admin role installs builds;
  the operator role cannot (it can still update an instance to a build an
  admin installed). Install only builds you trust.
- ESM only ever signals or stops processes it started itself (it checks the
  process start time and executable before touching a pid).
- A stop is one SIGINT; ESM never sends a second signal and never kills a
  game server on Linux, so a world save is never cut short by ESM.
- ESM never changes the firewall. It tells you which ports each instance
  uses; opening them is up to you.

Known limits are listed in [docs/DESIGN.md](docs/DESIGN.md#known-limits).

## Documentation

- [docs/DESIGN.md](docs/DESIGN.md): how ESM works and why.
- [docs/openapi.yaml](docs/openapi.yaml): the HTTP API.

## Development

```sh
gofmt -l .
go vet ./...
go test ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

CI runs these on Linux and Windows (gofmt and govulncheck on Linux only);
the release workflow runs govulncheck before it builds, and a known
vulnerability stops the release.

The tests build a fake game server (`cmd/esm-fakeserver`) and a fake
SteamCMD (`cmd/esm-fakesteamcmd`) and drive whole services through the API
and the command line. Tests that start systemd
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
