# pullgate design

Status: accepted for version 0.1 on 2026-11-11.

pullgate lets a Linux host change only through its own updates. The host
pulls a git repo by itself. It applies a new commit only after its owner
taps **Apply** on a Telegram question that names that exact commit.

Read [THREAT-MODEL.md](THREAT-MODEL.md) next. It says what this design
protects, and where it stops.

## 1. Words

- **instance**: one repo that one host follows, with one config file
  `/etc/pullgate/NAME.toml`, one state directory and one lock. A host can
  have several instances. `NAME` matches `[a-z0-9][a-z0-9-]{0,31}`.
- **tip**: the commit that the configured branch points to after a fetch.
- **applied SHA**: the last commit that root applied with success. It is
  empty before the first apply.
- **held SHA**: the last SHA whose apply failed, while no newer apply has
  worked (section 10).
- **question**: one Telegram message that names one full SHA and has two
  buttons, Apply and Skip.
- **code**: a random one-time code of 128 bits (22 characters, base64url).
  The buttons carry it instead of the SHA.
- **approval**: a small file that the listener writes when the owner taps a
  button. It holds the code and the button (apply or skip). Nothing else.
- **tree**: a clean, read-only copy of the files of one commit, written by
  pullgate itself from git objects.
- **check request**: a small file that the listener writes when the owner
  sends `/check` or `/check NAME`. It holds no data from the message. It
  starts a normal run now, instead of at the next timer (sections 8 and 9).
- **pause file**: `/var/lib/pullgate/NAME/paused`. While it exists, the
  instance does nothing: no run, no apply.
- **update question**: a question that names one release of pullgate
  itself: its version and the SHA-256 of its tarball (section 15).

## 2. The rules

These rules come from issue #1. Each one has tests (section 14).

1. **The gate lives on the host, never in the repo.** What runs, who may
   approve and which paths get a warning come only from the host config:
   `/etc/pullgate/NAME.toml` and `/etc/pullgate/listener.toml`. pullgate
   never reads any setting from the repo. A commit cannot change the gate.
2. **One exact commit.** The question shows the full SHA once, on its last
   line. Apply applies only that SHA, even when a newer commit arrives in
   between. The tap is bound to the question (its code), not to the text.
3. **No code from the new commit runs before Apply.** Before the tap,
   pullgate only fetches the commit and reads it with git. The optional
   `check` and `summary` commands may read the tree as data. They run as an
   unprivileged user (`check_user`), never as root, on a read-only tree.
4. **Only one process holds the bot token**: the listener. It runs as its own
   unprivileged user. It only connects out (`getUpdates` long-poll), it opens
   no port. It accepts a tap only from the configured owner user ID, in the
   configured chat, on an open question. All it can do is write an approval
   for that open question. The approval carries only the code. Root looks
   up the SHA in its own state. The only other thing it can write is a
   check request for `/check` of the owner. That request holds no data, and
   it only starts a normal run sooner (sections 8 and 9).
5. **Root does the apply.** It acts only on an approval whose code matches a
   question that root recorded itself, and only within the time limit of that
   question. By default it moves forward only: the new SHA must descend from
   the applied SHA. Going to an older or unrelated commit needs the explicit
   setting `allow_older = true`. Before the first apply there is no applied
   SHA, so any tip may be asked.
6. **Root never runs git in a directory another user can write.** Before
   every git command, root checks the git directory and every directory above
   it (section 6). git runs with hooks off and without system or global
   config. pullgate writes the tree itself from git objects: no filters, no
   submodules, no LFS.

## 3. Processes and users

| process | user | started by | holds |
|---|---|---|---|
| `pullgate run NAME` | root | a timer (`pullgate-run@NAME.timer`), or a path unit (`pullgate-check@NAME.path`) when a check request exists | the clone, the state, the deploy key |
| `pullgate apply NAME` | root | a path unit (`pullgate-apply@NAME.path`) when the inbox is not empty, or the auto approval exists | the state |
| `pullgate listen` | `pullgate` (own user) | a service (`pullgate-listen.service`) | the bot token, its list of open questions |
| `check`, `summary` | `check_user` (default `pullgate-check`) | `pullgate run` | nothing |
| `apply`, `health` | root | `pullgate apply` | what the owner's command needs |
| `pullgate status NAME` | root | the owner | nothing |
| `pullgate update` | root | a timer (`pullgate-update.timer`, daily), or a path unit (`pullgate-update.path`) for `/update` and a tap | the binary, the units, its own state (section 15) |

Root and the listener never talk directly. They use a spool directory
(section 8). Both units start the same `pullgate-run@NAME.service`, so a
run on `/check` has the same sandbox, the same lock and the same rules as a
run on the timer. systemd never starts one unit twice at once.

The run unit is sandboxed (read-only system, private `/tmp`, no new
privileges). The apply unit runs the owner's `apply` command as root, so it
can be less strict; each example sets what its `apply` needs.

## 4. Files

| path | owner, mode | what |
|---|---|---|
| `/etc/pullgate/NAME.toml` | root, 0600 or 0644 | the instance config |
| `/etc/pullgate/listener.toml` | root, 0640 group listener, or 0644 | the listener config |
| the deploy key | root, 0600 | read-only SSH deploy key |
| the known hosts file | root, 0644 | the pinned SSH host key of the git server |
| the bot token file | the listener user, 0600 (or a systemd credential) | the bot token |
| `/var/lib/pullgate/NAME/` | root, 0711 | the state directory |
| `/var/lib/pullgate/NAME/repo.git` | root, 0700 | the bare clone |
| `/var/lib/pullgate/NAME/state.json` | root, 0600 | the state (section 9) |
| `/var/lib/pullgate/NAME/lock` | root, 0600 | the lock (`flock`) |
| `/var/lib/pullgate/NAME/paused` | root | the pause file |
| `/var/lib/pullgate/NAME/auto-approval` | root, 0600 | the approval that `auto` mode writes (section 13) |
| `/var/lib/pullgate/NAME/trees/` | root, 0711 | the trees of one process (section 6) |
| `/var/spool/pullgate/` and `/var/spool/pullgate/NAME/` | root, 0755 | the spool |
| `/var/spool/pullgate/NAME/outbox` | root, group listener, 0770 (no setgid) | messages from root to the listener |
| `/var/spool/pullgate/NAME/outbox/*.json` | root, group listener, 0640 | one message |
| `/var/spool/pullgate/NAME/inbox` | listener, 0700 | approvals from the listener to root |
| `/var/spool/pullgate/NAME/check` | listener, 0700 | check, status and pause requests from the listener to root |
| `/var/spool/pullgate/NAME/receipts` | listener, 0700 | receipts from the listener to root: which outbox files it sent (section 8) |
| `/var/lib/pullgate-listener/state.json` | listener, 0600 | the listener's open questions |
| `/var/lib/pullgate/_update/` and `.../tmp/` | root, 0700 | the state, the lock and the downloads of `pullgate update` (section 15) |
| `/var/spool/pullgate/_update/` | as `NAME/` above | the outbox, inbox and check request of the updates |
| `/usr/local/bin/.pullgate.prev` | root | the old binary during an update (a hard link) |

The paths can be changed with `--config-dir`, `--state-dir` and
`--spool-dir`, for tests.

**Checks of the gate files.** pullgate refuses to start when the instance
config, the deploy key, the known hosts file, or the program (the first
word) of a command is a symlink, is not owned by root, or can be written by
group or others; or when a directory above one of them fails the check of
section 6. A symlink on the way to a program (such as `/bin -> usr/bin`)
is allowed only when it is owned by root and the directory that holds it
passes the check too. The deploy key must also not be readable by group or others. The
listener checks `listener.toml` and its token file the same way (the token
file must be owned by the listener user). In tests, "root" means the user
that runs the test.

**Files in directories that the listener can write.** Root never follows a
path that the listener can change. It opens the spool directories with
`os.OpenRoot` (no symlinks out of the directory), fixes their owner and mode
through the open directory, creates files in them with
`O_CREAT|O_EXCL|O_NOFOLLOW` under a random temporary name and renames them
in the same directory. `/var/spool/pullgate` and `NAME/` are owned by root
and pass the check of section 6, so the listener cannot swap `outbox`,
`inbox` or `check` for a symlink.

