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
looks too and shows it on the Box page and in `GET /api/v1/box`
(`update_available`). It looks at most every 5 minutes, and only while
the box is in use: as long as a page or a `GET /api/v1/box` came since
the last look, it looks again when the result turns 5 minutes old, so a
page that comes within 5 minutes of the one before shows a result at
most 5 minutes plus one look old. Once no page has come since the last
look, it stops looking. After such a pause the first page shows the old
result, with when it was made, and starts a new look; the pages after
that look ended show the new one. Set `"update_check_every"` in
`esm.json` to look less or more often (a duration such as `"30m"`, at
least `"2m"`), and `"update_check": false` to turn the looking off.

To look at once, press **Check now** in the ESM version card of the Box
page (every role may, as it changes nothing; the button is there while
`update_check` is on), or call `POST /api/v1/box/release-check`, which
waits for the answer, at most 15 seconds, and returns the same release
status. One look runs at a time: a check while one runs waits for it,
and one within 60 seconds of the last look gets that result again
without asking GitHub.

GitHub answers at most 60 such lookups an hour from one address without
a login, and the self-update uses the same lookup, so ESM asks
sparingly: each look sends the ETag of the last answer, so an unchanged
release list comes back as a short 304 Not Modified. When GitHub refuses
a look as asked too often (403 or 429), the release check (the pages,
its looks in the background and Check now) asks nothing more until the
time GitHub names (`Retry-After`, or `X-RateLimit-Reset`; at least a
minute, at most an hour, 15 minutes when it names none).
`esm update-self`, the Box page's update button and `esm version --check`
still ask GitHub when someone runs them, and while GitHub asks to wait
they fail with its 403 or 429. The Box page says until when the check
waits, and Check now shows the last result meanwhile.

ESM never updates itself on its own, also not when a look found a newer
release. `esm update-self` (or the Box page's update button, admins only,
there once a look found a newer release) downloads the newest release
for this system, checks the signature with the built-in key and the
binary's hash against the signed list, and only then puts the new binary
in place; the old one stays next to it as `esm.previous` (to roll back by
hand, move it back; a service made by `esm service install` does that by
itself when the new ESM does not come up, see below). The page's button
(`POST /api/v1/box/self-update`) then ends the service so that its
service manager starts the new binary, and does that only when something
will: on Linux, when systemd runs ESM (the unit of `esm service install`).
Otherwise, and on Windows, the update is refused and nothing changes,
unless the caller sets `exit_anyway` (the page asks first); ESM then
updates, ends, and has to be started again by hand. After a restart the
page reloads itself once the new ESM answers, or says after a minute that
ESM did not come back. `esm update-self` on the command line replaces
the binary and ends nothing: restart the service yourself
(`systemctl --user restart esm`). Game servers keep running either way
and the new ESM adopts them.

One self-update runs at a time: while it runs it holds the lock file
`<binary>.update.lock` next to the binary, and another one, from the page
or the command line, is refused. A lock older than 15 minutes was left by
an update that did not finish and is taken over.

**A new ESM that does not come up is replaced by the old one.** On Linux,
when ESM runs as the service that `esm service install` writes, a
self-update (from the page or from `esm update-self`) is not final until
the new ESM has stayed up: the update writes `esm.update.pending` next to
the binary, and the service's unit runs a small shell script
(`esm.update.sh`, written by `esm service install` and kept up to date by
ESM, see below) before every start. While the pending file is there, the
script counts the starts; at the
3rd start it moves the new binary aside as `esm.failed`, moves
`esm.previous` back to `esm`, writes `esm.update.rolledback`, removes the
pending file and starts the old ESM instead. So the new ESM gets two
starts (systemd starts it again 5 seconds after it ends), and nobody has
to log in to the box. The script does not need the new binary to run, and
never keeps ESM from starting: with no update waiting it does nothing,
and without `esm.previous` it says so in the service log and leaves the
binary alone. A new ESM that has been up for 30 seconds removes the
pending file: the update worked. (Restarting the service three times
within 30 seconds of an update, by hand or otherwise, is counted like
three failed starts.) An update that was killed before its new binary was
in place leaves a pending file next to an `esm.previous` that is the
binary that is there already (the same file, or the same bytes): there is
nothing to put back, so the script drops the pending file at the next
start without changing a binary, and an ESM that finds the same at its
30 seconds does too. A binary put there by hand is then never rolled back
by an old mark.

