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

## 2. The rules

These rules come from issue #1. Each one has tests (section 14).

1. **The gate lives on the host, never in the repo.** What runs, who may
   approve and which paths get a warning come only from the host config:
   `/etc/pullgate/NAME.toml` and `/etc/pullgate/listener.toml`. pullgate
   never reads any setting from the repo. A commit cannot change the gate.
2. **One exact commit.** The question shows the full SHA. Apply applies only
   that SHA, even when a newer commit arrives in between.
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
| `/var/spool/pullgate/NAME/outbox` | root, group listener, 0770 | messages from root to the listener |
| `/var/spool/pullgate/NAME/inbox` | listener, 0700 | approvals from the listener to root |
| `/var/spool/pullgate/NAME/check` | listener, 0700 | check requests from the listener to root |
| `/var/lib/pullgate-listener/state.json` | listener, 0600 | the listener's open questions |

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

[listener]
user = "pullgate"         # the user of the listener; owns the inbox
```

Unknown keys are an error. An unknown mode or `on_failure` is an error. A
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
     was skipped, or its check failed, or it was refused, or it failed
     `max_failed_tries` times: nothing to do. `pullgate status NAME
     --ask-again` clears this mark.
   - tip is not a descendant of the applied SHA (an older commit, a force
     push, a rewritten branch) and `allow_older` is false: refuse. Send one
     notice for this SHA and mark it refused. When `allow_older` is true,
     ask, with a warning line at the top: "This commit is not a descendant
     of the applied commit." With no applied SHA, every tip is allowed.
   - the questions in the last 24 hours reached `questions_per_day`: log it,
     send one notice per day, and do not ask.
6. Export the tree of the tip, and the tree of the applied SHA
   (`PULLGATE_OLD_TREE`).
7. Run `check` as `check_user` (section 11). If it fails, mark the SHA
   "check failed", send a notice with the last lines of its output, and do
   not ask. The same SHA is not checked again.
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

A file the listener cannot parse is removed and logged. If Telegram refuses
a message, the listener tries again on the next round, and drops it after
one day with a log line.

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

`pullgate-check@NAME.path` watches the directory (`DirectoryNotEmpty=`) and
starts `pullgate-run@NAME.service`, the unit of the timer. `pullgate run`,
with the lock held:

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
  "expires": time, "open": true }
```

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
usage text, which lists the instances and never repeats the message. A
message of another user, or in another chat, gets no answer. The listener
never logs the text of a message: only why it was ignored and the sender
ID.

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
     rolled back to OLD". If it fails too, write the pause file, clear
     `in_progress` → and report "failed, the rollback failed too, the
     instance is paused". With no applied SHA there is nothing to roll back
     to: hold the SHA, as with `pause`.
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

**In a rollback**, `PULLGATE_SHA` and `PULLGATE_TREE` are the last good SHA
and its tree; `PULLGATE_OLD_SHA` and `PULLGATE_OLD_TREE` are the SHA that
just failed and its tree; `PULLGATE_CHANGED_FILES` lists the paths that
differ between them. So `apply` must be safe to run again on an older tree.

Their working directory is `PULLGATE_TREE`. Their umask is 022. Each has a timeout
(`check_timeout`, `apply_timeout`); on timeout the whole process group is
killed and the command counts as failed. pullgate keeps at most 64 KiB of
their output. The output of `apply` and `health` goes to the journal; the
report shows only the exit status. The output of `check` (its last lines)
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
⚠ The last apply (SHA) failed.            (only when a SHA is held)
⚠ This commit is not a descendant of …     (only with allow_older)
Apply this commit?
<code>FULL SHA</code>
First apply: there is no applied commit yet. All N files are listed.
or: Since <code>APPLIED SHA</code>:
⚠ Watched path changed: path
⚠ Symlink out of the tree: path → target
⚠ Submodule skipped: path

Commits (N):
• subject
Changed files (N):
• path
Summary:
<pre>output of the summary command</pre>
```

Text from the commit (subjects, paths, symlink targets, summary output) is
cleaned before it is shown: control and invisible characters become `?`
(newlines stay only in the summary output), each line is cut to 200
characters, and then it is HTML-escaped. At most 50 commit subjects are
read.

The message must fit one Telegram message (4096 characters). pullgate cuts
it to 3800 characters, counted on the escaped text in UTF-16 units: the head
(host, instance, SHA, the warnings) always stays, and when lines are left
out the last line says how many ("… 120 more lines left out"). The cut is on
whole lines, so it never splits an HTML tag or entity. The `<pre>` block is
closed after a cut.

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
the held SHA, the pause file and an apply in progress. Flags:

- `--ask-again`: clear the marks of the last fetched tip (expired, skipped,
  check failed, refused, failed, held), so the next run asks again;
- `--pause`: write the pause file;
- `--resume`: remove the pause file and clear an apply in progress.

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

## 15. Not in version 0.1

- Self-update of pullgate. `install.sh` updates it only when root runs
  the script (section 16).
- Signed commits (`git verify-commit`).
- A web UI.

## 16. The install script

`install.sh`, at the root of the repo, installs a release, or updates one.
It is a POSIX `sh` script. Root runs it on the host itself:

```sh
sh install.sh [--version X.Y.Z] [--bin-dir DIR] [--gh PATH] [--reinstall]
```

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
