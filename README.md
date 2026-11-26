# pullgate

pullgate lets a Linux or Windows host change only through its own updates. The host
pulls a git repo by itself. It applies a new commit only after its owner
taps **Apply** on a Telegram question that names that exact commit. Then
the host applies it, with a health check and a rollback or a hold.

- **The gate lives on the host.** What runs, who may approve and which paths
  get a warning come from `/etc/pullgate`, never from the repo. A commit
  cannot change the gate.
- **One exact commit.** The question shows a short SHA at the top and the
  full SHA once, on its last line. Apply applies only that full SHA, even
  when a newer commit arrives in between.
- **No code from the new commit runs before Apply.** Optional `check` and
  `summary` commands read the commit as data, as an unprivileged user.
- **Only the listener holds the bot token.** It runs as its own user and
  opens no port on either platform. On Windows it runs inside the SYSTEM agent.
- **Forward only** by default. A force push or an older commit is refused.
- **Root never runs git in a directory another user can write.**

It runs on Linux with systemd and on Windows with Task Scheduler. It needs `git` and
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

## Install on Linux

On Linux, pullgate runs on amd64 or arm64. See [Windows](#windows) for Windows setup. There are three ways to get the
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

Then set it all up with **`pullgate bootstrap`**, as root, on a console of
the host. It is the one command after the install: nothing else is made
or pasted by hand.

```sh
pullgate bootstrap --instance config-repo --repo git@github.com:example/config-repo.git \
    --apply /usr/local/lib/config-repo/apply.sh
```

1. **Users, directories, units.** It makes the listener user `pullgate`
   and the check user of each instance (`pullgate-check-NAME`, or the
   `check_user` of an existing `NAME.toml`) when they do not exist:
   system users with their own group, no home and no shell. It makes
   `/etc/pullgate` and `/var/spool/pullgate` (root, 0755) and
   `/var/lib/pullgate` (root, 0711), and writes the units of this version
   to `/etc/systemd/system` (a changed unit is replaced; keep your changes
   in a drop-in), then runs `systemctl daemon-reload`.
2. Make a Telegram bot with BotFather first. Bootstrap asks for its token
   with `systemd-ask-password`: it does not show the token, does not keep
   it in the shell history, and does not take it as an argument. It checks
   the token with Telegram, and stores it as `/etc/pullgate/bot-token`
   (owner root, mode 0600). For automation, `--token-stdin` reads it from
   the first line of stdin instead.
3. **Pairing.** Bootstrap prints a one-time code of 8 digits, valid for 10
   minutes. Send it to the bot, in your **private** chat with it.
   Bootstrap takes your user ID and the chat ID from that message, and
   writes `/etc/pullgate/listener.toml`. Nobody types or pastes an ID, so
   a script that someone else wrote cannot plant one. A wrong code (5 of
   them stop it), a group chat or a timeout writes nothing. Stop
   `pullgate-listen.service` while bootstrap runs, if it is running.
4. For each `--instance NAME --repo URL` (and `--branch`, default `main`):
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
   It also makes the state and the spool of each instance, as a run does.
5. **Doctor, then the units.** It runs `pullgate doctor` (below). Only when
   doctor finds no problem, it enables `pullgate-listen.service` and
   restarts it (one start, so it reads the new `listener.toml`), and runs
   `systemctl enable --now` for `pullgate-apply@NAME.path`,
   `pullgate-run@NAME.timer`, `pullgate-check@NAME.path` (for `/check`)
   and `pullgate-update.timer` and `pullgate-update.path` (release notices
   and `/update`). A problem enables nothing: fix it, and run bootstrap
   again. Then it runs doctor again, with the units running; a problem
   there leaves the units running: fix it, and run `pullgate doctor`.

Each run writes `listener.toml` with the instances of that run only: give
every instance of the host each time. To add an instance to a host that
is paired already, add `--add`: no token, no pairing; it keeps the IDs and
the other instances.

**From a recipe.** When the project of an instance ships it as a recipe
(below), one command sets it up, with its scripts and its config:

```sh
pullgate bootstrap --recipe https://github.com/OWNER/REPO/releases/download/vX.Y.Z/FILE.tar.gz \
    --instance config-repo --repo git@github.com:example/config-repo.git
```

`listener.toml`, the token, the deploy key and the known hosts file are
pullgate's own files: no other config tool (for example Ansible, run by
an instance) may write them.

Every step logs to the journal: `journalctl -t pullgate`.

