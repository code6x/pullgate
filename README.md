# pullgate

pullgate lets a Linux host change only through its own updates. The host
pulls a git repo by itself. It applies a new commit only after its owner
taps **Apply** on a Telegram question that names that exact commit. Then
root applies it, with a health check and a rollback or a hold.

- **The gate lives on the host.** What runs, who may approve and which paths
  get a warning come from `/etc/pullgate`, never from the repo. A commit
  cannot change the gate.
- **One exact commit.** The question shows a short SHA at the top and the
  full SHA once, on its last line. Apply applies only that full SHA, even
  when a newer commit arrives in between.
- **No code from the new commit runs before Apply.** Optional `check` and
  `summary` commands read the commit as data, as an unprivileged user.
- **Only the listener holds the bot token.** It runs as its own user and
  opens no port.
- **Forward only** by default. A force push or an older commit is refused.
- **Root never runs git in a directory another user can write.**

It is a small Go program for plain Linux with systemd. It needs `git` and
`ssh`. No server, no web UI.

Read [DESIGN.md](DESIGN.md) for how it works and
[THREAT-MODEL.md](THREAT-MODEL.md) for what it protects and where it stops.

## How it works

```
timer ──> pullgate run NAME (root)
            fetch ─> read the commit ─> check, summary (check_user)
            ─> question into the outbox
                     │
pullgate listen (own user, holds the token) ─> Telegram ─> owner taps Apply
                     │
            approval (only a one-time code) into the inbox
                     │
path unit ─> pullgate apply NAME (root)
            looks the code up in its own state ─> apply ─> health
            ─> success: report; failure: rollback or hold, and report
```

## Install

pullgate runs on Linux, on amd64 or arm64. There are three ways to get the
binary.

**With the install script** (as root, on the host itself). Download the
script, read it, then run it:

```sh
curl --proto '=https' --tlsv1.2 -fsSL -o /root/install.sh https://raw.githubusercontent.com/code6x/pullgate/main/install.sh
less /root/install.sh
sh /root/install.sh [--version X.Y.Z] [--bin-dir DIR] [--gh PATH] [--reinstall]
```

It installs the newest release, or the one of `--version`, as
`/usr/local/bin/pullgate` (or `DIR/pullgate`). It installs nothing unless
every check passes: the SHA-256 in `SHA256SUMS`, the build attestation of
the release (with `gh attestation verify`), and the version that the new
binary prints. It needs `curl`, `tar`, `gzip`, `sed` and GNU coreutils. Without
`--gh`, it downloads a pinned gh and checks its SHA-256. It replaces the old
binary only in the last step, and restarts nothing: it prints what to
restart. Run it again to update, or update from Telegram with `/update`
(below). `--reinstall` installs the same version
again, or replaces a build of unknown version. Exit codes: 0 done or nothing
to do, 1 failure (nothing was installed), 64 wrong usage. To get the
script of one release, put its tag (`vX.Y.Z`) in the URL instead of `main`.
To start it with a clean environment, run it as
`env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin sh /root/install.sh`. What it does and
what it trusts: [DESIGN.md](DESIGN.md) section 16 and
[THREAT-MODEL.md](THREAT-MODEL.md) T17.

**By hand.** Download a release and check it as
[docs/releasing.md](docs/releasing.md) says (section "Verify a download").
Then, as root:

<!-- install-test:binary -->
```sh
install -m 0755 pullgate /usr/local/bin/pullgate
```

**From the source**, with Go 1.26 or newer. Then install it as above:

```sh
CGO_ENABLED=0 go build -trimpath -o pullgate ./cmd/pullgate
```

Then, as root, make the users and the directories, and copy the units from
[contrib/systemd/](contrib/systemd/):

<!-- install-test:setup -->
```sh
useradd --system --no-create-home --shell /usr/sbin/nologin pullgate
# One check user per instance, never the listener user:
useradd --system --no-create-home --shell /usr/sbin/nologin pullgate-check-config-repo
install -d -m 0755 /etc/pullgate /var/spool/pullgate
install -d -m 0711 /var/lib/pullgate
cp contrib/systemd/* /etc/systemd/system/
```

Then set it up with **`pullgate bootstrap`**, as root, on a console of
the host:

```sh
pullgate bootstrap --instance config-repo --repo git@github.com:example/config-repo.git \
    --apply /usr/local/lib/config-repo/apply.sh
```

1. Make a Telegram bot with BotFather first. Bootstrap asks for its token
   with `systemd-ask-password`: it does not show the token, does not keep
   it in the shell history, and does not take it as an argument. It checks
   the token with Telegram, and stores it as `/etc/pullgate/bot-token`
   (owner root, mode 0600). For automation, `--token-stdin` reads it from
   the first line of stdin instead.