**No setgid bit on the outbox.** Root gives each outbox file the group of
the listener user itself: it creates the file, then calls `fchown` on the
open file (group only) and `fchmod` to 0640, all before the rename. So the
listener never sees an outbox file with another group, and no directory
needs the setgid bit. This matters because `pullgate-run@.service` sets
`RestrictSUIDSGID=yes`, which refuses every `chmod` that sets a setuid or
setgid bit. Version 0.1.0 made the outbox `2770`, and its first run failed
under the unit (issue #19). The other choice was to remove
`RestrictSUIDSGID=yes` from the run unit. We keep it: the run unit runs as
root with `CAP_FOWNER`, and it must never make a setuid or setgid file.
`Ensure` also takes the setgid bit off an outbox made by 0.1.0. Removing a
bit is allowed under `RestrictSUIDSGID=yes`.

**Read back after set.** Each time root sets an owner or a mode (the spool
directories in `Ensure`, each outbox file in `Send`), it reads them back
with `fstat` on the same open file, and fails when they differ: "the
kernel did not keep the owner or the mode that pullgate set on … Fix: run
pullgate doctor". The run ends with this error before it writes a
question, so it never logs `asked` for a question that the listener
cannot read (issue #19: the kernel cleared a bit without an error). Because root is not in the
listener's group, the `fchown` needs `CAP_CHOWN`: every unit that runs
`pullgate run` or `pullgate apply` must keep it. Without it, root cannot
write to the outbox, and the owner gets no question and no reply.

## 5. The instance config

```toml
mode = "ask"              # "ask" (default), "auto" or "off"
on_failure = "rollback"   # "rollback" or "pause"; the code default is "pause"
host = "example-host"     # shown in each message; default: the host name

[repo]
url = "ssh://git@git.example.com/owner/config-repo.git"
branch = "main"
deploy_key = "/etc/pullgate/NAME.key"
known_hosts = "/etc/pullgate/NAME.known_hosts"

[commands]
# argv lists, no shell. The first word is an absolute path.
check = ["/usr/local/bin/check-config", "--strict"]   # optional
summary = ["/usr/local/bin/config-diff"]              # optional
apply = ["/usr/local/sbin/install-config"]            # required
health = ["/usr/local/sbin/health-check"]             # optional
check_user = "pullgate-check"
check_timeout = "5m"
apply_timeout = "30m"

[watch]
# A warning in the question for each changed path that matches.
# "*" matches inside one path part, "**" matches any number of parts.
paths = ["policy/**", "*.toml"]

[limits]
questions_per_day = 10    # default 10
approval_ttl = "12h"      # default 12h, at most 7 days
max_failed_tries = 1      # default 1; counts only with on_failure = "rollback"
max_files = 100000        # default 100000 files in one tree
max_tree_bytes = 1073741824  # default 1 GiB in one tree
allow_older = false       # default false (rule 5)
refuse_symlinks = false   # default false; true refuses a commit with a symlink that points out of the tree
settle = "30m"            # default 0 (ask at once); ask only when the tip has not changed for this long, at most 168h
reask_after = "24h"       # default 0 (never); ask a skipped or expired tip once more after this time (ask mode)

[listener]
user = "pullgate"         # the user of the listener; owns the inbox
```

Unknown keys are an error. Every key must be written in lower case
(`a-z`, `0-9`, `_`; `Mode` is an error, in every config of pullgate:
section 20). An unknown mode or `on_failure` is an error. A
missing `apply` is an error. Every error names the file, the key and the fix.

There is no default for `on_failure` in the example configs: each example
sets it. The code default is `pause`, because it is the safe choice.

**The repo URL.** `ssh://…` and `user@host:path` use SSH (section 6). A path
that starts with `/`, or a `file://` URL, is a local repo. root runs git in
it too (`git-upload-pack`), so a local repo must pass the same check as the
clone (section 6): owned by root, nobody else can write it. Other URLs
(`https://`, `git://`, relative paths) are refused. The key and known hosts
paths may hold only `A-Za-z0-9._/-`, because git gives `GIT_SSH_COMMAND` to
a shell.

## 6. Git, safely (rule 6)

Before each git command, root checks the **git directory**:

- The git directory and each directory above it up to `/` must be owned by
  root (the running user) and not writable by group or others.
- One exception for a directory above: it may be writable by others when it
  is owned by root and has the sticky bit (like `/tmp`). Then no other user
  can rename or remove our directory in it.
- Every file and directory inside the git directory must be owned by root,
  must not be writable by group or others, and must be a regular file or a
  directory (no symlink, no special file).

If a check fails, pullgate stops with an error that names the path, and runs
no git.

Each git command runs:

- with an empty environment, plus `PATH`, `HOME` set to the state
  directory, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`,
  `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=`, `LC_ALL=C` and
  `GIT_SSH_COMMAND` (below);
- with these settings before the subcommand: `core.hooksPath=/dev/null`,
  `core.fsmonitor=false`, `protocol.allow=never`, `protocol.ssh.allow=always`,
  `protocol.file.allow=always` (for local repos, see section 5),
  `submodule.recurse=false`, `fetch.recurseSubmodules=false`,
  `core.attributesFile=/dev/null`, `transfer.fsckObjects=true`,
  `gc.auto=0` and `maintenance.auto=false`;
- with `--git-dir` set, never in a work tree;
- in its own process group, with a timeout that kills the group, and a cap
  on its output.

The fetch is:

```
git fetch --no-tags --no-recurse-submodules --no-write-fetch-head \
  --no-auto-gc -- URL +refs/heads/BRANCH:refs/pullgate/tip
```

`GIT_SSH_COMMAND` is `ssh -F /dev/null -i KEY -o IdentitiesOnly=yes
-o IdentityAgent=none -o UserKnownHostsFile=KNOWN_HOSTS
-o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes
-o BatchMode=yes`. The URL comes from the host config. It must not start
with `-`, so git cannot read it as an option. The branch must be a plain
name (`[A-Za-z0-9][A-Za-z0-9._/-]*`, no `..`).

Then `git rev-parse --verify refs/pullgate/tip^{commit}` gives the tip.
pullgate keeps `refs/pullgate/applied` on the applied SHA, so git never
loses it after a force push. No gc runs.

The other reads are `git merge-base --is-ancestor`, `git rev-list --count`,
`git log -z --no-mailmap --format=%s`, `git diff-tree -r -z --no-renames
--name-only` and `git ls-tree -r -z`.

**The tree.** pullgate never uses `git checkout` or `git archive`, because
both apply `.gitattributes` (filters, `export-subst`, `export-ignore`). It
reads `git ls-tree -r -l -z --full-tree SHA` and then each blob with
`git cat-file --batch`. It writes the files itself:

- Each path part must not be empty, `.`, `..` or `.git` (in any case), and
  must not hold a NUL. A bad path stops the export.
- Regular files are written 0444 (0555 when the git mode is 100755), with
  `O_CREAT|O_EXCL|O_NOFOLLOW`, inside directories that pullgate made in this
  export. A repeated name, or a path below a file or a symlink of the tree,
  stops the export.
- Symlinks (mode 120000) are written as symlinks. pullgate never follows a
  symlink of a tree. A symlink whose target is absolute or holds `..` gets a
  warning in the question; with `refuse_symlinks = true` it stops the
  export. A command that reads the tree must not follow symlinks blindly.
- Submodules (mode 160000) are skipped and listed in the question as a
  warning.
- The export stops when the tree has more than `max_files` entries or more
  than `max_tree_bytes` bytes.
- The export goes to `trees/.tmp-RANDOM`. Only when it is complete does it
  get its final name. Then each directory gets mode 0555, and the top
  directory gets 0750 with group `check_user`.

**Fresh trees.** A tree belongs to one process. `pullgate run` exports the
trees for `check` and `summary`; `pullgate apply` exports fresh trees for
`apply`, `health` and a rollback. Each process removes its trees when it
ends, and removes trees that an earlier process left (after a crash) when it
starts. So a tree that a command changed, or a half-written tree, is never
used again.

## 7. One run (`pullgate run NAME`)

1. Read the config. Take the lock (`flock`, no wait). If another run or
   apply holds it, log "busy" and exit 0; but when a check request waits,
   wait for the lock instead (section 8). If the pause file exists, log
   "paused" and exit 0. If `mode = "off"`, log "off" and exit 0. When a
   check request waits, take it first (section 8); then paused and off
   send a reply ("NAME is paused" or "NAME is off"), and do not fetch.
2. If the state shows an apply in progress (section 10), write the pause
   file, report, and exit.
3. Make sure the clone and the spool directories exist with the right owners
   and modes (section 4). Fetch (section 6).
4. Expire: each question in the state whose time is up is removed. If it was
   open, root asks the listener to edit it ("expired") and marks its SHA
   expired.
5. Decide:
   - tip = applied SHA: nothing to do. If a question is open, close it
     ("the branch is back to the applied commit").
   - tip = the SHA of the open question: nothing to do.
   - tip is the held SHA, or was asked before and the question expired or
     was skipped, or its check failed, or it was refused, or its check
     said "nothing to apply" (step 7), or it failed `max_failed_tries`
     times: nothing to do. `pullgate status NAME --ask-again` clears this
     mark, and also closes an open question for the tip (section 13).
   - **`reask_after`** (default 0, off): in `ask` mode, a tip marked
     skipped or expired is asked
     once more when its mark is at least `reask_after` old and it is still
     the tip. Asking sets the mark to asked; a second Skip or expiry sets
     a new mark time, so the next ask comes only after the next wait. It
     counts against `questions_per_day` like every question. `auto` mode
     has no Skip, so `reask_after` does nothing there.
   - **`settle`** (default 0, off): root keeps in its state the time it
     first saw the tip (`tip_since`). A new tip, or a clock that went back
     before that time, starts it again. While the tip is younger than
     `settle`, the run logs "settling" and stops: no check, no question,
     no notice. So three merges in ten minutes give one question, for the
     last one, and a writer cannot keep moving the branch to make the
     owner tap without reading. It also holds in `auto` mode. The time is
     root's own: the commit date is written by the writer and is not
     trusted. An open question for an older tip stays open while the new
     tip settles: it is closed only when the new tip is asked (step 9), as
     today while the new tip is checked. A state file from a version
     before `settle` has no `tip_since`; the first run sets it to now, so
     the tip it knew waits one settle time (fail closed).
   - tip is not a descendant of the applied SHA (an older commit, a force
     push, a rewritten branch) and `allow_older` is false: refuse. Send one
     notice for this SHA and mark it refused. When `allow_older` is true,
     ask, with a warning line at the top: "This commit is not a descendant
     of the applied commit." With no applied SHA, every tip is allowed.
   - the questions in the last 24 hours reached `questions_per_day`: log it,
     send one notice per day, and do not ask.
6. Export the tree of the tip, and the tree of the applied SHA
   (`PULLGATE_OLD_TREE`).
7. Run `check` as `check_user` (section 11). If it exits with status
   **3**, the commit has nothing to apply on this host (for example only
   docs, or files for other hosts): mark the SHA `nothing`, send no
   message, and do not ask. The applied SHA stays as it is, so the next
   question still lists every commit since the last real apply, and its
   check and summary still compare with the last applied tree. In the
   broken state (section 10) exit 3 is a failed check with its notice,
   so the owner sees that a fix forward was not asked. Any other
   non-zero status, a timeout or a signal is a failed check. If it fails, mark the SHA
   "check failed", send a notice with the end of its output, and do not
   ask. The same SHA is not checked again. In that output the tree paths
   (`PULLGATE_TREE`, `PULLGATE_OLD_TREE`) become `NEW` and `OLD`. The
   notice keeps the last lines, at most 10 and at most 1500 characters,
   so the reason (most tools print it last) is shown. A line longer than
   300 characters keeps its start and its end, with `…` between.
8. Run `summary` as `check_user`, and build the message (section 12).
9. If a question is open, close it: root marks it superseded in its state,
   and asks the listener to edit it ("a newer commit replaced this
   question"). A tap that the listener took before the edit still counts
   for the old SHA (rule 2); section 9 says how.
10. `ask`: make a new code, record the question in the state (synced to
    disk) and write the question to the outbox. `auto`: record the question
    in the state the same way, but instead of a message, write the code to
    the auto approval file (section 13).

## 8. The spool

Each instance has `/var/spool/pullgate/NAME/outbox` and `.../inbox`.

**Outbox** (root writes, the listener reads and removes). One JSON file per
message, named `SEQ.json` with a 20-digit sequence number (nanoseconds),
written to a random `.tmp-*` name and renamed. The listener handles them in
name order and skips names that start with `.`. Kinds:

- `ask`: `question` (an id, unique per instance), `code`, `expires`
  (time), `text` (HTML, ready to send);
- `edit`: `question`, `text`: replace the text of that question and remove
  its buttons. The listener forgets the code of the question;
- `notice`: `text`: a new message;
- `reply`: `question`, `text`: a reply to that question, or a new message
  when the listener does not know the question.

A file the listener cannot read or parse is removed and logged. The owner
also gets one notice per round for such files: "an outbox message of
instance NAME could not be read and was removed", with the command
`pullgate status NAME --ask-again`. The notice shows no part of the file,
because it may be a question that the owner never saw. If Telegram refuses
a message, the listener tries again on the next round, and drops it after
one day with a log line.

**Receipts** (the listener writes, root reads and removes). For each
outbox file it handled, the listener writes `receipts/NAME`, where NAME
is the name of the outbox file: `sent\n` when Telegram took the message,
`failed\n` when it removed the file without a send (it could not read or
parse it, Telegram refused it for good, or it was a day old). It writes
the receipt before it removes the outbox file. A receipt holds nothing
from the message. The spool of updates (`_update`) has no receipts.

Root keeps the outbox file name of each question in its state
(`"outbox"`), and reads the receipts (`TakeReceipts`): through
`os.OpenRoot` with `O_NOFOLLOW`, a regular file owned by the listener
user, at most 16 bytes, one of the two contents, at most 1000 per run.
Each file is removed; a bad one is logged. Then:

- After it writes a question, `pullgate run` waits up to 20 seconds for
  its receipt. `sent`: it logs `asked`, result `asked`. `failed`: the run
  fails, result `not-delivered`, with the fix (`pullgate doctor`, then
  `--ask-again`). No receipt yet: it logs `sent: the question is in the
  outbox; no receipt from the listener yet`, result `sent`.
- Each later run first takes the receipts. A `sent` receipt of a question
  logs `asked` then; a `failed` one logs the warning `question not
  delivered`. An open question with no receipt 3 minutes after it was
  asked gets the warning `question not delivered: no receipt from the
  listener`, once.
- `pullgate status` and `/status` show the line `delivery:` for an open
  question: `sent`, `waiting for the listener`, or `question not
  delivered` with the fix. It reads a receipt that no run took yet, and
  leaves it.

A receipt only changes log lines and the status. It is never an approval,
and root never acts on a commit because of it.

**Inbox** (the listener writes, root reads and removes). One file per tap,
`SEQ.json`, written to a `.tmp-*` name and renamed, at most 1 KiB, with
`{"code": "...", "action": "apply" | "skip"}`. Unknown fields are an error.
Root opens each file through `os.OpenRoot` with `O_NOFOLLOW`, needs a
regular file owned by the listener user, reads at most 1 KiB, and removes the
file before it acts. A bad file is logged and removed. Root removes `.tmp-*`
files older than one minute.

`pullgate-apply@NAME.path` watches the inbox (`DirectoryNotEmpty=`) and the
auto approval (`PathExists=`), and starts `pullgate apply NAME`. The apply
service has a start limit, so a stuck file cannot start it in a loop.

**Check requests** (the listener writes, root reads and removes). The
listener writes `/var/spool/pullgate/NAME/check/request` for `/check`
(section 9): a `.tmp-*` file renamed to that one fixed name, so many
requests before root reads them are one file. The content is always the
six bytes `check\n`. It holds nothing from the message: not the text, not
a SHA, not a code. A check request can only start a normal run. It is not
an approval, and `pullgate apply` never reads this directory.

`pullgate-check@NAME.path` watches the files `check/request`,
`check/status`, `check/pause` and `check/doctor` (`PathExists=`; `resume`
and `ask-again` wait for the timer; another file there starts
nothing) and starts `pullgate-run@NAME.service`, the unit of the timer. `pullgate run`,
with the lock held (a run that had to wait for the lock, and then finds
no request, stops: another run took the request):

1. Reads and removes every file in the directory, with the checks of an
   approval: through `os.OpenRoot` with `O_NOFOLLOW`, a regular file owned
   by the listener user, at most 64 bytes. It must also have the name
   `request` and exactly the content `check\n`. Root removes each file
   before it acts. A bad file is logged and removed; it is not a request.
   A run that finds only bad files goes on as a normal (timer) run, with
   no reply and no mark against the limit.
2. **The limit**, kept by root in its own `state.json` (`check_at`), not
   by the listener: at most one run per minute (`CheckInterval`) starts
   from a check request. When the last one is less than a minute away
   (before or after, so a clock jump back of less than a minute does not
   pass it), the run removes the request and exits without a fetch. It
   sends one reply per minute: "A check ran less than a minute ago, so this
   /check was dropped." (`check_note`).
3. Paused or off: reply so, do not fetch.
4. Else the normal run (section 7). After it, a reply says what it found,
   unless the question or a notice says it already: "No new commit. The
   applied commit is SHA.", "the question for SHA is still open", "SHA was
   handled before (mark)", "the last apply of SHA failed and it is held",
   or, in `auto` mode, "New commit SHA … it is applied now, without a tap"
   (section 13). A run that fails sends "The check failed. See the journal
   of the host", without the error text.

**A request while the lock is held.** A run started by the timer exits
"busy" when another process holds the lock. A run that sees a check request
waits for the lock instead (`flock` with a wait), so the request is not
lost and the path unit does not start the run again and again. The lock
makes sure that two runs, or a run and an apply, never run at once. The
run unit's `TimeoutStartSec` caps the wait.

**The start limit.** Each check request starts the run unit. The path unit
has a trigger limit (`TriggerLimitBurst=10` in 60 seconds), which stops a
loop when a run fails before it removes the request. If the limit is hit,
systemd stops the path unit until the owner runs `systemctl reset-failed
pullgate-check@NAME.path` and starts it again. The run unit has no start
limit (`StartLimitIntervalSec=0`), so many requests never stop the timer.
So that `/check` alone never hits the limit, the listener also writes at
most one request per instance in 10 seconds (`CheckGap`, kept in
memory). Root does not trust this: its own limit is the one above.

## 9. Questions and approvals

Root keeps its questions in `state.json`:

```
{ "id": 7, "code": "…", "sha": "…", "base": "…", "asked": time,
  "expires": time, "open": true, "outbox": "…", "delivery": "sent" }
```

`outbox` is the name of the outbox file of the question, `delivery` what
its receipt said (`sent`, `failed`, or none yet; section 8).

`base` is the applied SHA when the question was asked. At most one
question is open. A superseded question stays in the list (`"open": false`)
until it expires, so that a tap that the listener took before the close
still counts (rule 2). The listener forgets a code when it handles the
`edit` that closes the question, so it cannot take a tap for it after that.
Every list entry is removed when it is answered or expires.

**The listener** calls `deleteWebhook` at start and then `getUpdates` with
`allowed_updates = ["callback_query", "message"]`. It keeps the last 200
`update_id`s that it handled in its state file. An update with one of them
was handled before: it is ignored, a tap and a message alike. A lower ID
that is not in the list still counts, because Telegram may start the IDs
again at a random number after a quiet week. It accepts a tap only when all of
these hold, else it answers "This question is closed." and writes nothing:

- `from.id` is `owner_id`, and `message.chat.id` is `chat_id`;
- the data is `a:CODE` or `s:CODE`, and CODE is a code that it holds;
- the message ID is the message ID of that question;
- the message was not edited (no `edit_date`), and its text is the text
  that Telegram gave back when the listener sent it (a SHA-256 of it is
  kept). So a token thief who edits the text above real buttons gets no
  tap counted (THREAT-MODEL.md T1);
- the question has not expired by the listener's clock;
- the question has no tap yet. After the first tap the listener forgets the
  code, so a second tap does nothing.

Then it writes the approval, edits the question ("Apply tapped. Waiting for
the host." or "Skipped."), and answers the tap.

**`/status` and `/pause`** (code6x/pullgate#32). The same checks of the
sender, the chat and the edit hold, as for `/check`. The text is `/status`
(every instance), `/status NAME`, or `/pause NAME` (more words after NAME
are allowed and dropped: the reason never reaches root). The listener
writes a request file in the check directory: `status` or `pause`, with
exactly its name and a newline in it, and nothing from the message. The
same command for the same instance within a minute writes nothing, and
gets "asked less than a minute ago". Root
takes the requests in its run, as check requests (owned by the listener,
a regular file, the exact content; any other file is removed and does
nothing):

- `pause`: if there is no pause file, root writes one with the reason
  `paused from Telegram at TIME` (the prefix `state.TelegramPause`; no
  other reason starts with it). A pause file that is there is never
  changed, so that a pause that pullgate or root set keeps its reason. The
  pause is never rate-limited; its reply goes out at most once a minute.
  The file is made with a hard link, which fails when the name exists, so
  a pause that root writes at the same moment is never replaced.
- `status`: root sends a notice with the facts of `pullgate status`
  (section 13), cleaned and in `<pre>`, at most once a minute. It is
  read-only.
- Then the normal run goes on, so a stream of `/status` never stops the
  fetches. After a `/pause`, the run ends as paused.

**`/doctor` and `/doctor NAME`** (code6x/pullgate#50) go the same way, with
the request file `doctor`, as `/status`: `/doctor` writes one request for
every instance. Root, in `pullgate run NAME`, after it takes the request,
runs the checks of section 18 for NAME with `Public` set, at most once a
minute (`doctor_at` in the state), and sends one notice. Root runs them,
not the listener, because some checks read root-only owners and modes
and read the outbox as the listener user. `Public` leaves out every check
that reads a secret: the token file, Telegram, and `git ls-remote` with
the deploy key; each is a note that says to run `pullgate doctor` as
root. The notice holds only the `PROBLEM` and `note` lines, the number of
`ok` checks and the summary, after `doctor.Redact`: URLs and git
addresses become `<address>`, runs of 32 or more key characters and bot
token forms `<secret>`, IP addresses `<ip>`, absolute paths outside
pullgate's own files `<path>`, and the host name of the system (and its
first label) `<host>`. Then it is cleaned and cut as command output
(section 12), in `<pre>`: when it is too long, the first lines are cut
and the summary stays. A config that does not load is named with its
file only, as its error may quote a value of the config (the repo url,
`api_url`); `pullgate doctor` as root shows why. A known limit: in a path
with spaces that is not pullgate's own, the words between the spaces stay
(the parts with a `/` go). The files that pullgate makes have no spaces;
such a path can only come from a config value that the owner chose. Root
saves `doctor_at` at once, also when the instance is paused or off, so the
limit holds then too. The normal run goes on after it, as after `/status`.

**`/resume NAME` and `/ask_again NAME`** (code6x/pullgate#33) go the same
way, with request files `resume` and `ask-again`. They are limited, so
that they cannot undo a safety stop:

- `resume`: root lifts the pause only if the pause file is one that
  `/pause` wrote: a regular file of root, with one link, of exactly one
  line `paused from Telegram at YYYY-MM-DD HH:MM UTC`. A pause by pullgate
  itself (a cut-off apply), by root (`pullgate status --pause`) or with
  any other text stays, and the reply says to run `pullgate status NAME
  --resume` as root. `/resume` never ends the broken state (section 10).
- `ask-again`: root clears only an open question for the last tip (its
  message may be lost; the next question has a new code) or an `expired`
  mark, and saves the state before the listener closes the message. It
  refuses every other case: a tip that is skipped, failed, held, refused,
  failed its check, the failed commit of the broken state, or an instance
  that is paused or off. When `resume` and `pause` come in one run, the
  pause wins. Root's `pullgate status
  NAME --ask-again` still clears every mark.

**`/check`.** The listener writes a check request (section 8) only when all
of these hold, else it writes nothing:

- `from.id` is `owner_id`, and `chat.id` is `chat_id`;
- it is a new message, not an edited one (no `edited_message`, no
  `edit_date`);
- the text is exactly `/check` (a request for every instance in
  `instances`) or exactly `/check NAME`, with one space and a NAME that is
  in `instances`. Nothing before, after or between: no second word, no new
  line, no `@`. So `/check@otherbot` does nothing. The listener does not
  know its own bot name, so it refuses `/check@` for its own name too (in a
  private chat Telegram sends `/check` without it).

Then it replies "Checking NAME…" to the message. A second `/check` for the
same instance within 10 seconds writes nothing and gets "a check was asked
a few seconds ago" (section 8, the start limit). When a request cannot be
written (for example the host never ran pullgate for NAME, so the directory
does not exist), it replies that the request was not saved. To the owner in
the chat, a text that starts with `/check` but does not match gets the
usage text, which lists the instances and never repeats the message.

**`/help`.** When the text is exactly `/help`, from `owner_id`, in
`chat_id`, not edited, the listener replies with a fixed list: the
commands (every entry of the command table, below), the instance names from
`instances`, what Apply and Skip do, and the root commands for the host
(`pullgate status NAME`, `--pause`, `--resume`, `--ask-again`), at most
once per minute. The
listener does not know the mode or the pause file of an instance (root's
files), so the list points to `pullgate status NAME` for them. Any other
text of the owner in the chat that is not a command (does not start with
`/check`) gets one reply, "Not a command. Send /help for the list.", at
most once per minute (in memory), so a stuck client or a long paste cannot
fill the chat. A message of another user, or in another chat, or an
edited message, gets no answer, so the bot does not tell strangers that it
exists. The listener
never logs the text of a message: only why it was ignored and the sender
ID.

**One command table, and the menu** (code6x/pullgate#69). The command
handling, the `/help` list and the menu that Telegram shows when the owner
types `/` come from one table in `internal/listener/commands.go`, so they
cannot drift apart. A test checks that each name matches
`^[a-z0-9_]{1,32}$` (Telegram's rule), each menu text is plain text of 1
to 256 characters, and that there are at most 100. At start, after
`deleteWebhook`, the listener calls `getMyCommands` with the scope
`{"type": "chat", "chat_id": chat_id}` and, only when the list differs,
`setMyCommands` with the same scope. It sets no menu for any other scope,
so a menu set by hand in BotFather for other chats stays. In a group
chat, every member sees the menu; only the owner's commands count. Both calls have
a 30 second limit. A failure (also "chat not found") is logged once and
never stops the listener; the next start tries again. The menu shows only
names and fixed texts; it holds no instance name and nothing of the host.
A tap on a menu entry sends the bare command, so the text of a command
that needs NAME says so, and the bare command answers with the list of
instances.

The command to ask again is `/ask_again`, as Telegram allows no `-` in a
command name. `/ask-again` stays as an alias that is never shown. The
request file kind `ask-again`, the flag `--ask-again` and the state do not
change, so a root and a listener of different versions work together
during an update.

**Root** (`pullgate apply NAME`) takes the lock (with a wait). If the pause
file exists or `mode = "off"`, it removes the approvals, replies "ignored:
the instance is paused" (or off) to each question it knows, and exits.
Else it reads the approvals in name order. For each one:

1. find the question with that code in its own state, with a constant-time
   compare. No match: log and ignore.
2. check `asked <= now < expires` by its own clock. Else: drop the question,
   reply "expired", ignore.
3. remove the question from its state (synced), so the code works once.
4. `skip`: mark the SHA skipped, reply "skipped", next approval.
5. `apply`: take the SHA **from its own state**, never from the approval.
   Check again that the SHA descends from the applied SHA (unless
   `allow_older`, or there is no applied SHA). Else reply "refused" and
   ignore. Then apply it (section 10).

`pullgate apply` does at most one apply. Approvals after it stay in the
inbox for the next start. After an apply, every open question whose `base`
is no longer the applied SHA is closed ("the applied commit changed"), and
its SHA mark is cleared, so the next run asks again with a correct summary.

## 10. The apply

The steps, with the state synced to disk at each arrow:

1. Write `in_progress = {phase: "apply", sha, old_sha}` →
2. Export fresh trees (section 6). Run `apply` as root in the tree of the
   SHA (environment in section 11). Then `health`, if it is set.
3. Both succeed: the applied SHA becomes this SHA, `refs/pullgate/applied`
   moves, `in_progress` and the held SHA are cleared, all SHA marks are
   dropped (a SHA that is not a descendant is refused again anyway) →
   report "applied".
4. One fails: count a failed try for this SHA →. Then by `on_failure`:
   - `rollback`, with an applied SHA: write `in_progress = {phase:
     "rollback", sha, old_sha}` →. Run `apply` with the last good SHA, then
     `health`. If that works, clear `in_progress` → and report "failed,
     rolled back to OLD". If it fails too, the instance becomes
     **broken** (below): write `broken = {since, sha, old_sha, reason}` and
     clear `in_progress` in one save →, and report it. With no applied SHA
     there is nothing to roll back to: hold the SHA, as with `pause`.
   - `pause`: do not roll back and do not write the pause file. The SHA
     becomes the held SHA. Clear `in_progress` →. It is not tried again,
     every report and `pullgate status` say that the last apply failed, and
     a **newer** commit is still asked, with the line "The last apply (SHA)
     failed." at the top. So the owner can fix it forward with a new commit.
   The applied SHA does not change after a failure. State moves forward
   only after success.

**Tries.** Every try needs a new question and a new tap (in `auto` mode, a
new run). With `rollback`, a SHA with fewer than `max_failed_tries` failed
tries is asked again on the next run; with `max_failed_tries` failed tries
it is not asked again until a new commit (or `--ask-again`). With `pause`,
a failed SHA is held at once and the count does not matter.

**Broken.** After a failed rollback the host may be in a half state. A
plain pause would need root on the host for every such case, and most of
them are fixed by a new commit. So the instance becomes broken instead:

- One message goes out when it starts: the reason (the errors of the apply
  and of the rollback, each cut in the middle so that its end stays,
  code6x/pullgate#25) and the two ways out: a fix forward, or root on the
  host.
- The instance keeps fetching. It asks only for a **fix forward**: a tip
  that descends from the failed SHA, and is not the failed SHA or the old
  applied SHA. This holds with `allow_older` too. Any other tip is refused
  with a notice. The failed SHA is not tried again, whatever
  `max_failed_tries` says.
- The question has the lines "⚠ The instance is broken: an apply failed
  and pullgate could not roll it back. The host may be in a half state.
  This commit is a fix forward." and "Reason: …" at the top. The reason
  is in `<code>`, after the fixed line. In `auto` mode the owner is asked too:
  nothing is applied by itself on a half state. `questions_per_day`
  counts these questions.
- At the tap, root checks the fix forward rule again. A fix forward that
  succeeds (apply and `health`) ends the broken state. One that fails is
  **not** rolled back: the instance stays broken, the failed fix forward
  becomes the new failed SHA and reason, and the next newer commit is
  asked with the warning.
- The broken state never ends by itself. It does not stop a pause: a
  pause file still stops everything. Root ends it with `pullgate status
  NAME --resume`. No command from Telegram can end it.
- During a fix forward, `PULLGATE_FAILED_SHA` is the failed SHA.
- When it starts, every question asked before is dropped (the open one
  is closed in the chat), so no tap on a question without the warning
  applies. The next run asks again.
- If the save of the broken state fails, `in_progress` stays in the file,
  and the next start pauses (a crash, below).

**A crash** leaves `in_progress` in the state. The next run or apply cannot
know what happened on the host. It writes the pause file, marks the SHA
failed and reports "the apply (or rollback) of SHA was cut off; check the
host, then run `pullgate status NAME --resume`".

**Changes to the gate.** An approved commit may change the gate itself: the
host config, the units, or the pullgate binary, when the `apply` command
installs them. pullgate reads its config once when a process starts, so a
rollback uses the commands of the process that did the apply. Put the gate
files of the repo in the `watch` list, so the question warns about them.

## 11. Commands and their environment

Commands are argv lists. The first word must be an absolute path. No shell.
They run with an empty environment plus:

- `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`
- `PULLGATE_INSTANCE`: the instance name
- `PULLGATE_SHA`: the full SHA of the commit
- `PULLGATE_OLD_SHA`: the applied SHA, empty on the first apply
- `PULLGATE_TREE`: the tree of `PULLGATE_SHA`, read-only
- `PULLGATE_OLD_TREE`: the tree of `PULLGATE_OLD_SHA`, read-only, empty on
  the first apply
- `PULLGATE_CHANGED_FILES`: the path of a read-only file with the paths
  that differ between the two SHAs, each one ended by a NUL byte (all paths
  on the first apply). Read it with `xargs -0`. A file, because a list of
  paths can be too long for the environment and a path can hold a newline.
- `PULLGATE_FAILED_SHA`: the held SHA, if there is one. The host may be
  part way to it, not at `PULLGATE_OLD_SHA`.
- `PULLGATE_ROLLBACK`: `1` during a rollback, else empty.
- `PULLGATE_REPORT_FILE`: only for `apply`, and not in a rollback. The
  path of an empty file (root's, mode 0600, in the per-process trees
  directory) where `apply` may write short lines for the owner, for
  example "skipped step x on purpose". After `apply` (also when it
  failed), root reads the file: only a regular file, never a link
  (`O_NOFOLLOW`), at most 64 KiB. Its first 10 lines are added to the
  `applied` or `failed` message under "Report of apply:", each cleaned and
  shown as code (section 12), at most 1500 characters in all; a last line
  says how many lines were left out. A missing or empty file adds
  nothing. `health` does not get the file.

**In a rollback**, `PULLGATE_SHA` and `PULLGATE_TREE` are the last good SHA
and its tree; `PULLGATE_OLD_SHA` and `PULLGATE_OLD_TREE` are the SHA that
just failed and its tree; `PULLGATE_CHANGED_FILES` lists the paths that
differ between them. So `apply` must be safe to run again on an older tree.

Their working directory is `PULLGATE_TREE`. Their umask is 022. Each has a timeout
(`check_timeout`, `apply_timeout`); on timeout the whole process group is
killed and the command counts as failed. pullgate keeps at most 64 KiB of
their output. The output of `apply` and `health` goes to the journal; the
report shows only the exit status. `check` exits 0 to pass, 3 for
"nothing to apply on this host" (section 7, step 7), and anything else to
fail. The output of `check` (its last lines)
and of `summary` goes into Telegram messages, after the cleaning of
section 12.

**check and summary (rule 3)** run as `check_user`, with its own group and
no other groups. When pullgate runs as root, `check_user` must not be root
(uid 0), and must not be the listener user or share its group, because a
check that is taken over could then write approvals: pullgate refuses such
a config. When pullgate does not run as root
(tests, a trial), it cannot change user, so `check_user` must be the user
that runs it. A process of `check_user` could leave its process group and
live on. So after each `check` and `summary`, root runs `kill -KILL -1` as
`check_user`, which stops every process of that user; and when the run unit
ends, systemd stops everything left in its cgroup. Use one `check_user` per
instance. The run unit's sandbox (read-only system, private `/tmp`, no new
privileges) also applies to them.

## 12. The message

The question is HTML (`parse_mode = HTML`), with link previews off:

```
pullgate on <b>HOST</b>, instance <b>NAME</b>
⚠ The instance is broken: an apply failed and pullgate could not roll it
  back. The host may be in a half state. This commit is a fix forward.
Reason: <code>reason</code>                (both only when broken, section 10)
⚠ The last apply (SHA) failed.            (only when a SHA is held)
⚠ This commit is not a descendant of …     (only with allow_older)
Apply this commit?
<code>SHORT SHA</code>
First apply: there is no applied commit yet. All N files are listed.
or: Since <code>SHORT APPLIED SHA</code>:
⚠ Watched path changed: <code>path</code>
⚠ <code>text of a WARN: line of the summary</code>
⚠ N more warnings from the summary          (only with more than 10)
⚠ Symlink out of the tree: <code>path</code> → <code>target</code>
⚠ Submodule skipped: <code>path</code>

Commits (N):
• <code>subject</code>
Changed files (N):
• <code>path</code>
Summary:
<pre>output of the summary command</pre>
Full: <code>FULL SHA</code>
```

**SHAs.** Every message shows a SHA as its first 12 characters, in
`<code>`. The question, as it is sent, shows the full SHA once, on its last line
("Full:"), so it can be copied and compared. That line stays after a cut.
(After a tap, the listener adds its note below it.) Logs,
the state and `pullgate status` keep the full SHA. Root applies the full
SHA from its own state, never a SHA from the text.

**Text from the repo is only shown as code.** Text from the repo, the
commit, or a command that ran on it (paths, subjects, symlink targets,
submodule paths, the output of `summary` and `check`, and errors that may
hold any of them) is put into a message only by `message.Code` (one line in
`<code>`) or `message.Output` (lines in `<pre>`). Telegram makes no link,
mention, hashtag or bot command of text inside `<code>` or `<pre>`, so a
file named `README.md` or `login.example.com` stays plain text. The escape
function is not exported, so other code cannot put repo text into a message
another way; a test also checks the gate code for its own HTML. Before that,
the text is cleaned: control and invisible characters (also right-to-left
marks) become `?` (newlines stay only in the summary output), each line is
cut to 200 characters, and then it is HTML-escaped. At most 50 commit
subjects are read. Every message and every edit is sent with link previews
off.

**Warnings from the summary.** The `[watch]` list catches changed paths;
only the owner's `summary` can find other risky changes, because it reads
the tree. A line of the `summary` output that starts with `WARN: ` (exactly
these 6 characters, at the start of the line) is taken out of the `<pre>`
block and shown in the head, after the watched-path warnings, as
`⚠ <code>text</code>`: the text after the prefix, cleaned and cut to 200
characters like any repo text. A CRLF line end is not part of the text,
and a `WARN: ` line with no text is dropped. At most 10 such lines are shown; more give
one line "⚠ N more warnings from the summary". A summary with only `WARN: `
lines has no "Summary:" block. When `summary` failed, its output is not
used, so no warning comes from it. A summary without `WARN: ` lines gives
the same message as before.

The message must fit one Telegram message (4096 characters). pullgate cuts
it to 3800 characters, counted on the escaped text in UTF-16 units: the head
(host, instance, SHA, the warnings) always stays, and when lines are left
out the last line says how many ("… 120 more lines left out"). The cut is on
whole lines, so it never splits an HTML tag or entity. The `<pre>` block is
closed after a cut. The body is cut from its end: first the rest of the
summary, then the file list, then the commits. The "Full:" line stays last.
When the head alone is too long, the path warnings (watched paths,
symlinks, submodules) are cut first and counted ("⚠ … and N more
warnings"); the summary warnings stay. Each summary warning line is at most
220 characters after the escape (200 plain characters fit), so 10 of them
take at most about 2250 and the watched paths keep room. Only when the fixed
head lines and the summary warnings alone do not fit (a very long broken
reason) are the last summary warnings counted in the "more warnings from
the summary" line, so that the message still fits.

## 13. Modes, limits, status

- `ask` (the default): as above.
- `auto`: no question. `pullgate run` records the question in its state and
  writes its code to `/var/lib/pullgate/NAME/auto-approval` (root-owned,
  0600). The path unit starts `pullgate apply`, which handles it like an
  approval from the inbox, then removes it, and reports. The apply needs
  the same checks (code, time, forward only). For development hosts.
  **`/check` on an `auto` instance** starts the run as the timer does, so a
  new commit is applied at once, without a tap. This is a choice: an auto
  instance applies every new commit within one timer period anyway, so
  `/check` only makes it sooner. It cannot apply a commit that the timer
  would not apply. The reply says "it is applied now, without a tap". Who
  does not want this keeps the instance in `ask` mode.
- `off`: `pullgate run` and `pullgate apply` do nothing.

`pullgate status NAME` prints the mode, the applied SHA, the open question,
the held SHA, the mark of the last tip (`nothing` reads "nothing (the check
found nothing to apply on this host)"), "settling until …" while the tip
waits for `settle`, the broken state ("broken since …, last error …"), the
pause file and an apply in progress. Flags:

- `--ask-again`: clear the marks of the last fetched tip (expired, skipped,
  check failed, nothing, refused, failed, held), so the next run asks again. If a
  question for the tip is open (for example its message was lost), root
  removes it from its state, then asks the listener to close its message.
  The next run asks again with a new question ID and a new code. An
  approval for the old code finds no question and does nothing, even one
  that the listener wrote before the close;
- `--pause`: write the pause file;
- `--resume`: remove the pause file, clear an apply in progress, and end
  the broken state (section 10). Run it only after you checked the host.

The listener (`pullgate listen --config /etc/pullgate/listener.toml`):

```toml
token_file = "/etc/pullgate/bot-token"   # or $CREDENTIALS_DIRECTORY/bot-token
owner_id = 123456789                     # the Telegram user ID of the owner
chat_id = 123456789                      # the chat (the private chat with the owner)
instances = ["config-repo", "playbooks"]
spool_dir = "/var/spool/pullgate"        # default
state_file = "/var/lib/pullgate-listener/state.json"
api_url = "https://api.telegram.org"     # only tests change it
```

Several instances share one listener when they share one bot: two
`getUpdates` loops on one token conflict.

All processes log with `log/slog` to stderr; the units set
`SyslogIdentifier=pullgate`, so each step is in the journal. The bot token,
the deploy key and the code never appear in a log line or an error.

## 14. Tests

Each rule and each limit has a test. The tests use a fake Telegram server
(`net/http/httptest`) and a local bare git repo as the remote. They need
`git`. On CI, a missing `git` is a failure, not a skip.

The end-to-end test (`test/e2e`, build tag `e2e`) builds the binary, makes a
bare repo with two commits, runs `pullgate run`, the listener against the
fake Telegram server, a tap, `pullgate apply`, and checks that the second
commit was applied. Then it sends `/check` and checks the request, the run
and the reply "No new commit".

The tests of `install.sh` (`install_linux_test.go`, at the root) run the
script with `/bin/sh` against a fake release: a fake `curl`, `uname`, `gh`
and `pullgate`. They never use the network. They show that the script
installs nothing when a check fails (section 16), and that it installs when
all checks pass. CI also runs a pinned ShellCheck on the script.

`test/units/first-run.sh` runs on CI as root, on the throwaway runner. It
installs the binary and the units of `contrib/systemd/` as the README
says, with no drop-in. It runs the first `pullgate run` under the real
`pullgate-run@.service`, checks the owners and modes of section 4, then
runs `pullgate listen` under the real `pullgate-listen.service` against a
fake Telegram on `127.0.0.1`, and checks that the listener sent the
question and removed it from the outbox. At the end it runs
`systemctl enable` on the four units of the README (a list in the
script), and checks that each is `enabled`. It also follows the README's
update from 0.1.0 for the listener: the old unit with `add-wants`, then the
new unit and `reenable`. So a unit setting that breaks a run or the listener, or a unit
that cannot be enabled, fails CI. Without root, CI also runs
`systemd-analyze verify` on every unit, and `test/units/install-sections.sh`
checks that each unit the README tells you to enable has an `[Install]`
section with `WantedBy=`.

`internal/selfupdate` tests `pullgate update` with the real built-in
`install.sh` against a fake release (a fake `curl`, `gh` and `uname`, and
fake binaries): the question, the install, a wrong SHA-256, a failed
attestation, a wrong version (each before the question and on Apply), a
tarball that changed after the question, an older version, a newer release
while a question is open, Skip, an expired question, a listener that does
not start (rollback), a crash at each step, the daily notice (once, again
each day for a security release, off with `check = false`), and the unit
update (a changed unit, a new unit, a masked unit, a disabled unit, a
drop-in).

`test/units/full-loop.sh` (CI job `install-loop`, on each pull request
and on `main`) is the path a user takes, as root on the throwaway runner:

1. It runs the install commands **from README.md itself**: each install
   block has a comment line `<!-- install-test:NAME -->` (`binary`,
   `setup`, `start`) before its `sh` block, and the script runs the block
   as written, with `NAME` set to `config-repo`.
   `test/units/readme-block.sh --check` fails when a block is missing, or
   when an install command (`useradd`, `groupadd`, `install -X`, `chown`,
   `chmod`, `cp contrib/systemd`, `systemctl enable`) is in an unmarked
   fenced block. So
   the README cannot drift from what CI runs.
2. A local bare repo is the remote. A fake Telegram on `127.0.0.1`
   (`test/units/faketg.py --tap`) records each message and taps Apply on
   each question as the owner. The instance is a recipe, on a fake
   release server on `127.0.0.1` with a fake gh: `pullgate bootstrap
   --recipe` sets it all up, with no setup block before it and no
   `systemctl enable` after it (section 20).
3. A good commit: `pullgate run` under `pullgate-run@.service` asks (with
   `check` and `summary` as the check user), the listener (user
   `pullgate`) sends the question and writes the approval,
   `pullgate-apply@.service` applies it, and `pullgate status` shows it as
   applied.
   Then a commit moves the recipe to a new version: the recipe's `apply`
   runs `pullgate recipe fetch` and `pullgate recipe use` (section 21),
   and `pullgate status` shows the new version.
4. It restarts the listener and checks that it polls again.
5. A commit whose apply fails: the rollback runs, and its notice reaches
   the fake Telegram.
6. With `--update-to` (the CI job builds this commit twice, as 900.0.0
   and 900.0.1): a fake GitHub (a fake `/usr/bin/curl` that serves local
   files, and a fake gh in `[updates] gh`, because the build under test has
   no attestation), a changed unit and a drop-in, then `/update` on the
   fake Telegram. The fake taps Apply; root installs 900.0.1 with the
   built-in `install.sh`, puts back the shipped unit, lists the drop-in,
   restarts the listener, and the new listener sends the report.
7. Each unit of the README `start` block is `enabled` and `active`.

After each loop, the outbox must be empty and the listener must have
logged no error and no warning. So a message that root writes and the
listener cannot read fails the job.

The workflow **Install a release** (`install-release.yml`) runs the same
script against a published release: it installs the release with the
tag's own `install.sh --version X.Y.Z`, then runs
`full-loop.sh --installed X.Y.Z` with the README and units of the tag.
The Release workflow calls it after it publishes; it also runs on
`release: published` and by hand.

## 15. Updates of pullgate itself (`/update`)

The owner updates pullgate from Telegram. It is the same gate as for a
commit: one question that names one exact release, and a tap. What it
trusts: THREAT-MODEL.md, T20.

**The parts.** `pullgate update` runs as root, from
`pullgate-update.service`. Two units start it: `pullgate-update.timer`
(once a day) and `pullgate-update.path` (the check request or an approval
of the updates). It reads `/etc/pullgate/listener.toml` (it needs no
token), and uses its own state and spool, named `_update`
(`/var/lib/pullgate/_update/`, `/var/spool/pullgate/_update/`). An instance
name cannot start with `_`, so no instance can use them. The binary carries
two things of its own version: a copy of `install.sh` (section 16), and the
units of `contrib/systemd/` (`pullgate units` prints them as JSON, with the
list of units that the README enables).

**The listener** handles `/update` as `/check`: only exactly `/update`,
from `owner_id`, in `chat_id`, not edited. It writes the check request
`_update/check/request` (the six bytes `check\n`, nothing from the
message), at most one in 10 seconds, and replies "Looking for a new release
of pullgate…". A tap on an update question writes an approval to
`_update/inbox`, as for an instance (section 9). It sends what root puts
in `_update/outbox`.

**Looking for a release.** Root runs the built-in `install.sh
--check-only` with `TMPDIR=/var/lib/pullgate/_update/tmp`. The script does
the checks of section 16 on the newest release (the SHA-256 and the
attestation; it does not run the new binary before the tap), installs
nothing, and
prints lines that start with `@pullgate ` for root: `version X.Y.Z
security|normal`, `up-to-date X.Y.Z`, `fetched X.Y.Z`, `checked X.Y.Z
SHA256 security|normal`. A release is a security release when its notes
have the section `### Security` (release-please makes it from `security:`
pull requests, docs/releasing.md). Root itself checks again that the
version is newer than its own. Then:

- **Up to date**: nothing. For `/update`, a notice says so.
- **A newer release that passed every check**: for `/update`, root asks.
  From the timer, it sends one notice per release: the version, the link
  to the release notes and "send /update to install it". A security
  release says so on the first line, and its notice comes again each day
  (23 hours) until it is installed. A notice has no buttons and needs no
  tap. `[updates] check = false` in `listener.toml` stops the daily look
  and its notices; `/update` still works.
- **A release that failed a check** (after its files were downloaded):
  one warning notice per release ("failed a check, so it is not
  offered"), with the reason from `install.sh`. It never gets a question
  or the "/update" hint. For `/update`, the warning comes each time.
- **No answer from GitHub** (before the download): a log line; for
  `/update`, a notice. A build from the source (`pullgate dev`) does
  nothing and says so: it cannot know which release is newer.

At most one `/update` per minute is acted on (`check_at` in root's state).

**The question.** Root makes a new code, writes the question to its state
(synced), then to the outbox, as in section 7:

```
pullgate on <b>HOST</b>, update of pullgate
⚠ Security release. It fixes a security problem.   (only for one)
Update pullgate to <code>X.Y.Z</code>?
Installed now: <code>OLD</code>
Release notes: https://github.com/code6x/pullgate/releases/tag/vX.Y.Z
File: <code>pullgate_X.Y.Z_linux_ARCH.tar.gz</code>
SHA-256: <code>…64 hex digits…</code>
```

It can be answered for 12 hours. At most one update question is open.
When the timer or `/update` finds a newer checked release while a question
is open, root closes the old one ("a newer question replaced this one")
and asks again. When the installed version is the newest again (installed
by hand), it closes the question.

**Root's state**, `/var/lib/pullgate/_update/state.json` (root, 0600):
`question` (id, code, version, sha256, asked, expires), `notified` and
`warned` (release → time of its last notice), `check_at`, `check_note`,
and `in_progress` (version, from, started) while an update runs.

**Apply.** `pullgate update` takes the lock (`_update/lock`), reads the
approvals as in section 9 (code with a constant-time compare, time, once),
and takes the version and the SHA-256 **from its own state**. Skip: a reply,
nothing else. Apply, forward only (the version must be newer than its
own; never an older one from Telegram):

1. Write `in_progress` (synced). Give the old binary a second name,
   `DIR/.pullgate.prev` (a hard link), and sync the directory.
2. Run the built-in `install.sh --version X.Y.Z --sha256 SHA256
   --forward-only --bin-dir DIR` (and `--gh` from `[updates] gh`). It
   downloads the files again and does every check again; the tarball must
   also have the SHA-256 of the question. It renames the new file over
   `DIR/pullgate` as its last step. A failure here installs nothing: root
   removes `.pullgate.prev`, clears `in_progress` and reports why.
3. Run `DIR/pullgate version`: it must print `pullgate X.Y.Z`.
4. Run `DIR/pullgate units` (the new binary). Check the list: only names
   `pullgate-*.service`, `.timer`, `.path`, at most 32 files of 64 KiB.
   Write each unit that differs from `/etc/systemd/system/NAME` (a
   temporary file, sync, rename), and keep the old content. A unit there
   that is a link (masked) is left alone. Drop-ins
   (`/etc/systemd/system/pullgate-*.d/*`) are never changed; they are
   listed in the report.
5. `systemctl daemon-reload`. For each unit that the README enables (for
   each instance in `instances`): when it is enabled, or new in this
   version, `systemctl reenable` and `systemctl restart` it. A unit that
   the owner disabled stays disabled. The listener is re-enabled here,
   but restarted last.
6. `systemctl restart pullgate-listen.service`, wait 5 seconds, and check
   `systemctl is-active pullgate-listen.service`.
7. Remove `.pullgate.prev`, clear `in_progress`, and write the report:
   "pullgate is updated: OLD → NEW", the units it installed, the links it
   left alone, and every drop-in. The new listener sends it.

When step 3, 4, 5 or 6 fails: rename `.pullgate.prev` back over
`DIR/pullgate`, write back the old unit files (and remove the new ones),
`daemon-reload`, restart the units and the listener, clear `in_progress`,
and report "the update failed: REASON. pullgate OLD is back". If the
rollback has problems too, the report says which, and how to fix it by
hand with `install.sh`.

**A crash** leaves `in_progress`. The binary is the old file or the new
one: each change of the name `pullgate` is a rename in one directory, after
a sync (THREAT-MODEL.md, T10). The next start reports "the update was cut
off" with what `pullgate version` says, and how to check the host. It does
not guess, and leaves `.pullgate.prev` for the owner.

**The config** (in `listener.toml`, every key optional):

```toml
[updates]
check = true                # false: no daily look, no notices; /update works
listener_user = "pullgate"  # the listener user: owns the inbox of the updates
gh = "/usr/bin/gh"          # the gh that checks the attestation, as install.sh --gh
```

There is no key to skip a check.

**Not yet**: signed commits (`git verify-commit`), a web UI.

## 16. The install script

`install.sh`, at the root of the repo, installs a release, or updates one.
It is a POSIX `sh` script. Root runs it on the host itself:

```sh
sh install.sh [--version X.Y.Z] [--bin-dir DIR] [--gh PATH] [--reinstall]
```

`pullgate update` runs the copy of this script that is built into the
binary, with three more flags (section 15): `--check-only` (every check,
install nothing), `--sha256 HEX` (with `--version`: install only a tarball
with this SHA-256) and `--forward-only` (never the same or an older
version). It reads the lines that start with `@pullgate ` on the standard
output.

All the work is in functions. Only the last line starts `main`, so a
download that stops in the middle runs nothing. The steps:

1. Clean the environment: remove `TAR_OPTIONS`, `GZIP`, `CURL_HOME`, the
   `SSL_CERT_*`, `OPENSSL_*` and `LD_*` variables, `GCONV_PATH`,
   `GLIBC_TUNABLES`, `GODEBUG`, `CDPATH`, `IFS` and more (the list and the
   reasons are in `clean_env` of the script). Set `LC_ALL=C`. As root, set
   `PATH=/usr/sbin:/usr/bin:/sbin:/bin`. pullgate and gh run with a time
   limit (`timeout`): 60 seconds, and 300 seconds for
   `gh attestation verify`. curl has its own limit.
2. Read the arguments. `--version` must be `X.Y.Z`. A wrong argument
   exits with 64.
3. Check the platform: Linux on amd64 or arm64 only.
4. Check the paths: only root may change the bin directory
   (default `/usr/local/bin`), each directory above it, the old binary and
   the `--gh` file. The old binary `DIR/pullgate` must be a regular file or
   missing, never a link. A directory that others can write may pass only
   above the checked path, and only with the sticky bit (as `/tmp`).
5. Make a temporary directory with `mktemp -d` in `TMPDIR` (default
   `/tmp`). Check `TMPDIR`, each directory above it, and the new directory.
6. Ask the GitHub API for the newest release, or for the release of
   `--version`. Its tag must be `vX.Y.Z` of that version.
7. Run the old binary with `version`. The same version: nothing to do,
   exit 0, unless `--reinstall`. A newer installed version: nothing to do,
   unless `--version` asks for the older one; then it says that this is a
   downgrade. An unknown version (`pullgate dev`, no output, or it does not
   run): fail, unless `--reinstall`.
8. Download `pullgate_<version>_linux_<arch>.tar.gz`, `SHA256SUMS` and
   `pullgate_<version>.sigstore.json`. Every download uses
   `curl -q --proto '=https' --tlsv1.2` and has a size limit: 1 MiB for the
   API answer, `SHA256SUMS` and the bundle; 64 MiB for a tarball.
9. `SHA256SUMS` must have exactly one line for the tarball, and the SHA-256
   must match.
10. Get gh: the `--gh` file (gh 2.93.0 or newer), or gh 2.102.0, downloaded
    and checked against the SHA-256 for this CPU that is written in the
    script. gh runs with no token and an empty config.
11. Run `gh attestation verify` on the tarball with the bundle,
    `--repo code6x/pullgate`, `--cert-identity
    https://github.com/code6x/pullgate/.github/workflows/release.yml@refs/heads/main`
    and `--deny-self-hosted-runners`.
12. Take only `pullgate` out of the tarball. Run `pullgate version` from the
    temporary directory. It must print `pullgate X.Y.Z` of the version
    asked for, and nothing else.
13. Copy it into the bin directory under a new name, mode 0755, owner
    root. Run `pullgate version` there again. Write it to the disk
    (`sync`), then rename it over the old binary. The rename is atomic: a
    new process gets the old file or the new one.
14. Print what to restart. The script restarts nothing.
    `pullgate-listen.service` runs the old binary until it is restarted.
    The timers and path units start a new process each time, so they get
    the new binary at their next start.

A trap removes the temporary directory and a new file that was not renamed
yet, at every exit. A failure exits with 1, and its last line says that
nothing was installed. What the script trusts: THREAT-MODEL.md, T17.

## 17. Bootstrap (`pullgate bootstrap`)

`pullgate bootstrap` sets up a new host, as root, after the binary is
installed (README "Install"). It is the one command of the setup: it also
makes the users, the directories and the units, runs `pullgate doctor`,
and enables the units. Its goal: nobody types or pastes the owner ID or the
chat ID, and the token never shows.

```
pullgate bootstrap [--token-stdin] [--api-url URL]
    --instance NAME --repo URL [--branch B] [--apply PROGRAM] [--known-hosts FILE]
    [--instance NAME2 ...]
```

It must run as root, and `/etc/pullgate` and its parents must be safe
(rule 6). `--api-url` takes only `https://api.telegram.org` or a test
server on `http://127.0.0.1:PORT` (no user info): another server would see
the token and could plant the IDs. A test server gets a loud warning.
`listener.toml` lists only the instances of this run: give every instance
of the host in each run, or use `--add` (section 20) on a host that is
paired. `--recipe URL` makes the configs from a checked recipe instead
of `--apply` (section 20).

1. **Checks first.** Every instance name, URL and branch is checked, and
   an ssh URL to a host other than `github.com` needs `--known-hosts`. A
   new instance (no `NAME.toml`) needs `--apply`. Nothing is asked or
   written before these pass.
   **Then the setup:** the listener user `pullgate` and the check user of
   each instance (`check_user` of an existing `NAME.toml`, else
   `pullgate-check-NAME`; it must not be the listener user) are made with
   `useradd --system --no-create-home --user-group --shell
   /usr/sbin/nologin` when they do not exist. The base directories are
   made with the owner and the mode that `pullgate doctor` checks (the same
   table, `doctor.BaseDirs`: `/etc/pullgate` and `/var/spool/pullgate`
   root 0755, `/var/lib/pullgate` root 0711), set through the open
   directory (`O_NOFOLLOW`) and read back. The units that the binary
   carries are written to `/etc/systemd/system` (owner root, 0644; a unit
   that differs is replaced, so changes belong in a drop-in), then
   `systemctl daemon-reload`.
2. **The token.** It runs `systemd-ask-password` (no echo, not in the
   shell history, not an argument), or with `--token-stdin` reads the
   first line of stdin. It must look like a bot token. `getMe` checks it.
   The token is never printed, never in an error, never an argument, and
   never in the environment of a program that bootstrap starts: each one
   gets only `PATH` and `LC_ALL`.
3. **Pairing.** It makes a one-time code of 8 random digits, valid for 10
   minutes, and prints it on the console. It skips the updates that came
   before the code. It reads messages with `getUpdates`. A message that is
   not a number (for example `/start`) is skipped. Any other number is a
   wrong code; at 5 wrong codes it stops. The right code (compared in
   constant time) counts only from a **private** chat with a person: chat
   type `private`, the sender is not a bot, and the sender ID is the chat
   ID. The right code from a group or a channel stops it: the code is used
   up. Then it writes `bot-token` (root, 0600) and `listener.toml` (root,
   group of the listener, 0640; written first) with `owner_id` and `chat_id` from that
   message, replacing any file that was there. A wrong code, a group, a
   timeout or an error writes nothing; an old `listener.toml` stays, and
   bootstrap says that it did not write it. It confirms the updates, so
   the listener does not see the code, and sends "paired" to the chat.
   There is no flag, config key or variable that sets these IDs.
4. **Each instance.** For an ssh URL: it makes `NAME.key` with `ssh-keygen
   -t ed25519` (an existing key is kept only if it is a regular file of
   root; a `.pub` without a key is refused), sets it to 0600, and prints the
   public key with "add it as a read-only deploy key". It writes
   `NAME.known_hosts` from the `github.com` ed25519 key that the binary
   carries, or from `--known-hosts FILE`, and prints the SHA256 fingerprint
   of each key, to compare with what the git host publishes. A local URL
   needs neither. If `NAME.toml` does not exist, it writes a minimal one:
   `mode = "ask"`, `on_failure = "pause"`, the repo, the key, the known
   hosts, `apply` and `check_user = "pullgate-check-NAME"`. An existing
   `NAME.toml` is kept, with its own repo, key and known hosts: then no
   key or known hosts file is made, and the probe uses the config. Then it runs `git ls-remote` (section 6: the same git
   flags and ssh options) every 10 seconds, for at most 30 minutes, until
   the branch can be read.
   Then it makes the state directory and the spool of the instance, as a
   run does (`state.Dir.Ensure`, `spool.Dirs.Ensure`, with read-back).
5. **Doctor, then the enable.** It runs `pullgate doctor` on the instances
   of the new `listener.toml`, without the checks that the units are
   enabled and run (`BeforeEnable`). Any problem: it enables nothing and
   fails with "pullgate doctor found N problem(s) … no unit was enabled".
   Else it runs `systemctl enable --now` for every unit of `pullgate units`
   `enable` (NAME is each instance), but `systemctl enable` only for
   `pullgate-listen.service`, and then restarts it (so it reads the new
   `listener.toml`; one start, so no listener is killed in its first
   request), then doctor again with every check.
   A problem then fails bootstrap with "the units are enabled, but …".

**pullgate's own files.** `listener.toml`, `bot-token`, `NAME.key`,
`NAME.known_hosts` and the instance `mode` belong to pullgate. A later
config tool, for example Ansible run by an instance, must not write them.
`listener.toml` says so in its first lines.

## 18. Doctor (`pullgate doctor`)

`pullgate doctor [NAME] [--telegram] [--config FILE] [--unit-dir DIR]`
checks the setup of a host, as root. It prints one line per check: `ok`,
`note` (worth a look, not a problem), or `PROBLEM` with the fix in the same
line. It exits 1 when there is a problem, else 0. With NAME it checks that
instance, else every instance of `listener.toml`. It changes nothing, but
for one test file in each outbox, which it removes.

`/doctor` from Telegram runs these checks too, without the ones that read
a secret, and redacted (section 9).

The checks:

- **Version and units.** The version of the binary. Each unit that this
  version carries (`pullgate units`) is installed in `/etc/systemd/system`
  (else a problem); one that differs from it is a note. The units the
  README enables are enabled: the listener, `pullgate-apply@NAME.path` and
  `pullgate-run@NAME.timer` must be (else a problem); the check path unit
  and the update units are optional (a note). `pullgate-listen.service`
  must be active.
- **Configs.** `listener.toml` is valid and safe (section 4), and lists
  NAME. The bot token file is owned by root (the systemd credential of the
  example unit) or by the listener user, with mode 0600. Each instance
  config is valid, and its gate files and programs pass the checks of
  section 4; `check_user` and the listener user exist and differ; the
  `[listener] user` is the same as `updates.listener_user`.
- **Owners and modes, as read back.** The type (a directory, or a regular
  file for `state.json`), and: the state directory, the clone, the
  trees, `state.json`, the spool, the outbox, the inbox, the check and the
  receipts directories have exactly the owner, the group and the mode of
  section 4 (setuid, setgid and sticky bits count). A path the first run
  has not made yet is a note.
- **The listener can read the outbox.** Root writes a test file to the
  outbox as `Send` does, under a name that starts with `.doctor-` (the
  listener skips names that start with `.`). Then doctor starts itself as
  the listener user, with its groups (`pullgate _doctor-read PATH`), and
  reads one byte of it. The test file is removed. It reads
  `listener.toml` the same way, as the listener user. `_doctor-read` reads
  one byte, follows no symlink, and prints nothing of the file.
- **The repo.** `git ls-remote` of the branch, with the deploy key and the
  pinned host key, as a run fetches, in a new temporary home.
- **Telegram**, only with `--telegram`: `getMe` (the token works) and
  `getChat` with `chat_id` (the bot can reach the chat). No message is
  sent. The token is never printed.

The README "Install" ends with `pullgate doctor`, and the CI install test
(`test/units/full-loop.sh`) runs that README block and needs exit 0.

## 19. Recipes (`pullgate recipe fetch`)

A recipe is an instance's scripts and config template, as `FILE.tar.gz` in
a GitHub release of the project that ships it (code6x/pullgate#55). This
section has the first step: fetch, check and unpack. README "Recipes" has
the format and the test options.

`pullgate recipe fetch URL [--gh PATH] [--bundle NAME] [--signer-workflow PATH] [--dir DIR]`,
as root:

1. **The URL** must match
   `https://github.com/OWNER/REPO/releases/download/vX.Y.Z/FILE.tar.gz`
   as a whole (OWNER, REPO and FILE of `A-Za-z0-9_.+-`, not starting with
   `.`). For tests the same path on `http://127.0.0.1:PORT`, with a
   warning. A redirect must stay on HTTPS (GitHub sends release files to
   its storage), at most 5. TLS 1.2 or newer, no proxy.
2. **gh** is checked before any download: an absolute path, a regular
   file owned by root and not writable by group or others, in safe
   directories (rule 6), version 2.93.0 or newer (the minimum of
   install.sh). It runs with only `PATH`, `LC_ALL`, an empty `HOME` and
   `GH_CONFIG_DIR`, so it has no token.
3. **Downloads**: the tarball (at most 64 MiB), `SHA256SUMS` and the
   bundle (at most 1 MiB each), into memory. An HTTP error is a failure.
4. **SHA-256**: `SHA256SUMS` must have exactly one line for FILE, and it
   must match.
5. **Attestation**: `gh attestation verify` on a private copy, with the
   bundle, `--repo OWNER/REPO`, `--cert-identity
   https://github.com/OWNER/REPO/WORKFLOW@refs/heads/main` (WORKFLOW is
   `.github/workflows/release.yml`, or `--signer-workflow`),
   `--deny-self-hosted-runners` and `--format json`, at most 5 minutes.
   A subject of the signed statement must have the name FILE and the
   SHA-256 of step 4. Then it prints the repo, the tag, the file, the
   SHA-256 and the identity.
6. **Unpack** into `DIR/.tmp-*` (DIR is
   `/usr/local/lib/pullgate/recipes`; the folders are made 0755 and must
   pass the check of rule 6; `OWNER/REPO` is made only after every check).
   A pax global header (as `git archive` writes) is skipped. Only regular files and folders, with
   clean relative names of `A-Za-z0-9_.+-` and `/`; no `..`, no absolute
   path, no setuid, setgid or sticky bit, no entry named like the record;
   one file at most 16 MiB, all at most 64 MiB, at most 1000 entries. A
   file gets 0755 when it has an execute bit, else 0644. Then the
   manifest is read (unknown keys refused); its `version` must be the X.Y.Z
   of the URL (the attestation names the file, not the tag, so a signed
   tarball of an older release put into a newer one is refused here); and
   the programs of `requires` are looked up. Then the record `.pullgate-recipe.json` (the SHA-256 of
   the tarball and of each file) is written, and the folder is renamed to
   `X.Y.Z`. Any failure removes the temporary folder: nothing is written.
7. **A version folder that is there already** is used only if it, and
   everything in it, is owned by root, not writable by group or others,
   with no setuid or setgid bit, and its files are exactly the files of
   its record with the same SHA-256, for the same tarball; `requires` is
   checked again. Else the error
   says to move it away.

Section 20 sets an instance up from a recipe; section 21 updates it.

## 20. Bootstrap from a recipe (`pullgate bootstrap --recipe`)

```
pullgate bootstrap [--add] --recipe URL [--gh PATH] [--bundle NAME] [--signer-workflow PATH]
    [--replace-config] --instance NAME --repo URL [--branch B] [--known-hosts FILE]
    [--recipe-instance NAME] [--instance NAME2 ...]
```

It is section 17 with the config of each instance made from the recipe's
template. The order:

1. **Checks** of section 17 step 1, and the recipe URL (section 19 step
   1). `--apply` is refused with `--recipe` (the template names the
   programs). `--gh`, `--bundle`, `--signer-workflow`, `--replace-config`
   and `--recipe-instance` need `--recipe`. A `NAME.toml` that is there
   already must load (section 5) and have the same repo URL (and branch,
   if `--branch` is given): a kept config keeps its repo. Every
   placeholder value is checked (step 3).
2. **Fetch** (section 19, with the same flags). Inside it, before the
   rename of the new folder (or on a folder that is there already), every
   instance gets its config from its template (step 3). A failure writes
   nothing: no recipe folder, no user, no unit, no config.
3. **The template.** Each `--instance NAME` picks the instance of the
   manifest named by `--recipe-instance`, else the one named NAME, else
   the only one. Its template is TOML with the placeholders `@NAME@`,
   `@HOST@` (the host name), `@REPO_URL@`, `@BRANCH@` and `@RECIPE_DIR@`
   (the version folder), filled in one pass. For a kept config,
   `@REPO_URL@` and `@BRANCH@` are its own. Another `@WORD@` is refused,
   and so is a value with `"`, `'`, `\`, a newline, another control
   character, or a `@WORD@`. The filled template must be TOML with every key in lower
   case (`a-z`, `0-9`, `_`: the TOML decoder matches keys without case,
   so `Mode` would set `mode`; every config of pullgate refuses such a
   key). It must not set `mode`, `[repo] deploy_key`, `[repo]
   known_hosts`, `[commands] check_user` or `[listener]`; it may set
   `[repo] url` and `branch` only to `@REPO_URL@` and `@BRANCH@`. pullgate
   then sets `mode = "ask"`, the repo URL and branch, the deploy key and
   the known hosts (`NAME.key` and `NAME.known_hosts` for an ssh URL, or
   those of a kept config), and `check_user` (`pullgate-check-NAME`, or
   that of a kept config). The result must pass the checks of section 5,
   without the file checks. It is written with a header that names the
   recipe; comments of the template are not kept.
4. **Users, directories, units**, then the token and the pairing, as
   section 17. With `--add`, no token is read and no pairing is done:
   `listener.toml` must pass the checks of the listener (owner, mode,
   keys) and `bot-token` must be there. It prints the owner ID, the chat
   ID and the instances it keeps. `--add` takes no `--token-stdin` and no
   `--api-url`.
5. **Each instance**: the deploy key and the host keys for a new config
   (section 17 step 4), the wait for `git ls-remote`, and the state and
   the spool.
6. **The setup** of each instance, if its manifest entry has `setup`:
   the program of the recipe with its arguments, as root, in the version
   folder, with only `PATH` (section 19's search path), `LC_ALL=C`,
   `PULLGATE_INSTANCE` and `PULLGATE_RECIPE_DIR`, at most 30 minutes, in
   its own process group (killed at the timeout). Its output goes to the
   console. A failure stops bootstrap: no config is written and no unit
   is enabled; the instance is off, or keeps the config it had. The error
   says so.
7. **`NAME.toml`**, only after every setup passed. If there is none, the
   template's config is written (root, 0600). The header names the
   recipe and holds no date, so the same recipe with the same values
   makes the same file. If there is one and it differs, the diff is printed (`-` the file, `+` the recipe) and the
   file is kept; with `--replace-config`, the old one is copied to
   `NAME.toml.bak-YYYYMMDD-HHMMSS` (root, 0600) and the new one written.
   A new config is in `ask` mode, also when the old one was `auto`.
8. **The record**, only when `NAME.toml` is now the recipe's config (written,
   or the same; not for a kept config that differs: then an old record
   is removed, so status does not name a recipe the config may not be), in
   `/etc/pullgate/NAME.recipe` (root, 0644, TOML): the URL,
   `OWNER/REPO`, the version, the file, its SHA-256, the version folder,
   the instance of the manifest, the bundle and signer workflow flags,
   the value of `@HOST@` and the time. `pullgate status NAME` and
   `/status` show the recipe and its version from it (a line `recipe:`),
   or say that it cannot be read. It is information: no check of a run
   reads it.
9. With `--add`, `listener.toml` is written again only now, with every
   key kept and the new names added to `instances`, so the listener does
   not ask for an instance that is not set up. Then doctor and the
   enable (section 17 step 5).

## 21. Updates of a recipe (`pullgate recipe update`, `pullgate recipe use`)

```
pullgate recipe update NAME (--version X.Y.Z | --url URL) [--reinstall] [--gh PATH] [--dir DIR]
pullgate recipe use NAME X.Y.Z [--dir DIR]
```

Both run as root, on an instance that has a record `NAME.recipe`
(section 20); without one, the error says to run `bootstrap --recipe`.

- **The new version.** `--version X.Y.Z` makes the URL from the record:
  the same release folder with `vX.Y.Z`, and the same file name with the
  old version replaced by the new one. `--url URL` gives it whole. It
  must be a recipe URL (section 19) of the same `OWNER/REPO` as the
  record; another project needs `bootstrap --recipe --replace-config`.
- **Forward only.** A version older than the record's is refused, by
  both commands and by `bootstrap --recipe`. The same version is refused
  by `update` unless `--reinstall`; for `use` it is nothing to do (exit
  0), so an `apply` that runs again does not fail. Nothing is written
  when it is refused.
- **`update`** fetches the new version with every check of section 19
  (the `--bundle` and `--signer-workflow` of the record).
- **`use`** downloads nothing: the version folder must have been
  unpacked by `recipe fetch`. It must pass the checks of a folder that is
  there already (section 19 step 7) against its own record of the
  unpack, which also names the URL it came from and the `--bundle` and
  `--signer-workflow` of the fetch (they must be those of the instance
  record); `requires` is checked again. A version has no leading zeros
  (`1.2.3`, not `01.2.3`), so one version has one folder.
- **The config** is made again from the new template (section 20 step
  3), before anything is written, with the same local values: the repo
  URL, branch, deploy key, known hosts and check user of `NAME.toml`, the
  `@HOST@` and the recipe instance of the record. The `mode` of
  `NAME.toml` is kept (the owner may have set it). The old `NAME.toml` is
  kept as `NAME.toml.bak-YYYYMMDD-HHMMSS`. Then the record is written
  again. The recipe's `setup` does not run: it runs only at the
  bootstrap.
- **Folders.** The folder of the version before is kept. A version
  folder of the same recipe that is older than that one is removed when
  no record in `/etc/pullgate` names it (by its repo and version, or its
  folder) and no `/etc/pullgate/*.toml` holds its path. When a record or
  a config cannot be read, no folder is removed.
- **From an `apply`.** Both take no instance lock, so an instance's own
  `apply` may call them while it holds the lock: for example `recipe fetch
  URL` (it prints the folder last), then a run of the new version's
  playbook from that folder, then `recipe use NAME X.Y.Z`. The new config
  counts from the next run; the `apply` that runs now is the old one.
  A rollback does not undo a `recipe use`: the config stays on the new
  version, and forward only keeps it there. A recipe's `apply` must be
  able to roll back with the new version's scripts.


## Windows file and token protections

Windows support is under development. The Windows file checks use a
handle opened without following the final reparse point. Managed files
and directories require a SYSTEM or Administrators owner and a DACL
that grants access only to those two SIDs. Files must have one hard link.
The runtime must validate parent directories before using these checks.

Token setup will encrypt the token with machine-scope DPAPI. The
encrypted file needs the same access checks as other protected files.

Windows state uses an exclusive byte-range lock. State updates write
and flush a private temporary file, then replace the old state using
a write-through move on the same volume. New state paths get an ACL
at creation. The Linux state paths continue to use Unix modes and
`flock`.

The Windows Git runner takes an absolute Git for Windows executable
from a trusted installation. It checks the protected clone and local
remote before invoking Git. The Windows export rejects symlinks and
paths that could name a device, an alternate stream, or a short alias.