Bootstrap runs doctor itself. Run it again any time, as root:

<!-- install-test:doctor -->
```sh
pullgate doctor
```

**`pullgate doctor [NAME] [--telegram]`** prints one line per check: `ok`,
`note` (worth a look, not a problem), or `PROBLEM` with the fix in the same
line. It exits 1 on any problem. It checks that the units of this version
are installed and enabled and that the listener runs; that the configs,
the token file, the deploy key, the known hosts file and the programs are
safe; the owners and modes of the state and the spool directories, as the
kernel kept them; that the listener user can read the outbox (it tries it
with a test file); and that git can read the branch with the deploy key.
`--telegram` also asks Telegram whether the token works and the bot can
reach your chat (it sends no message). Run it after the install, after an
update, and whenever a question does not come. Before the first run, it
notes the directories that the first run makes.

**When a question does not come.** The listener writes a receipt for each
message it sends. A run logs `asked` only after the receipt. Without one,
it logs `sent` (the question is in the outbox), and the next run warns
`question not delivered` after 3 minutes. `pullgate status NAME` shows it
in the line `delivery:`. Run `pullgate doctor`, fix what it says, then
`pullgate status NAME --ask-again`.

To see the version of a binary, run `pullgate --version` or
`pullgate version`. Both print `pullgate X.Y.Z` and exit 0. A build from
the source without `-ldflags "-X main.version=..."` prints `pullgate dev`.

## Commands: `/help`

Send the bot `/help` to get the list of commands, the instances of this
listener, what Apply and Skip do, and the `pullgate status` commands for
the host. Any other text from you (not a command) gets one short reply:
"Not a command. Send /help for the list.", at most once a minute.
Messages from other users, or in other chats, get no answer.

When you type `/` in the chat with the bot, Telegram shows a menu of these
commands. The listener sets this menu at each start, only for your chat
(`chat_id`), and only when it differs. A menu that you set by hand in
BotFather for other chats is not changed. If Telegram refuses (for example
"chat not found" when you never wrote to the bot), the listener logs it
once and goes on; send `/help` once and restart `pullgate-listen.service`
to get the menu. If `chat_id` is a group, every member of the group sees
the menu, but only your taps and commands count. A tap on a menu entry sends the bare command: a command
that needs NAME, such as `/pause`, then answers with the list of
instances.

The command to ask again is `/ask_again` (Telegram allows no `-` in a
command). The old `/ask-again` still works.

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

## Status and pause: `/status`, `/pause`, `/resume`, `/ask_again`

- `/status` (all instances) or `/status NAME`: the mode, the applied
  commit, the last tip, the open question, paused or not and why, broken
  or not. It changes nothing. At most one answer per instance a minute.
- `/pause NAME`: pauses the instance. Nothing is fetched, asked or applied
  until you send `/resume NAME` (or root runs `pullgate status NAME --resume`). If it is
  paused already, the old pause stays as it is. A reason after NAME is not
  sent to the host.

- `/resume NAME`: lifts a pause that `/pause` set. Whoever holds the bot
  token can do that too: if you suspect the token, pause as root. Any other pause (one
  that pullgate set itself after a cut-off apply, or one that root set)
  stays: run `pullgate status NAME --resume` as root.
- `/ask_again NAME`: asks again about the newest commit, but only when its
  question expired or was lost. A commit that was skipped, failed, held or
  refused is not asked again from Telegram; root can, with
  `pullgate status NAME --ask-again`.

As with `/check`, only your user ID in your chat counts, and the listener
only writes a small request file; root does the work.

## Check the setup from Telegram: `/doctor`

`/doctor` (all instances) or `/doctor NAME` asks root to run the checks
of `pullgate doctor` that need no secret. The answer lists each problem
and each note with its fix, how many checks passed, and the summary. It
leaves out the checks that read a secret: the bot token, Telegram, and
git with the deploy key. For those, run `pullgate doctor` as root.

The answer never shows an IP address, a key or a token, the host name, a
git address, or a path outside the files of pullgate (`/etc/pullgate`,
`/var/lib/pullgate`, `/var/spool/pullgate`, `/usr/local/bin/pullgate`,
`/usr/local/lib/pullgate` and the `pullgate-*` units): such text shows as
`<ip>`, `<secret>`, `<host>`, `<address>` or `<path>`. Root answers at most
once a minute per instance. The checks run inside the sandbox of
`pullgate-run@NAME.service`, so they see what a run sees.

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