2. **Pairing.** Bootstrap prints a one-time code of 8 digits, valid for 10
   minutes. Send it to the bot, in your **private** chat with it.
   Bootstrap takes your user ID and the chat ID from that message, and
   writes `/etc/pullgate/listener.toml`. Nobody types or pastes an ID, so
   a script that someone else wrote cannot plant one. A wrong code (5 of
   them stop it), a group chat or a timeout writes nothing. Stop
   `pullgate-listen.service` while bootstrap runs, if it is running.
3. For each `--instance NAME --repo URL` (and `--branch`, default `main`):
   it makes the deploy key on the host (`/etc/pullgate/NAME.key`; the
   private key never leaves the host) and prints the public key. Add it
   to the repo as a **read-only** deploy key. It pins the host key of the
   git server in `/etc/pullgate/NAME.known_hosts`: for `github.com` from
   the key that pullgate carries, with its fingerprint, to compare with the
   one GitHub publishes; for another git host from `--known-hosts FILE`
   (make it with `ssh-keyscan`, and compare the fingerprints). If
   `/etc/pullgate/NAME.toml` does not exist, it writes a minimal one with
   `--apply PROGRAM` (owner root, mode 0600); add `check`, `health` and
   `watch` as the examples below show. Then it waits until `git ls-remote`
   can read the branch.
4. Bootstrap does not enable the units yet. Do it as below.

Each run writes `listener.toml` with the instances of that run only: give
every instance of the host each time.

`listener.toml`, the token, the deploy key and the known hosts file are
pullgate's own files: no other config tool (for example Ansible, run by
an instance) may write them.

Start it:

<!-- install-test:start -->
```sh
systemctl daemon-reload
systemctl enable --now pullgate-listen.service
systemctl enable --now pullgate-apply@NAME.path pullgate-run@NAME.timer
systemctl enable --now pullgate-check@NAME.path   # optional: /check
systemctl enable --now pullgate-update.timer pullgate-update.path   # optional: release notices, /update
```

Every step logs to the journal: `journalctl -t pullgate`.

To see the version of a binary, run `pullgate --version` or
`pullgate version`. Both print `pullgate X.Y.Z` and exit 0. A build from
the source without `-ldflags "-X main.version=..."` prints `pullgate dev`.

## Commands: `/help`

Send the bot `/help` to get the list of commands, the instances of this
listener, what Apply and Skip do, and the `pullgate status` commands for
the host. Any other text from you (not a command) gets one short reply:
"Not a command. Send /help for the list.", at most once a minute.
Messages from other users, or in other chats, get no answer.

## Check now: `/check`

The timer runs every 5 minutes. After a push you do not have to wait: send
the bot `/check` (all instances) or `/check NAME` (one instance). The
listener answers "Checking NAME…" and writes a check request.
`pullgate-check@NAME.path` then starts the normal run at once. You get the
usual Apply question, or "No new commit (the applied commit is …)", or
"NAME is paused" / "NAME is off" (then nothing is fetched).

- Only your user ID in your chat counts. The text must be exactly
  `/check` or `/check NAME`. Other messages get no request. Edited messages
  do not count.
- `/check` never applies anything in `ask` mode: it only fetches and asks.
  **In `auto` mode, `/check` applies a new commit at once, without a tap**,
  as the timer would a few minutes later.
- Root allows at most one run per minute from `/check`. More requests in
  that minute are dropped, with one reply. The listener also takes at most
  one `/check` per instance in 10 seconds.
- `pullgate-check@NAME.path` stops after 10 starts in a minute. Then
  `/check` waits for the timer until you run
  `systemctl reset-failed pullgate-check@NAME.path` and
  `systemctl start pullgate-check@NAME.path`.
- Without `pullgate-check@NAME.path`, the request waits for the next timer
  run.

## Status and pause: `/status`, `/pause`, `/resume`, `/ask-again`

- `/status` (all instances) or `/status NAME`: the mode, the applied
  commit, the last tip, the open question, paused or not and why, broken
  or not. It changes nothing. At most one answer per instance a minute.
- `/pause NAME`: pauses the instance. Nothing is fetched, asked or applied
  you send `/resume NAME` (or root runs `pullgate status NAME --resume`). If it is
  paused already, the old pause stays as it is. A reason after NAME is not
  sent to the host.

- `/resume NAME`: lifts a pause that `/pause` set. Any other pause (one
  that pullgate set itself after a cut-off apply, or one that root set)
  stays: run `pullgate status NAME --resume` as root.
- `/ask-again NAME`: asks again about the newest commit, but only when its
  question expired or was lost. A commit that was skipped, failed, held or
  refused is not asked again from Telegram; root can, with
  `pullgate status NAME --ask-again`.

As with `/check`, only your user ID in your chat counts, and the listener
only writes a small request file; root does the work.