The ESM that runs the update writes the pending file, so an update is
protected when the ESM it starts from has this rollback: the update to the
first release that has it is not, and the one after it is.

The old ESM that is started after a rollback reports it: one audit entry
(`box.self_update_rollback`, actor `system`), a warning in the service
log, `last_self_update` in `GET /api/v1/box` (`from`, `to`, `time`,
`rolled_back`), `esm status`, and a notice on the Box page. The report
stays until the next self-update puts a new binary in place; it needs no
acknowledging. `esm.failed` is the binary that did not come up, for
whoever wants to find out why. A rollback puts the old program back and
nothing else: files the new ESM changed (`esm.json`, for one) stay as it
left them. On Windows nothing changes: no pending file is written, there
is no unit, and a failed update is rolled back by hand (move
`esm.previous` back). On Linux without that unit (ESM started by hand, or
a unit without the script) the pending file is written but nobody
counts the starts, so that is the same.

A unit written by an earlier ESM has no script. To get the rollback, write
the unit again and let systemd read it (see "Linux: run ESM as a
service"): `esm service install --force`, then
`systemctl --user daemon-reload && systemctl --user restart esm`.

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

A token has a role. `read` looks at everything but tokens, logins, the audit
log and backup downloads. `operator` also starts, stops, restarts, updates
(to a build that is installed), backs up and restores servers and downloads
backups. `admin` does everything, installing builds included. `site` is for
the token of a website's admin panel, and only a token has it (a login
cannot): see [A token for a website's admin panel](#a-token-for-a-websites-admin-panel).

### Logins and passwords

`esm user list` shows the logins: name, role, last login and how many
sessions are open.

A login changes its own password on the web page, on the Box screen. The
form asks for the current password and the new one twice, and the new one
follows the same rules as at setup (12 characters or more, at most 1024
bytes). The change ends the login's other sessions; the one that made it
goes on. A wrong current password counts as a failed login (5 in 5
minutes, then logins wait too), so the form is no faster a way to guess a
password than the login. An API token cannot change a password.

A forgotten password is set anew on the box, as ESM's account:

```sh
esm user passwd admin
```

It asks for the new password twice and shows nothing of what is typed; the
terminal's echo comes back also when the command is ended while it asks
(Ctrl-C, and on Linux Ctrl-\ or a hang-up). From a script, give it as one
line on standard input (`esm user passwd admin < new-password.txt`). It
asks only when standard input is a terminal or a Windows console: under
mintty, the terminal window of Git Bash, standard input is a pipe, so the
command asks nothing, reads one line, and mintty shows what is typed;
there, use a Windows console (PowerShell or cmd), or pipe the password in.
It is never taken from the command line or the environment, where other
accounts on the box could read it, and an error never repeats what was
given there. This needs no old password because it works only over the
unix socket, which only ESM's account can open; the same call over the
network is refused for every login and token. It ends every session of the
login, and the new password logs in at once, also after failed logins of
that name. Every change and reset is in the audit log, never the password.

### Reach the API from another machine

A control panel, a bot or a website on another machine or in a container
reaches the API through the **API listener**: a second listener, off by
default, TLS only, for API tokens. Add it to `esm.json` and restart ESM
(game servers keep running):

```json
"api_listener": {
  "listen": "192.0.2.10:8743",
  "host_names": ["esm.example.com"]
}
```

- `listen` is one address of the box and a port; there is no default.
  Every address (`0.0.0.0`, `[::]`) needs `"all_addresses": true` as well,
  and then `host_names` must name what clients use.
- `host_names` are the names (or addresses) clients use for it, with or
  without a port. It answers to those and to its listen address, nothing
  else (421).
- Without `tls_cert` and `tls_key` (your own certificate and key, set
  together; a relative path is taken from the root), ESM makes a
  self-signed certificate once and keeps it in `<root>/secrets/`. It makes
  a new one when a host name or address is added (taking one out changes
  nothing) or 30 days before the ten years are up, and keeps the key, so
  the public key pin stays the same.
- It takes API tokens only, each with its role
  (`esm token create --role read --name panel`; `site` for a website, see
  below). A browser login, setup
  and the web page are refused there unless you set `"page": true`; calls
  that work only on the box (`esm user passwd`) stay refused.

The certificate's two fingerprints are in the start log, on the Box
screen, in `GET /api/v1/box`, and here:

```sh
esm tls
# API listener on 192.0.2.10:8743 with TLS (API tokens only): certificate made by ESM, self-signed,
# valid until 2036-11-12, file /opt/durinn/secrets/api-listener.crt; SHA-256 D7:70:0C:...:67:07;
# public key pin sha256//TSL59PnSHoe+qcdInJbWNR7e6DLKMKCrVzfyzpGDrTk=
```

A client pins the public key. The pin alone checks the key; with a copy
of the certificate file as the only certificate it trusts, the client
checks the certificate and the host name too:

```sh
PIN='sha256//TSL59PnSHoe+qcdInJbWNR7e6DLKMKCrVzfyzpGDrTk='
curl -k --pinnedpubkey "$PIN" -H "Authorization: Bearer $ESM_TOKEN" https://192.0.2.10:8743/api/v1/instances
curl --cacert api-listener.crt --pinnedpubkey "$PIN" -H "Authorization: Bearer $ESM_TOKEN" https://esm.example.com:8743/api/v1/instances
esm --url https://192.0.2.10:8743 --pin "$PIN" list
```

`GET /api/v1/instances` gives a panel every server in one call: state,
build, ports, display name, and the player count in `live.players`, which
is null (with the reason in `live.unknown`) while the game server does not
report it. Login limits, request limits and the audit log are as on the
main listener. On both, an address with 60 failed authentications (a wrong
token, no token) within a minute gets 429 for every call without a valid
token; the limit lifts once the oldest failure of the last minute is a
minute old (calls refused meanwhile count as failures too). Valid tokens
and logins from that address go on working, and the audit log records
only a couple of entries of it a minute.
The API listener also ends the connection after each 401 or 429. See
[The API listener](docs/DESIGN.md#the-api-listener) for the details.

### A token for a website's admin panel

A website that manages ESM through the API listener, while it signs its
own employees in (with single sign-on, say) and decides what
each may do, calls ESM with one token of the `site` role:

```sh
esm token create --role site --name panel
```

It may do what an `operator` may (look at everything, start, stop, restart,
update to an installed build, back up, restore, download a backup, read
logs and events, and looking for a newer ESM release, as every role may),
and four calls more:

- change what a panel needs of an instance (`PATCH /api/v1/instances/<name>`):
  `display_name`, `max_players`, `listed`, `rules` (the world rules),
  `auto_update`, `update_max_wait`, `autostart`, `restart_on_crash`,
  `schedule`, `backup` (the policy), `map`, `world`, `cluster` and
  `channel`. A call that names any other field (`extra_args`, `limits`,
  `bind_ip`, `ports`, `secret_delivery`, `build`, or one added later) is
  403 naming it, and changes nothing: those reach the server's command line
  or the box, and stay the admin's. To change the rules without
  overwriting a change made meanwhile, send the call with `If-Match` (see
  [Changing a server's settings without overwriting a
  change](#changing-a-servers-settings-without-overwriting-a-change));
- install a build from a Steam source of `esm.json`, and only from one:
  `{"source": "steam:<name>"}` (`POST /api/v1/builds`). A folder, an archive
  or an https address is a program that runs with ESM's own rights, and
  gets 403;
- remove the builds nothing uses (`POST /api/v1/builds/prune`);
- update ESM itself (`POST /api/v1/box/self-update`), but not with
  `"exit_anyway": true`, which ends ESM with nothing to start it again: ESM
  never stays down because of the site (403).

Everything else is 403 for it: tokens, logins, the audit log, creating and
deleting instances, passwords and tokens of a server, deleting a build, the
health check. Its role is not a step between `operator` and `admin`: a call
that needs `admin` stays closed to it unless it is one of the four. It looks
at [jobs](#long-calls-as-jobs-for-a-panel) like every role, and cancels the
installs from a Steam source, which it may start.

The audit log records each call that changes something with the id of the
token (`esm token create` prints it; `GET /api/v1/tokens` lists ids with
names). To have it name the person the website acts for, send their name in
`X-ESM-Acting-User` (up to 64 characters, no control characters, once):

```sh
curl --cacert api-listener.crt --pinnedpubkey "$PIN" -X POST \
  -H "Authorization: Bearer $ESM_TOKEN" -H "X-ESM-Acting-User: jane@example.com" \
  -H "Content-Type: application/json" -d '{}' \
  https://esm.example.com:8743/api/v1/instances/alpha/restart
```

The entry then has `"actor": {"kind": "token", "id": "tok_...", "role":
"site", "acting_user": "jane@example.com"}`. ESM does not check the name and
decides nothing by it: it only records what the token says, and only from
a `site` token (from any other caller the header is not looked at). A name
that is too long or has a control character is refused with 400, and the
call is not made. The website must keep the token to itself and enforce its
employees' rights itself; whoever has the token has the site role.

### Long calls as jobs (for a panel)

An update, a backup, a restore, the install of a build (a Steam download can
take long) and ESM's own update run inside their request, and answer when
they are done. A panel that wants to show how far one is, or that must not
lose it to a dropped connection, starts it as a **job**: the same call with
`?async=true`. ESM checks the call as it always does (the same 4xx answers,
at once), starts the job, and answers 202 Accepted with the job and its
address in `Location`:

```sh
curl --cacert api-listener.crt --pinnedpubkey "$PIN" -i -X POST \
  -H "Authorization: Bearer $ESM_TOKEN" -H "Content-Type: application/json" \
  -d '{"to": "1.1.0"}' \
  "https://esm.example.com:8743/api/v1/instances/alpha/update?async=true"
# HTTP/1.1 202 Accepted
# Location: /api/v1/jobs/job_3f9a1c07b2e44d8a91c0e5f2
# {"id":"job_3f9a1c07b2e44d8a91c0e5f2","kind":"update","target":"alpha","state":"running","step":"", ...}
```

The job runs with a context of its own: the client may go away, and ESM goes
on with it. It is read with `GET /api/v1/jobs/<id>` (or, newest first,
`GET /api/v1/jobs`, which takes `?state=running` and `?target=alpha`), and
every change of it is also an event of type `job` on
`GET /api/v1/events`. A job has

- `kind` (`update`, `backup`, `restore`, `install` or `self_update`) and
  `target` (the instance, the source of the build, or `esm`);
- `actor`, who started it, as the audit log records the call (a token's id and
  role, and the person a site token said it acted for);
- `state`: `running`, then `done`, `failed` or `cancelled`;
- `step`, the step it is in, and `steps`, the steps it went through with
  `started` and `ended` (and, for a download, `bytes`, `total` and
  `percent` as far as the server or SteamCMD tell them);
- `started` and `ended`;
- when it is `done`, the `result`: what the call answers without
  `async` (the status of the instance, the backup, the build, or the result of
  the self-update); when it `failed`, the `error`, with the `code` and
  `message` of an error answer.

The steps, as far as they apply (a server that is stopped has no `stop`):

| Job | Steps |
|-----|-------|
| `update` | `backup`, `stop`, `switch_build`, `start`, `wait_ready`; then `rollback` if the new build did not get ready (the job `failed`) |
| `backup` | `wait_save` (of a running server), `copy`, `prune` |
| `restore` | `stop`, `backup_current`, `put_in_place`, `start`, `wait_ready` |
| `install` | `download` (a web address or Steam), `unpack`, `verify`, `place` |
| `self_update` | `verify_signature` (the release is looked up and its signed list of hashes checked), `download`, `place_binary`, `restart` |

A self-update job ends `done` when the new binary is in place and ESM is
about to end; ESM then ends, and its jobs with it, so the caller looks at
`GET /api/v1/box` for the new version. What only the
release server can tell (ESM is the newest release already: `up_to_date`)
ends that job `failed`; the checks that need no release server (nothing to
start ESM again, an update that runs) are answered at once.

An install can be stopped while it downloads, unpacks or verifies, with
`DELETE /api/v1/jobs/<id>`: nothing has been put in place then, and the job
ends `cancelled`. Once it puts the build in place (step `place`), or for any
other job (an update, a backup, a restore and ESM's own update run to their
end), it is 409 `not_cancellable`. Whoever cancels needs the role that starts
the job: the admin for an install from a folder, an archive or an address,
and for one from a Steam source the site role too.

Roles are those of the call: starting a job needs what the call without
`async` needs (the site role's limits included: it installs from a Steam
source only, and sends no `exit_anyway`), and every role that reads looks at
jobs. A job carries what the audit log records of the call that started it,
and that is for the operator role and above (the site role included): the
read role gets the actor without its `id` and `acting_user` (its `kind` and
`role` stay), the `target` of an install as its kind (`folder`, `archive`, the
scheme and host of an address; a Steam source stays as it is), the result of a
self-update without the paths of the binary, and, of an install or a
self-update that failed, the `code` of the error with a text that names no path
(the text of such an error may name the binary of ESM and its update lock, the
import folder or SteamCMD), in `GET /api/v1/jobs`, in
`GET /api/v1/jobs/<id>` and in the `job` events, those asked for later with
`Last-Event-ID` too. The `acting_user` and the target are cut and scrubbed as
the fields of an audit entry are, for everybody. For an instance with
`secret_delivery: command_line` the read role gets the notice in place of the
reason and of the text of an error, as it does everywhere. The call without `async` is unchanged and makes no job. A job
that ends writes an audit entry `job.end` (the call that started it was
recorded when it was made). ESM keeps the last 100 jobs and those that ended
less than 24 hours ago, **in memory only**: they are lost when ESM ends, and a
job that ran then is not resumed (look at the instance or the builds to see
what became of it). At most 32 jobs run at once (429 `busy`). When ESM is
stopped it waits for the jobs that run, like it waits for requests, for up to
30 seconds, and then stops them.

`esm jobs` and `esm job <id>` show them on the box. The web page runs the
Overview's Update, Back up and Restore, and the Builds install, as jobs, and
says the step they are in.

### Changing a server's settings without overwriting a change

`PATCH /api/v1/instances/<name>` replaces the whole `rules` map with the one
it is sent. A panel that reads the rules, adds one and writes them all back
would overwrite a rule that someone set on the web page, or from another
panel, in between. To rule that out, every server has a `revision`: a
whole number, 1 when the server is made, that grows by 1 with every stored
change of its settings: a `PATCH` (also one that sets nothing), new
passwords or tokens, and a switch of the build by an update, a rollback or
an automatic update. `GET /api/v1/instances` and `GET
/api/v1/instances/<name>` show it as `revision`; the calls that give or
change one server also send it as the `ETag` header (`"7"`, a strong tag).
A `PATCH`, or a `PUT .../secrets`, sent with `If-Match: "<revision>"` is
made only if the server is still at that revision. That is the way for a
panel to change the rules safely:

1. **Read** the server (`GET /api/v1/instances/<name>`). Keep its `ETag` and
   its `rules`.
2. **Merge**: put your change into the rules you read, and keep all the
   others as they are (the map replaces the one stored).
3. **Write** the whole merged map with the `ETag` as `If-Match`.
4. On **412**, with the error code `changed`, someone changed the server's
   settings after step 1. Nothing was done, and the message says the
   revision it is at now. Go back to step 1: read, merge on what is there
   now, write. On 200 the change is made, and its answer has the new
   `revision` and `ETag`, so the next change can go on from it without
   reading again.

```sh
# 1. read: the ETag header and the rules
curl -si --cacert api-listener.crt --pinnedpubkey "$PIN" -H "Authorization: Bearer $ESM_TOKEN" \
  https://esm.example.com:8743/api/v1/instances/alpha
# HTTP/1.1 200 OK
# ETag: "7"
# {"name":"alpha", ..., "rules":{"HarvestRate":"2","MOTD":"Welcome"}, ..., "revision":7, ...}

# 3. write the merged map, if the server is still at revision 7
curl -s --cacert api-listener.crt --pinnedpubkey "$PIN" -X PATCH \
  -H "Authorization: Bearer $ESM_TOKEN" -H "Content-Type: application/json" -H 'If-Match: "7"' \
  -d '{"rules": {"HarvestRate": "2", "MOTD": "Welcome", "PvP": "True"}}' \
  https://esm.example.com:8743/api/v1/instances/alpha
# 412: {"error":{"code":"changed","message":"the settings of instance alpha changed since they were read: they are at revision 8 now, and this change was made for revision 7; read them again and make the change anew"}}
```

- Without `If-Match` a call is made as before (the last one wins), and
  `If-Match: *` means any revision. `If-Match` is read on these two calls
  only; it must be `*` or one revision in double quotes, and anything else
  (a list, a weak tag, the header twice, a number without quotes) is 400
  `bad_if_match`.
- The check and the change are made under the one lock of the server, so of
  several changes made for the same revision at the same time exactly one
  is made and the others are 412.
- A `site` token is held to its fields first: a body with a field it may
  not change is 403 whatever `If-Match` says, and the answer is the same
  whether the revision matches or not. Then the form of `If-Match` is
  checked (400), then the instance (404) and what is wrong with the
  request whatever the instance is (a `build` field, the length of a
  password: 422), then the revision (412), then the change itself against
  the instance (422 or 409).
- The web page's settings form sends the revision it showed, so two admins
  in two tabs cannot overwrite each other: the second save is refused with
  a notice that the settings changed meanwhile, and a button that loads
  them again. The command line does not send `If-Match`: `esm secret` and
  `esm update` make their change as they always did, and each adds 1 to the
  revision, so a panel that read before it is told with 412.
- Going back to an older release keeps working, also by the rollback of a
  self-update that did not come up: `instance.json` has no new field, and
  the revision is kept in a file of its own beside it,
  `instances/<name>/revision`, which an older ESM does not look at (none
  is revision 1). What an older ESM changes is not counted in it, so after
  going back and forth a panel reads again before it changes anything.

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
(`Restart=always`, `RestartSec=5`, `StartLimitIntervalSec=0`,
`ExecStart=<this esm binary> serve --root <root>`) and runs nothing else.
It uses `KillMode=process`: stopping or restarting ESM never stops game
servers. Lingering (above) keeps the unit running after you log out; ESM
prints the command but does not run it.

The unit also has `ExecStartPre=-/bin/sh <esm binary>.update.sh <esm
binary>`: the script `esm service install` writes next to the binary
(`esm.update.sh`, replaced when it differs), which puts the old binary
back when a self-updated ESM does not come up (see "Releases and
updates"). A self-update replaces the binary only, so ESM also rewrites
the script at every start when it is there next to the binary and
differs from the one built into that ESM; it never makes one that is not
there, so a box that does not run as the unit stays clean, and the update
brings the script its new binary expects. The `-` makes systemd ignore
its failure, so it can never keep
ESM from starting, and `StartLimitIntervalSec=0` keeps systemd from
giving up on a start that keeps failing before the script gets to act.
If the script cannot be written (a folder ESM cannot write to), the unit
is still written and the command says so; only the rollback is then
missing.

**An existing unit has to be written again to get the rollback.** A unit
that an earlier ESM wrote has no `ExecStartPre`. Write it again, then let
systemd read it and restart ESM (game servers keep running):

```sh
esm --root ~/durinn service install --force
systemctl --user daemon-reload
systemctl --user restart esm
```

(Without `--force`, `esm service install` refuses to replace a unit that
differs from the one it would write.) Also do this again if the binary
moves to another folder, as the unit names the binary's path.

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
   `esm steam list` shows what each source last did (the Steam tab of the
   Builds screen on the page shows the same, and an admin can check and
   install from it); `esm doctor` checks SteamCMD, the sources and the
   passwords.

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
`behind_tls_proxy`); the [API listener](#reach-the-api-from-another-machine)
serves the page too with `"page": true`. The first visit asks for the setup code and creates
the admin login; after that the page is the login. A forgotten password is
set anew on the box with `esm user passwd` (see
[Logins and passwords](#logins-and-passwords)).

It shows the box (memory, CPUs, disk, ports, firewall commands, health
checks, tokens, and the form that changes your password), every server
(state, players, memory against its ceiling, settings, live log, backups,
timed restarts), builds, clusters and the audit log, and it starts, stops,
restarts, updates, backs up, restores, installs builds and changes
settings. An update, a backup, a restore and an install run as
[jobs](#long-calls-as-jobs-for-a-panel): the page says the step they are in,
and an install can be cancelled while it downloads. Each screen shows only what the API returns;
where ESM cannot know something yet (CPU use per server, the player list,
whether players can reach the box from outside), the page says so. A
server's settings screen has a field for every game setting ESM knows by
name (see [Game settings](#game-settings)), grouped, each with a one-line
help and, under "More", the rest of it with its range and default; an
empty field leaves the setting to the game. Saving is made on the revision
the screen showed (see [Changing a server's settings without overwriting a
change](#changing-a-servers-settings-without-overwriting-a-change)): when
the settings were changed meanwhile, nothing is saved and the screen says so,
with a button to load them again. A server's Overview has an
Update card (its build, the newer one when its Steam source has one, and
one status: up to date, update available, waiting, failed, or a new build
checked at the next start), and the Dashboard shows the same status for
every server. The Builds screen lists the builds newest first, says plainly
whether a server runs each one, keeps it for a rollback or nothing uses it
(and how much space removing the unused ones frees), installs from a
folder, an archive or a Steam source, and lists the Steam sources as
`esm steam list` does. A server's Backups screen shows when the last
backup was made, when the next is due, how many are kept and their size,
and lists them by day with the reason as a tag. Reading works without JavaScript; changes need it. What each role
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
esm jobs --state running                     # the long calls that a panel or the page started
esm job job_3f9a1c07b2e44d8a91c0e5f2         # one job: its steps and how long each took
esm stop alpha
esm list --json
esm build install steam:main                 # a new build of a Steam source, if there is one
esm steam list
esm version --check                          # is a newer ESM out?
esm update-self                              # put it in place (signature checked)
esm user list                                # the logins and their last login
esm user passwd admin                        # a new password for a login (asked twice)
esm tls                                      # the API listener and the pins of its certificate
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

The server name (`display_name`, or the instance's name when it is empty)
goes to the server twice, with the same value: as `-ServerName=` and as
`-SteamServerName=`. The game's Steam session code takes the name it
gives Steam from `-SteamServerName=`, not from `-ServerName=` (only
without the switch does it fall back to its engine configuration), and
cuts a name of 64 characters or more to 63, with a warning in its log;
Steamworks limits a game server name to 64 bytes including the
terminator. So a display name set or changed has at most 63 bytes of
UTF-8 (63 characters of plain ASCII), and so at most 63 characters,
which the game does not cut. Both switches are passed the same way and
follow the rules below once, the name counts twice in the length of the
command line, and neither switch can be set by a rule or an extra
argument. A new name reaches a running server at its next start. No real
server has run with `-SteamServerName=` yet: that a name with a space
arrives whole there (it does for `-ServerName=`) and that the name then
shows in the public server list are not seen yet. A display name of 64
bytes, which an earlier version took, is passed as stored and may be
cut: the game cuts it to 63 when it has 64 characters (all of one byte),
and what Steam shows for one with characters of more than one byte is
not known. It may be sent back unchanged, but not changed to another
name of more than 63 bytes, and the instance carries the warning
`unsafe_stored_settings` until the name has at most 63 bytes.

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
  a switch), and with it every space and line or paragraph separator but
  the plain space; format characters (zero width spaces and joiners,
  direction marks, the soft hyphen, the byte order mark); private-use,
  surrogate and unassigned code points; and the en dash, which the server
  does not start with on its command line (write `-`). That goes by the
  class of a character, not by how it looks: a letter or symbol that
  shows as blank (U+3164, U+2800) is printable text and taken.
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
whole command line of an instance, with everything on it (the server
name counts twice), may have 12000 characters after a change: the
server's engine does not start with more than 16383, and what starts the
server may add to the line. A change that would make it longer is
refused (`command_line_too_long`).

A value stored by an earlier version is kept until it is changed. An
empty one (a rule, or an extra argument that ends in `=`) is left off the
command line when the server starts, and the service log says so; any
other that breaks the rules is passed as it is, and the instance carries
the warning `unsafe_stored_settings`, which names the settings (never
their values) to change. The warning also names a rule or an extra
argument stored earlier that names `-SteamServerName`: it is passed after
ESM's own, which the server reads, and the instance takes no other change
until it is removed. One exception: with an en dash anywhere on its
command line the server's engine stops at once, so ESM does not start a
server whose line would hold one (409 `command_line_unsafe`, which names
the setting, a password or token by its field, and never a value). An
instance whose stored command line is longer than 12000 characters
carries the warning `command_line_too_long`; beyond 16127 ESM does not
start its server, and says why (409 `command_line_too_long`): of the
16383 the engine starts with, ESM keeps 256 free for what a build's start
script may put on the line besides (the name of the game's project, say),
since it does not know what a build's script adds. This version passes
the server name a second time, so every command line is longer than an
earlier version made it, by ` -SteamServerName=`, the name and, for a
name with a space, its quotes (at most 84 characters): an instance whose
stored line was close to 12000 may now carry `command_line_too_long`, and
one close to 16127 is now not started. A shorter display name (it counts
twice), or shorter rules or extra arguments, bring it back.

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

**World rules apply at every start of the server.** The game applies the
rules set here each time the server starts, not only when it creates a
world. A rule left empty uses the game's default and replaces the value
saved with the world. `LastWipe` is not a world rule: left empty it keeps
the date saved with the world.

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
- A password of a login is changed only with the current one, by the
  login's own session (a wrong one counts as a failed login), or set anew
  on the box over the unix socket (`esm user passwd`); a change ends the
  login's other sessions, a reset all of them. No token changes one.
- Requests and replies are bounded: headers 64 KiB, bodies 1 MiB, slow
  clients are cut off, a client that stops reading an event stream is
  dropped without holding anything up, and log tails are capped.
- Every call that changes something or tries to (whether it works, fails
  or is refused), and every call refused for lack of a role, is written to
  an append-only audit log. A read refused for a missing login (401) or a
  host name that is not ESM's (421) is not. The call of a `site` token also
  names the person it says it acts for (`X-ESM-Acting-User`); that name is
  a note, never used to decide anything.
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
  admin installed), and the `site` role only from a Steam source you put in
  `esm.json`. Install only builds you trust.
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