## Recipes

A project that uses pullgate can ship an instance as a **recipe**: its
scripts and a config template, as a `.tar.gz` in a GitHub release, with
`SHA256SUMS` and a build attestation, as pullgate's own releases have.
`pullgate bootstrap --recipe URL` sets an instance up from one (below);
`pullgate recipe fetch URL` only checks and unpacks it.

```
pullgate recipe fetch URL [--gh PATH] [--bundle NAME] [--signer-workflow PATH] [--dir DIR]
```

As root, it downloads `URL`, checks it, and unpacks it. The last line it
prints is the folder of the recipe.

- `URL` must be `https://github.com/OWNER/REPO/releases/download/vX.Y.Z/FILE.tar.gz`:
  no other host, no `latest`, no query.
- From the same release it gets `SHA256SUMS` (exactly one line for
  `FILE.tar.gz`, and the SHA-256 must match) and the attestation bundle:
  `FILE.sigstore.json` (`FILE` without `.tar.gz`), or the name of
  `--bundle NAME`.
- It runs `gh attestation verify` with `--repo OWNER/REPO`,
  `--cert-identity https://github.com/OWNER/REPO/.github/workflows/release.yml@refs/heads/main`
  and `--deny-self-hosted-runners`, and the signed statement must name
  `FILE.tar.gz` with its SHA-256. `--signer-workflow .github/workflows/NAME.yml`
  names another workflow of the same repo. No flag skips a check.
  gh must be 2.93.0 or newer, a file of root that only root can change:
  `/usr/bin/gh`, or `--gh PATH`. gh runs with no token and an empty config.
- Before it writes anything it prints the repo, the tag, the file, its
  SHA-256 and the signer identity.
- It unpacks into `/usr/local/lib/pullgate/recipes/OWNER/REPO/X.Y.Z/` (or
  `--dir DIR` instead of `/usr/local/lib/pullgate/recipes`), through a new
  temporary folder and a rename, with the record `.pullgate-recipe.json`
  (the SHA-256 of the tarball and of each file). A folder that is there
  already is used only if only root can write it and it matches its record.
- Limits: the tarball 64 MiB, one file 16 MiB, all files 64 MiB, 1000
  entries.

**The format.** The tarball holds only plain files and folders: no link,
device, absolute path, `..`, or setuid, setgid or sticky bit. A file with
an execute bit gets mode 0755, any other 0644; the owner is root. At its
root, the manifest `pullgate-recipe.toml`:

```toml
format = 1
version = "1.2.3"              # must be the tag of the release, without the v

[[instance]]
name = "example"               # the default instance name
config = "example.toml.in"     # the template of /etc/pullgate/NAME.toml
requires = ["python3"]         # programs that must be in the PATH of root
setup = ["setup"]              # optional: a program of the recipe
```

An unknown key, a name that is not an instance name, or a path that is
not a file in the recipe is refused. `version` is signed with the tarball,
the tag in the URL is not: a tarball of another release is refused. A program of `requires` is looked up
in `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`; a
missing one stops it, and nothing is written.

**The template** is the TOML of `/etc/pullgate/NAME.toml`, with these
placeholders only: `@NAME@` (the instance name), `@HOST@` (the host name),
`@REPO_URL@` and `@BRANCH@` (from `--repo` and `--branch`), and
`@RECIPE_DIR@` (the folder of the unpacked recipe, for the paths of its
programs). An example: [contrib/examples/config-file/](contrib/examples/config-file/)
(`pullgate-recipe.toml` and `config-repo.toml.in`).

```toml
host = "@HOST@"

[repo]
url = "@REPO_URL@"
branch = "@BRANCH@"

[commands]
check = ["@RECIPE_DIR@/check.sh"]
apply = ["@RECIPE_DIR@/apply.sh"]
```

A template may not set `mode`, `[repo] deploy_key`, `[repo] known_hosts`,
`[commands] check_user` or `[listener]`: pullgate writes `mode = "ask"`,
the deploy key, the host keys and the check user itself, so a recipe never
picks `auto` mode or a key. Every key must be in lower case. Another placeholder, or a value with a quote,
a backslash or a newline, is refused, and the result must be a valid
instance config. Comments of the template are not kept.

**Set up an instance from a recipe:**

```
pullgate bootstrap [--add] --recipe URL [--gh PATH] [--bundle NAME] [--signer-workflow PATH]
    [--replace-config] --instance NAME --repo URL [--branch B] [--known-hosts FILE]
    [--recipe-instance NAME] [--instance NAME2 ...]
```

