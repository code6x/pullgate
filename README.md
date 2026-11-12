# pullgate

pullgate lets a Linux host change only through its own updates. The host
pulls a git repo by itself. It applies a new commit only after its owner
taps **Apply** on a Telegram question that names that exact commit. Then
root applies it, with a health check and a rollback or a hold.

- **The gate lives on the host.** What runs, who may approve and which paths
  get a warning come from `/etc/pullgate`, never from the repo. A commit
  cannot change the gate.
- **One exact commit.** The question shows the full SHA. Apply applies only
  that SHA, even when a newer commit arrives in between.
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
binary prints. It needs `curl`, `tar`, `gzip` and GNU coreutils. Without
`--gh`, it downloads a pinned gh and checks its SHA-256. It replaces the old
binary only in the last step, and restarts nothing: it prints what to
restart. Run it again to update. `--reinstall` installs the same version
again, or replaces a build of unknown version. Exit codes: 0 done or nothing
to do, 1 failure (nothing was installed), 64 wrong usage. To get the
script of one release, put its tag (`vX.Y.Z`) in the URL instead of `main`.
To start it with a clean environment, run it as
`env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin sh /root/install.sh`. What it does and
what it trusts: [DESIGN.md](DESIGN.md) section 16 and
[THREAT-MODEL.md](THREAT-MODEL.md) T17.

**By hand.** Download a release and check it as
[docs/releasing.md](docs/releasing.md) says (section "Verify a download").
Then, as root: `install -m 0755 pullgate /usr/local/bin/pullgate`.

**From the source**, with Go 1.26 or newer. Then install it as above:

```sh
CGO_ENABLED=0 go build -trimpath -o pullgate ./cmd/pullgate
```

Then, as root, make the users and the directories, and copy the units from
[contrib/systemd/](contrib/systemd/):

```sh
useradd --system --no-create-home --shell /usr/sbin/nologin pullgate
# One check user per instance, never the listener user:
useradd --system --no-create-home --shell /usr/sbin/nologin pullgate-check-config-repo
install -d -m 0755 /etc/pullgate /var/spool/pullgate
install -d -m 0711 /var/lib/pullgate
cp contrib/systemd/* /etc/systemd/system/
```

1. Make a Telegram bot with BotFather. Put its token in
   `/etc/pullgate/bot-token` (owner root, mode 0600).
2. Find your Telegram user ID and the chat ID of your private chat with the
   bot. Write `/etc/pullgate/listener.toml` from
   [contrib/examples/listener.toml](contrib/examples/listener.toml).
3. Make a **read-only** deploy key for the repo:
   `ssh-keygen -t ed25519 -N '' -f /etc/pullgate/NAME.key`. Add the public key
   to the repo as a read-only deploy key.
4. Pin the host key of the git server in `/etc/pullgate/NAME.known_hosts`.
   Check the fingerprint against the one your git host publishes.
5. Write `/etc/pullgate/NAME.toml` (owner root, mode 0600). Start from one of
   the examples below.
6. Start it:

```sh
systemctl daemon-reload
systemctl enable --now pullgate-listen.service
systemctl enable --now pullgate-apply@NAME.path pullgate-run@NAME.timer
```

Every step logs to the journal: `journalctl -t pullgate`.

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
  tree. If the rollback fails too, the instance is paused.
- `on_failure = "pause"`: no rollback. The failed commit is held and not
  tried again. A newer commit is still asked, with a warning at the top, so
  you can fix it forward.

`pullgate status NAME` shows the state. As root:
`pullgate status NAME --pause` stops the instance,
`pullgate status NAME --resume` starts it again, and
`pullgate status NAME --ask-again` asks about the tip again.

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
| `pullgate-apply@.service` | 7.9 EXPOSED | it runs your `apply` command as root, which must change the system. Add a drop-in for what your `apply` needs (`systemctl edit pullgate-apply@NAME.service`) |

The run unit sets `MemoryDenyWriteExecute=yes`, which also holds for
`check` and `summary`. A command that needs a JIT (for example Node.js) does
not work there.

## Development

```sh
go test -race ./...
go vet ./...
golangci-lint run --build-tags e2e
go test -race -tags e2e ./test/e2e/...
```

The rules for changes are in [AGENTS.md](AGENTS.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