## Update pullgate: `/update`

Once a day `pullgate-update.timer` looks for a new release of pullgate. For
each new release that passes the checks of `install.sh` (the SHA-256 and
the build attestation), the bot sends one notice: the version, the link to
the release notes, and "send /update to install it". A release that fixes
a security problem says so first, and its notice comes again each day
until it is installed. A notice never installs anything.

The first look runs within an hour after the timer starts, and makes the
directories that `/update` needs. To have it at once, run
`systemctl start pullgate-update.service`.

To install it, send the bot `/update`:

1. The listener answers "Looking for a new release of pullgate…".
2. Root checks the newest release again and sends a question with the
   version, the link to the release notes, the tarball for this CPU, and
   its SHA-256, with **Apply** and **Skip**.
3. On **Apply**, root installs only that tarball, with every check of
   `install.sh` done again. Then it installs the units of
   `contrib/systemd/` of the new version where they differ from yours,
   runs `systemctl daemon-reload`, re-enables and restarts the units, and
   restarts the listener last.
4. The new listener sends the report: the old and the new version, the
   units it installed, and your drop-ins (`/etc/systemd/system/pullgate-*.d/`).
   Check that you still need each drop-in.

If the new listener does not stay active, root puts the old binary and the
old units back, and reports why. pullgate never installs an older version
from Telegram; to install one, run `install.sh --version X.Y.Z` by hand. A
build from the source (`pullgate dev`) does not update itself.

Keep your changes to a unit in a drop-in (`systemctl edit UNIT`): an
update replaces the unit files. A unit that you disabled stays disabled,
and a masked unit is left alone.

The settings, in `/etc/pullgate/listener.toml` (all optional):

```toml
[updates]
check = false               # no daily look and no notices; /update still works
listener_user = "pullgate"  # the user of the listener (default "pullgate")
gh = "/usr/bin/gh"          # the gh that checks the attestation, as install.sh --gh
```

To use `/update` on a host that runs an older version: install this
version with `install.sh`, copy the units of `contrib/systemd/` again, run
`systemctl daemon-reload`, and enable the two new units:
`systemctl enable --now pullgate-update.timer pullgate-update.path`.
How it works: [DESIGN.md](DESIGN.md) section 15; what it trusts:
[THREAT-MODEL.md](THREAT-MODEL.md) T20.

## Config

The instance config `/etc/pullgate/NAME.toml`:

```toml
mode = "ask"              # "ask" (default), "auto" (no question; for development hosts) or "off"
on_failure = "rollback"   # "rollback" or "pause"; the default is "pause"
host = "example-host"     # shown in each message

[repo]
url = "ssh://git@git.example.com/owner/config-repo.git"
branch = "main"
deploy_key = "/etc/pullgate/NAME.key"
known_hosts = "/etc/pullgate/NAME.known_hosts"

[commands]
check = ["/path/to/check"]       # optional; runs as check_user; a failure means no question
summary = ["/path/to/summary"]   # optional; runs as check_user; its output is in the question
apply = ["/path/to/apply"]       # required; runs as root after Apply
health = ["/path/to/health"]     # optional; runs as root after apply
check_user = "pullgate-check"

[watch]
paths = ["policy/**", "*.toml"]  # a warning for each changed path that matches

[limits]
questions_per_day = 10
approval_ttl = "12h"
max_failed_tries = 1
allow_older = false
```

The commands get these environment variables: `PULLGATE_SHA`,
`PULLGATE_OLD_SHA`, `PULLGATE_TREE` (a clean, read-only copy of the commit),
`PULLGATE_OLD_TREE` (the same for the applied commit),
`PULLGATE_CHANGED_FILES` (a file with the changed paths, each ended by a NUL
byte), `PULLGATE_FAILED_SHA`, `PULLGATE_ROLLBACK` and `PULLGATE_INSTANCE`.
The full list of keys and what they do is in [DESIGN.md](DESIGN.md),
sections 5 and 11.

**What happens when an apply fails** is your choice per instance:

- `on_failure = "rollback"`: pullgate runs `apply` again with the last good
  commit, then `health`. So `apply` must be safe to run again on an older
  tree. If the rollback fails too, the instance is **broken**: the host
  may be in a half state. pullgate tells you once, with the reason. Then it
  asks only for a fix forward (a newer commit on top of the failed one),
  with a warning, also in `auto` mode, and tries no rollback. A fix forward
  that works ends it, or root runs `pullgate status NAME --resume` after a
  look at the host.
- `on_failure = "pause"`: no rollback. The failed commit is held and not
  tried again. A newer commit is still asked, with a warning at the top, so
  you can fix it forward.

