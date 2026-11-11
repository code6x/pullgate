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
- **question**: one Telegram message that names one full SHA and has two
  buttons, Apply and Skip.
- **code**: a random one-time code of 128 bits (22 characters, base64url).
  The buttons carry it instead of the SHA.
- **approval**: a small file that the listener writes when the owner taps a
  button. It holds the code and the button (apply or skip). Nothing else.
- **tree**: a clean, read-only copy of the files of one commit, written by
  pullgate itself from git objects.
- **pause file**: `/var/lib/pullgate/NAME/paused`. While it exists, the
  instance does nothing.

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
   up the SHA in its own state.
5. **Root does the apply.** It acts only on an approval whose code matches a
   question that root recorded itself, and only within the time limit of that
   question. By default it moves forward only: the new SHA must descend from
   the applied SHA. Going to an older or unrelated commit needs the explicit
   setting `allow_older = true`.
6. **Root never runs git in a directory another user can write.** Before
   every git command, root checks the git directory and every directory above
   it (section 6). git runs with hooks off and without system or global
   config. pullgate writes the tree itself from git objects: no filters, no
   submodules, no LFS.

## 3. Processes and users

| process | user | started by | holds |
|---|---|---|---|
| `pullgate run NAME` | root | a timer (`pullgate-run@NAME.timer`) | the clone, the state, the deploy key |
| `pullgate apply NAME` | root | a path unit (`pullgate-apply@NAME.path`) when the inbox is not empty | the state |
| `pullgate listen` | `pullgate` (own user) | a service (`pullgate-listen.service`) | the bot token, its list of open questions |
| `check`, `summary` | `check_user` (default `pullgate-check`) | `pullgate run` | nothing |
| `apply`, `health` | root | `pullgate apply`, or `pullgate run` in `auto` mode | what the owner's command needs |
| `pullgate status NAME` | root | the owner | nothing |

Root and the listener never talk directly. They use a spool directory
(section 8).

## 4. Files

| path | owner, mode | what |
|---|---|---|
| `/etc/pullgate/NAME.toml` | root, 0600 or 0644, not writable by others | the instance config |
| `/etc/pullgate/listener.toml` | root, readable by the listener | the listener config |
| the deploy key | root, 0600 | read-only SSH deploy key |
| the known hosts file | root, not writable by others | the pinned SSH host key of the git server |
| the bot token file | the listener user, 0600 (or a systemd credential) | the bot token |
| `/var/lib/pullgate/NAME/` | root, 0711 | the state directory |
| `/var/lib/pullgate/NAME/repo.git` | root, 0700 | the bare clone |
| `/var/lib/pullgate/NAME/state.json` | root, 0600 | the state (section 9) |
| `/var/lib/pullgate/NAME/lock` | root, 0600 | the lock (`flock`) |
| `/var/lib/pullgate/NAME/paused` | root | the pause file |
| `/var/lib/pullgate/NAME/trees/SHA` | root, group `check_user`, 0750; files 0444 or 0555 | the trees |
| `/var/spool/pullgate/NAME/outbox` | root, group listener, 0770 | messages from root to the listener |
| `/var/spool/pullgate/NAME/inbox` | listener, 0700 | approvals from the listener to root |
| `/var/lib/pullgate-listener/state.json` | listener, 0600 | the listener's open questions |

The paths can be changed with `--config-dir`, `--state-dir` and
`--spool-dir`, for tests.

pullgate refuses to start when the instance config, the deploy key or the
known hosts file is a symlink, is not owned by root, or can be written by
group or others. The deploy key must also not be readable by group or
others. In tests, "root" means the user that runs the test.

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
max_failed_tries = 1      # default 1
max_files = 100000        # default 100000 files in one tree
max_tree_bytes = "1GiB"   # default 1 GiB in one tree
allow_older = false       # default false (rule 5)