It does all of `pullgate bootstrap` (Install, above), and:

1. It checks everything first, then fetches the recipe as `recipe fetch`
   does, and makes the config of each instance from its template. A
   failure writes nothing.
2. `--instance NAME` takes the recipe's instance of that name, or the
   only one; `--recipe-instance R` takes the instance R of the recipe
   under the name NAME. A recipe may hold more instances: give one
   `--instance` for each one to set up.
3. If `/etc/pullgate/NAME.toml` exists, it is kept, and bootstrap prints
   how the recipe's config differs from it. `--replace-config` writes the
   recipe's config instead, and keeps the old one as
   `NAME.toml.bak-YYYYMMDD-HHMMSS`. A kept config keeps its repo, deploy
   key, host keys and check user; `--repo` must be the same.
4. The recipe's `setup`, if it has one, runs once as root in the recipe
   folder, with only `PATH`, `LC_ALL`, `PULLGATE_INSTANCE` and
   `PULLGATE_RECIPE_DIR`, before `NAME.toml` is written. If it fails, no
   config is written and no unit is enabled.
5. It writes `/etc/pullgate/NAME.recipe`: the recipe, its version and its
   SHA-256 (not for a kept config that differs from the recipe's: then an old record is removed). `pullgate status NAME` shows it in the line `recipe:`.

With `--add`, the host must be paired already: bootstrap reads no token
and does no pairing. It prints the owner ID, the chat ID and the
instances that it keeps, and only adds the new names to `listener.toml`.
A `listener.toml` that fails the checks of the listener is refused.

**Update a recipe** (DESIGN.md section 21), as root:

```
pullgate recipe update NAME (--version X.Y.Z | --url URL) [--reinstall] [--gh PATH]
pullgate recipe use NAME X.Y.Z
```

`recipe update` fetches the new version with the same checks as the
first install, writes `NAME.toml` again from the new template with the
same values (repo, key, host keys, check user, host; the `mode` stays),
keeps the old one as `NAME.toml.bak-…`, and writes the record. It goes
**forward only**: an older version is refused, and the same one needs
`--reinstall`. The folder of the version before is kept until the next
update. The recipe's `setup` runs only at the bootstrap.

When an `apply` must run code of the new version before the switch (for
example the new playbook first), use the split form from the `apply`
itself: `pullgate recipe fetch URL` (the last line it prints is the new
folder), run what you need from there, then `pullgate recipe use NAME
X.Y.Z`. Both are safe while the `apply` holds the instance lock; the new
config counts from the next run. `recipe use` of the version in use does
nothing, so an `apply` that runs again does not fail.

**Test a recipe in your own CI.** Two options make an end-to-end test
possible with no network:

- `--gh PATH`: a fake gh. It is called as `gh --version` (it must print
  `gh version X.Y.Z …`) and as `gh attestation verify FILE --bundle FILE
  --repo OWNER/REPO --cert-identity ID --deny-self-hosted-runners --format
  json`. It must print the JSON of the real gh; pullgate reads
  `[{"verificationResult":{"statement":{"subject":[{"name":"FILE.tar.gz","digest":{"sha256":"…"}}]}}}]`.
  Like the real one, it must be owned by root and not writable by others.
- A release server on `http://127.0.0.1:PORT`: give the URL
  `http://127.0.0.1:PORT/OWNER/REPO/releases/download/vX.Y.Z/FILE.tar.gz`.
  The server serves `FILE.tar.gz`, `SHA256SUMS` and the bundle from the
  same folder. pullgate prints a loud warning. Only `127.0.0.1` is
  allowed, as for `--api-url`.

Both work for `pullgate bootstrap --recipe` too, with `--api-url
http://127.0.0.1:PORT` for a fake Telegram, or `--add` on a host that
your test has paired already.

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
check = ["/path/to/check"]       # optional; runs as check_user; a failure means no question;
                                 # exit 3 means "nothing to apply on this host": no question, no message
summary = ["/path/to/summary"]   # optional; runs as check_user; its output is in the question;
                                 # a line that starts with "WARN: " is shown at the top of the question
apply = ["/path/to/apply"]       # required; runs as root after Apply; the first 10 lines it
                                 # writes to $PULLGATE_REPORT_FILE are added to the applied/failed message
health = ["/path/to/health"]     # optional; runs as root after apply
check_user = "pullgate-check"

[watch]
paths = ["policy/**", "*.toml"]  # a warning for each changed path that matches

[limits]
questions_per_day = 10
approval_ttl = "12h"
max_failed_tries = 1
allow_older = false
settle = "0"           # e.g. "30m": ask only when the branch has not changed for this long
reask_after = "0"      # e.g. "24h": ask a skipped or expired commit once more after this time
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

## Windows

Windows 10 or Windows Server 2016 and newer on amd64 are supported. Install
Git for Windows and GitHub CLI for all users in Program Files. Use an
elevated Windows PowerShell console for the steps below. The boot task
runs under SYSTEM, so it keeps running after you sign out.

### Install

Download these three files from the same release into a private folder.

- `pullgate_X.Y.Z_windows_amd64.tar.gz`
- `pullgate_X.Y.Z.sigstore.json`
- `SHA256SUMS`

Replace `X.Y.Z` with the release version in the following commands. Run
them in the folder with the downloads. Verify the archive before running
the installer inside it.

```powershell
$version = 'X.Y.Z'
$archive = "pullgate_${version}_windows_amd64.tar.gz"
$bundle = "pullgate_${version}.sigstore.json"
$gh = Join-Path ([Environment]::GetFolderPath('ProgramFiles')) 'GitHub CLI\gh.exe'
& $gh attestation verify $archive --bundle $bundle --repo code6x/pullgate --cert-identity 'https://github.com/code6x/pullgate/.github/workflows/release.yml@refs/heads/main' --deny-self-hosted-runners
if ($LASTEXITCODE -ne 0) { throw 'Release verification failed.' }
tar.exe -xf $archive install.ps1
if ($LASTEXITCODE -ne 0) { throw 'Could not extract the installer.' }
.\install.ps1 -Version $version
```

The installer checks the checksum and attestation itself. It installs the
binary in `%ProgramData%\pullgate` and registers a disabled boot task.
The protected directory permits access only to SYSTEM and Administrators.

### Set up

Start a private chat with your bot. Have your bot token and numeric
Telegram user ID at hand. The chat ID in a private chat with the bot
equals your user ID.

For SSH, put the read-only deploy key and the pinned known hosts file in
the protected directory. Set each file's owner to SYSTEM or Administrators
and let its ACL inherit from the directory. For a local repository, put
the bare repo under the same protected directory and omit the SSH flags.
After writing to a local bare repo, check that Git-created files and
directories still belong to SYSTEM or Administrators and inherit the
protected ACL. A file owned by the signed-in user makes the fetch fail
even when that user is an administrator.

```powershell
$root = Join-Path $env:ProgramData 'pullgate'
$binary = Join-Path $root 'pullgate.exe'
$ownerId = [long](Read-Host 'Your numeric Telegram user ID')
& $binary setup --instance config-repo --repo 'ssh://git@git.example.com/config-repo.git' --deploy-key (Join-Path $root 'deploy-key') --known-hosts (Join-Path $root 'known-hosts') --apply 'C:\example\apply.ps1' --owner-id $ownerId --chat-id $ownerId
if ($LASTEXITCODE -ne 0) { throw 'Setup failed.' }
& $binary doctor config-repo
if ($LASTEXITCODE -ne 0) { throw 'Doctor found a problem.' }
Enable-ScheduledTask -TaskName pullgate
Start-ScheduledTask -TaskName pullgate
& $binary status config-repo
```

Setup copies the PowerShell script into the protected directory. It takes
the bot token without echo and stores it with machine-scope DPAPI. Keep
the bot token out of command arguments and config files.

The script runs only after a tap for the exact commit. It gets the same
`PULLGATE_*` variables as Linux, including `PULLGATE_TREE` and
`PULLGATE_SHA`. It receives any arguments from the `[commands] apply`
list after the absolute script path. Use `exit 0` for success and a
nonzero code for failure. See the [example script](contrib/windows/apply.ps1).
Windows supports ask and off modes and has no pre-approval check or
summary command. Its listener runs as part of the SYSTEM agent.

### Update or remove

To update, verify and extract `install.ps1` from the new release and run
the install commands again with the new version. Wait for any apply to
finish first. The installer replaces the executable and restarts a task
that was running. The config and state stay in ProgramData.

To remove the boot task and stop the agent, run `.\install.ps1 -Remove`.
The protected data, including the token and executable, stays in
`%ProgramData%\pullgate`. An administrator can delete that directory
after saving any data they want to keep.