`pullgate status NAME` shows the state. As root:
`pullgate status NAME --pause` stops the instance,
`pullgate status NAME --resume` starts it again, and
`pullgate status NAME --ask-again` asks about the tip again. If a question
for the tip is still open (for example its Telegram message was lost), it
closes that question too: the next run sends a new question, and the Apply
button of the old one no longer works.

## Example 1: a config file and a service reload

A service reads one policy file from a private repo. pullgate checks the
file, shows a diff of it in the question, copies it into place and reloads
the service. A failed health check rolls back to the last good file.

Files: [contrib/examples/config-file/](contrib/examples/config-file/)

- `config-repo.toml`: `on_failure = "rollback"`, a watch on `policy.toml`.
- `check.sh`: the file must exist, be small and be plain text.
- `summary.sh`: `diff -u` of the old and the new tree.
- `apply.sh`: install the file and `systemctl reload`.
- `health.sh`: `systemctl is-active`.

## Example 2: ansible-playbook on the tree

A server runs `ansible-playbook` locally on the approved commit. Running an
older playbook again can do more harm than good, so this example holds a
failed commit instead of rolling back.

Files: [contrib/examples/ansible/](contrib/examples/ansible/)

- `playbooks.toml`: `on_failure = "pause"`, watches on `ansible.cfg`,
  `site.yml` and sudoers files.
- `apply.sh`: `ansible-playbook --connection local site.yml` in the tree.

There is no `check` here: `ansible-playbook --syntax-check` loads
`ansible.cfg` and plugins from the tree, and that is code from the commit
before Apply.

## The systemd units

[contrib/systemd/](contrib/systemd/) has the units. Their
`systemd-analyze security` scores (systemd 255, 2026-11-11; lower is
better):

| unit | score | why |
|---|---|---|
| `pullgate-listen.service` | 1.2 OK | own user, no capabilities, read-only system |
| `pullgate-run@.service` | 2.4 OK | root, but only the capabilities it needs to run `check` as `check_user`, to stop it on a timeout, and to fix the spool; read-only system |
| `pullgate-update.service` | not measured | it installs pullgate and its units as root, and restarts them, so its sandbox is light |
| `pullgate-apply@.service` | 7.9 EXPOSED | it runs your `apply` command as root, which must change the system. Add a drop-in for what your `apply` needs (`systemctl edit pullgate-apply@NAME.service`) |

The run unit sets `MemoryDenyWriteExecute=yes`, which also holds for
`check` and `summary`. A command that needs a JIT (for example Node.js) does
not work there.

### Update from 0.1.0

To update a host from 0.1.0: update the binary, and copy the units of
`contrib/systemd/` of the new version to `/etc/systemd/system/` again.
The copy replaces the unit files, so keep your own changes in drop-ins
(`systemctl edit UNIT`); `systemctl cat UNIT` shows them. Then do the two
steps below.

**The run unit (issue #19).** In 0.1.0 the first `pullgate run` failed
under `pullgate-run@.service` with
`chmod .../outbox: operation not permitted`. A host may have a drop-in
with `RestrictSUIDSGID=no` for the run unit. Newer versions do not need it.

1. Find the drop-in: `systemctl cat pullgate-run@NAME.service` shows each
   file and its path. It is in `/etc/systemd/system/pullgate-run@.service.d/`
   or in `/etc/systemd/system/pullgate-run@NAME.service.d/`.
2. Remove the line `RestrictSUIDSGID=no`, or the whole file when that is
   all it holds.
3. Run `systemctl daemon-reload`.

The next run takes the setgid bit off the outbox by itself.

**The listener at boot (issue #20).** In 0.1.0, `pullgate-listen.service`
had no `[Install]` section, so `systemctl enable` did nothing and the
listener did not start after a reboot. Every host updated from 0.1.0 must
enable it again. A host may have the workaround
`systemctl add-wants multi-user.target pullgate-listen.service`. That made
the same link that `systemctl enable` makes now
(`/etc/systemd/system/multi-user.target.wants/pullgate-listen.service`), so
there is nothing to remove first (systemd has no `remove-wants`). With or
without the workaround, run:

```sh
systemctl daemon-reload
systemctl reenable pullgate-listen.service
ls -l /etc/systemd/system/multi-user.target.wants/pullgate-listen.service
```

`reenable` removes the old link, if there is one, and makes it again from
the new `[Install]` section. The `ls` shows the link.

## Development

```sh
go test -race ./...
go vet ./...
golangci-lint run --build-tags e2e
go test -race -tags e2e ./test/e2e/...
```

CI runs the install commands of this README as written: each install
block above has an `<!-- install-test:… -->` comment before it, and
`test/units/full-loop.sh` runs those blocks as root, then a full loop
under the units. Keep that comment when you change a block.

The rules for changes are in [AGENTS.md](AGENTS.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