[listener]
user = "pullgate"         # the user of the listener; owns the inbox
```

Unknown keys are an error. An unknown mode or `on_failure` is an error. A
missing `apply` is an error. Every error names the file, the key and the fix.

There is no default for `on_failure` in the example configs: each example
sets it. The code default is `pause`, because it is the safe choice.

## 6. Git, safely (rule 6)

Before each git command, root checks the **git directory**:

- the git directory and each directory above it up to `/` is owned by root
  (the running user) and is not writable by group or others. A parent
  directory that is writable by others is allowed only when it has the
  sticky bit and is owned by root (like `/tmp`), because then no other user
  can rename or remove our directory in it;
- every file and directory inside the git directory is owned by root, is not
  writable by group or others, and is not a symlink, except inside
  `objects/`, where only regular files and directories may be.

If a check fails, pullgate stops with an error that names the path, and runs
no git.

Each git command runs:

- with an empty environment, plus `PATH`, `HOME` set to the state
  directory, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`,
  `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=` and `GIT_SSH_COMMAND` (below);
- with `-c core.hooksPath=/dev/null -c core.fsmonitor=false
  -c protocol.allow=never -c protocol.ssh.allow=always
  -c protocol.file.allow=always -c submodule.recurse=false` before the
  subcommand (`file` is for tests and local
  mirrors; `http` and `git` are off);
- with `--git-dir` set, never in a work tree;
- with a timeout and a cap on its output.

The fetch is:

```
git fetch --no-tags --no-recurse-submodules --no-write-fetch-head \
  URL +refs/heads/BRANCH:refs/pullgate/tip
```

`GIT_SSH_COMMAND` is `ssh -F /dev/null -i KEY -o IdentitiesOnly=yes
-o IdentityAgent=none -o UserKnownHostsFile=KNOWN_HOSTS
-o StrictHostKeyChecking=yes -o BatchMode=yes`. The URL comes from the host
config. It must not start with `-`, so git cannot read it as an option.

Then `git rev-parse --verify refs/pullgate/tip^{commit}` gives the tip.

**The tree.** pullgate never uses `git checkout` or `git archive`, because
both apply `.gitattributes` (filters, `export-subst`, `export-ignore`). It
reads `git ls-tree -r -z --full-tree SHA` and then each blob with
`git cat-file --batch`. It writes the files itself:

- Each path part must not be empty, `.`, `..` or `.git` (in any case), and
  must not hold a NUL or `/`. A bad path stops the export.
- Regular files are written 0444 (0555 when the git mode is 100755), with
  `O_CREAT|O_EXCL|O_NOFOLLOW`, inside directories that pullgate created in
  this export.
- Symlinks (mode 120000) are written as symlinks. pullgate never follows a
  symlink of a tree. A command that reads the tree must not follow them
  either, or must check where they point.
- Submodules (mode 160000) are skipped and listed in the summary as a
  warning.
- The export stops when the tree has more than `max_files` entries or more
  than `max_tree_bytes` bytes.
- When the export ends, each directory gets mode 0555, and the top
  directory gets 0750 with group `check_user`.

## 7. One run (`pullgate run NAME`)

1. Read the config. Take the lock (`flock`, no wait). If another run or
   apply holds it, log "busy" and exit 0. If the pause file exists, log
   "paused" and exit 0. If `mode = "off"`, log "off" and exit 0.
2. If the state shows an apply in progress (section 10), write the pause
   file, report, and exit.
3. Make sure the clone, the spool directories and their owners and modes are
   right (section 4). Fetch (section 6).
4. Decide:
   - tip = applied SHA: nothing to do. If a question is open, close it
     ("the branch is back to the applied commit").
   - tip = the SHA of the open question: nothing to do.
   - tip was asked before and the question expired or was skipped, its check
     failed, it was refused, or it failed `max_failed_tries` times: nothing
     to do. `pullgate status NAME --ask-again` clears this mark.
   - tip is not a descendant of the applied SHA (an older commit, a force
     push, a rewritten branch) and `allow_older` is false: refuse. Send one
     notice for this SHA and mark it refused. When `allow_older` is true,
     ask, with a warning line at the top: "This commit is not a descendant
     of the applied commit."
   - questions in the last 24 hours reached `questions_per_day`: log it,
     send one notice per day, and do not ask.
5. Export the tree of the tip, and the tree of the applied SHA if it is not
   there yet (`PULLGATE_OLD_TREE`).
6. Run `check` as `check_user` (section 11). If it fails, mark the SHA
   "check failed", send a notice with the last lines of its output, and do
   not ask. The same SHA is not checked again.
7. Run `summary` as `check_user`, and build the message (section 12).
8. If a question is open, close it: root marks it superseded in its state,
   and asks the listener to edit it ("a newer commit replaced this
   question"). A tap that the listener took before the edit still counts
   for the old SHA (rule 2); section 9 says how.
9. `ask`: make a new code, record the question in the state (synced to disk)
   and write the question to the outbox. `auto`: do the apply right away
   (section 10), without a question.

The run removes trees that no question, no apply and no applied SHA uses.

## 8. The spool

Each instance has `/var/spool/pullgate/NAME/outbox` and `.../inbox`.

**Outbox** (root writes, the listener reads and removes). One JSON file per
message, named `SEQ.json` with a 20-digit sequence number, written to a
temporary name and renamed. The listener handles them in name order. Kinds:

- `ask`: `question` (an id, unique per instance), `code`, `expires`
  (time), `text` (HTML, ready to send);
- `edit`: `question`, `text`: replace the text of that question and remove
  its buttons. The listener forgets the code of the question;
- `notice`: `text`: a new message;
- `reply`: `question`, `text`: a reply to that question.

A file the listener cannot parse is moved to `outbox/bad/` and logged. If
Telegram refuses a message, the listener tries again later, at most for one
day, then drops it with a log line.

**Inbox** (the listener writes, root reads and removes). One file per tap,
`SEQ.json`, at most 1 KiB, with `{"code": "...", "action": "apply" | "skip"}`.
Unknown fields are an error. Root opens each file with `O_NOFOLLOW`, needs a
regular file owned by the listener user, reads at most 1 KiB, and removes the
file before it acts. A bad file is logged and removed.

`pullgate-apply@NAME.path` watches the inbox (`DirectoryNotEmpty=`) and
starts `pullgate apply NAME`.

## 9. Questions and approvals

Root keeps its questions in `state.json`:

```
{ "id": 7, "code": "…", "sha": "…", "asked": time, "expires": time,
  "open": true }
```

At most one question is open. A superseded question stays in the list
(`"open": false`) until it expires, so that a tap that the listener took
before the close still counts (rule 2). The listener forgets a code when it
handles the `edit` that closes the question, so it cannot take a tap for
it after that. Every list entry is removed when it is answered or expires.

**The listener** accepts a tap (a `callback_query`) only when all of these
hold, else it answers "This question is closed." and writes nothing:

- `from.id` is `owner_id`, and `message.chat.id` is `chat_id`;
- the data is `a:CODE` or `s:CODE`, and CODE is a code that it holds;
- the message ID is the message ID of that question;
- the question has not expired by the listener's clock;
- the question has no tap yet. After the first tap the listener forgets the
  code, so a second tap does nothing.

Then it writes the approval, edits the question ("Apply tapped. Waiting for
the host." or "Skipped."), and answers the tap.

**Root** (`pullgate apply NAME`) takes the lock (with a wait), reads every
approval in the inbox and removes it, and for each one:

1. finds the question with that code in its own state, with a constant-time
   compare. No match: log and ignore.
2. checks `asked <= now < expires` by its own clock. Else: ignore, and drop
   the question.
3. removes the question from its state (synced), so the code works once.
4. `skip`: marks the SHA skipped, reports, done.
5. `apply`: takes the SHA **from its own state**, never from the approval.
   It checks again that the SHA descends from the applied SHA (unless
   `allow_older`), then applies it (section 10).

Questions per day, the approval time limit and the failed tries per SHA are
limits in the config (section 5).

## 10. The apply

Before it runs the `apply` command, root writes `in_progress = {sha,
old_sha, started}` to the state and syncs it. Then:

1. Run `apply` as root, in the tree of the SHA, with the environment of
   section 11. Then `health`, if it is set.
2. Both succeed: the applied SHA becomes this SHA, `in_progress` is cleared,
   the failed mark is cleared, the state is synced. Report "applied".
3. One fails: count a failed try for this SHA. Then by `on_failure`:
   - `rollback`: when there is an applied SHA, run `apply` again with the
     last good SHA (the applied SHA, its tree as `PULLGATE_TREE`), then
     `health`. If that works, report "failed, rolled back to OLD". If it
     also fails, write the pause file and report "failed, the rollback failed
     too, the instance is paused". With no applied SHA there is nothing to
     roll back to: hold the SHA as in `pause`.
   - `pause`: do not roll back and do not write the pause file. Hold the
     failed SHA: it is not tried again, every report and `pullgate status`
     say that the last apply failed, and a **newer** commit is still asked,
     with the line "The last apply (SHA) failed." at the top. So the owner
     can fix it forward with a new commit.
   In both cases `in_progress` is cleared and the applied SHA does not
   change. State moves forward only after success.

A SHA with `max_failed_tries` failed tries is not asked again until a new
commit (or `--ask-again`). With `on_failure = "pause"` a failed SHA is held
at once.

**A crash** between the start of `apply` and the state write leaves
`in_progress` in the state. The next run or apply cannot know what happened
on the host. It writes the pause file, marks the SHA failed and reports
"the apply of SHA was cut off; check the host, then remove the pause file
with `pullgate status NAME --resume`".

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
- `PULLGATE_CHANGED_FILES`: the path of a read-only file with the changed
  paths, each one ended by a NUL byte (all paths on the first apply). Read
  it with `xargs -0`. A file, because a list of paths can be too long for
  the environment and a path can hold a newline.

Their working directory is `PULLGATE_TREE`. Each has a timeout
(`check_timeout`, `apply_timeout`); on timeout the whole process group is
killed and the command counts as failed. pullgate keeps at most 64 KiB of
their output. The output of `apply` and `health` goes to the journal; the
report shows only the exit status. The output of `check` (its last lines)
and of `summary` goes into Telegram messages, after the cleaning of
section 12.

**check and summary (rule 3)** run as `check_user`, with its own group and
no other groups. When pullgate runs as root, `check_user` must not be root
(uid 0): pullgate refuses such a config. When pullgate does not run as root
(tests, a trial), it cannot change user, so `check_user` must be the user
that runs it. The run unit's sandbox (read-only system, private `/tmp`, no
new privileges) also applies to them.

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

Commits (N):
• subject
Changed files (N):
• path
⚠ Watched path changed: path
⚠ Submodule skipped: path
Summary:
<pre>output of the summary command</pre>
```

Text from the commit (subjects, paths, summary output) is cleaned before it
is shown: control and invisible characters become `?` (newlines stay only
in the summary output), each line is cut to 200 characters, and then it is
HTML-escaped. At most 50 commit subjects are read.

The message must fit one Telegram message. pullgate cuts it to 3800
characters: the head (host, instance, SHA, warnings) always stays, the
watch warnings come before the lists, and when lines are left out the last
line says how many ("… 120 more lines left out"). The cut is done on
whole lines, before the lines are joined, so it never splits an HTML tag or
entity.

## 13. Modes, limits, status

- `ask` (the default): as above.
- `auto`: no question; `pullgate run` applies the tip itself after the check
  passes, with the same steps (section 10), and reports. For development
  hosts.
- `off`: `pullgate run` does nothing.

`pullgate status NAME` prints the mode, the applied SHA, the open question,
the held failed SHA, the pause file and an apply in progress. Flags:

- `--ask-again`: clear the marks of the tip (expired, skipped, check
  failed, failed), so the next run asks again;
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
commit was applied.

## 15. Not in version 0.1

- Self-update of pullgate.
- Signed commits (`git verify-commit`).
- A web UI.
