# pullgate threat model

Status: accepted for version 0.1 on 2026-11-11.

This file says what pullgate protects, from whom, and where it stops. Read
[DESIGN.md](DESIGN.md) first for the words and the flows.

## 1. The problem in one paragraph

A host that pulls its own config from a git repo changes whenever someone
pushes to that repo. Whoever can push (a person, a CI job, a stolen key)
can change the host, often as root. pullgate puts a person between the push
and the change: the host applies one exact commit only after its owner taps
Apply on a question that names that commit, and the rules of that gate live
on the host, out of reach of the repo.

## 2. What we protect

| asset | where it lives |
|---|---|
| the host itself (root) | the host |
| the gate: the instance config, the listener config, the units, the binary | `/etc/pullgate`, `/etc/systemd/system`, the binary path |
| the state: applied SHA, open questions, failed marks | `/var/lib/pullgate/NAME/state.json` |
| the deploy key (read-only) | `/etc/pullgate` |
| the bot token | the listener's token file or systemd credential |
| the approval decision | Telegram, the listener, the state |

## 3. Who we trust, and how much

| actor | trust |
|---|---|
| the owner (the person who taps) | Trusted. Their judgment is the check. |
| the owner's Telegram account and phone | Trusted. Anyone who can use this account can approve. |
| Telegram's servers | Trusted to deliver messages and to say truthfully who tapped. They see the question text: SHAs, commit subjects, file names and the summary output. |
| root on the host | Fully trusted. |
| the listener process and its user | Trusted only to pass on the owner's taps and `/check` messages. If it is taken over, it can approve an open question without the owner (T2), and start runs (at most one a minute, T18). It can never choose the SHA. |
| `check_user` and the `check`/`summary` commands | Not trusted with root. They read hostile data (T3). |
| the git server and everyone who can push | **Not trusted.** A commit may be hostile. |
| the network | Not trusted. SSH with a pinned host key protects the fetch. |
| other local users | Not trusted. |
| the `apply` and `health` commands | Trusted: the owner wrote them. They run as root on the approved tree. |
| GitHub, the release workflow on `main`, and the gh that `install.sh` pins | Trusted to build and serve the release that `install.sh` installs (T16, T17). |

## 4. What pullgate promises

1. **No change without a tap.** In `ask` mode, root runs `apply` only for a
   SHA whose question the owner approved (or that a taken-over listener
   approved, T2).
2. **The SHA is root's own.** The applied SHA is the one root put in the
   question. Nothing outside root's state can choose it.
3. **No code from a new commit runs before the tap, and never as root.**
4. **Forward only**, unless the host config says otherwise.
5. **The gate is not in the repo.** No commit can change who approves, what
   runs, or what is watched, except by an approved apply that installs it
   (T5).

## 5. Threats

Each threat says what an attacker can try, what stops it, and what is left.

### T1. A stolen bot token

*Attack.* Someone gets the bot token. They can call the Bot API as the bot:
read updates, send messages, edit them.

*Defense.* A token cannot make a tap from the owner's user ID; Telegram signs
nothing, but it reports the real sender of a `callback_query`, and the
listener checks `from.id` and `chat.id`. A thief who runs their own
`getUpdates` steals the updates from the listener (two pollers conflict),
so taps can get lost: lost taps deny, they do not approve. The thief can
send a fake question to the owner, with buttons whose codes the listener
does not hold; a tap on it does nothing, because the listener holds the
only list of codes, and root checks the code against its own state. The
thief can also read new taps of the owner (the code of an open question)
but a code alone is useless without write access to the inbox.
The thief can also edit the text of a real question, so that harmless text
sits above real buttons for a hostile SHA. The listener counts a tap only on
a message that was never edited, whose text is the text Telegram gave back
when the listener sent it (DESIGN.md section 9).

*`/check`.* A token cannot send a message as the owner either. A message
that the thief sends to the bot, or in another chat, comes with the
thief's own `from.id` (or the wrong `chat.id`); the listener ignores it,
gives no answer and logs no text (DESIGN.md section 9). The thief can read
the owner's `/check` messages, but that tells only that the owner wants a
check. Even a request that the thief could forge would only start a normal
run: it fetches and asks; in `ask` mode it never applies.

*Left.* The thief can show the owner false text (a fake "applied"
message), can hide questions, and can see the question text. Keep the
token in a file only the listener user can read, or a systemd credential.
Change it with BotFather when in doubt.

### T2. A compromised listener

*Attack.* An attacker runs code as the listener user.

*Defense.* The listener user can write only to the inboxes and the outboxes.
An approval holds a code; root looks the code up in its own state, so the
listener cannot name a SHA, cannot approve a commit that root did not ask
about, cannot pass the time limit, and cannot replay a used code. Root reads
inbox files with `O_NOFOLLOW`, caps their size, needs the listener as owner,
and rejects unknown fields. The forward-only rule still holds. The listener
user can write in the outbox and the inbox. Root never follows a path there:
the spool directories above them are root-owned, root opens them with
`os.OpenRoot`, and creates files with `O_CREAT|O_EXCL|O_NOFOLLOW` under a
random name. So a symlink that the listener plants cannot make root write
or change a file elsewhere. Root gives an outbox file the listener's group
with `fchown` on the open file, never through a path, and no spool
directory has the setgid bit. So the run unit keeps `RestrictSUIDSGID=yes`
(DESIGN.md section 4).

*Check requests.* The listener can also write check requests at any time,
without the owner. A request holds no data: root checks it like an
approval (`O_NOFOLLOW`, regular file, owned by the listener, at most 64
bytes) and also needs the fixed name and content. It cannot name a SHA,
an instance other than its own directory, or an action. It can only start
the normal run sooner, with the same sandbox, lock and rules. Root keeps
the limit (one run per minute from requests) in its own state, so the
listener cannot make root fetch more often by writing files; a bad file
is not a request (T18).

*Receipts.* The listener writes a receipt for each outbox file it handled
(DESIGN.md section 8). Root reads them with the checks of an approval
(`O_NOFOLLOW`, regular file, owned by the listener, at most 16 bytes, one
of two fixed contents, at most 1000 per run). A receipt only changes a log
line (`asked`, or the warning `question not delivered`) and the `delivery`
line of the status. It is never an approval. A fake `sent` receipt hides
a lost question, which a compromised listener can do anyway by not
sending it.

*Left.* It can approve any open question (or a superseded one that has not
expired) without the owner, and it can hide or fake messages. So the
attacker still needs a hostile commit in the repo **and** the listener.
With check requests it can make root fetch and ask once a minute, and make
an `auto` instance apply a new commit sooner than the timer would; never a
commit that the timer would not apply.
Run the listener as its own user, with the hardened unit.

### T3. A hostile commit

*Attack.* Someone who can push writes a commit made to hurt the host before
or during the review.

- **Its content.** Before the tap, no file of the commit runs. git only
  reads objects. `check` and `summary` read files as data, as `check_user`
  (not root), in a read-only tree, inside the run unit's sandbox. A parser
  bug in them hits only `check_user`. They have a timeout and an output cap.
  A process of `check_user` that leaves its process group to fake a later
  check or summary is stopped with `kill -KILL -1` as `check_user` after
  each command, and by systemd when the run unit ends. Use one `check_user`
  per instance.
- **A tree that changed.** Trees are written to a temporary name and renamed
  only when complete. Each process exports its own fresh trees and removes
  them at the end; `apply` never runs on a tree that a check, an earlier
  apply, or a crash left behind.
- **File names.** Each path part is checked: no `..`, no `.git`, no empty
  part, no NUL. Names with control or invisible characters, or with HTML,
  are cleaned and escaped before they are shown, so a name cannot fake a
  line of the question.
- **Commit subjects and summary output.** The same cleaning; HTML-escaped,
  each line capped. The output of a failed `check` keeps its end, so the
  reason is not cut away; root's tree paths in it become `NEW` and `OLD`.
  The head of the question (host, instance, SHA, the warnings) always
  comes first, before any text from the commit.
- **`WARN: ` lines of the summary in the head.** `summary` runs as
  `check_user` on the commit's tree, so a hostile commit may make it print
  `WARN: ` lines. They are shown only as cleaned code after `⚠ `, at most 10
  and a count, after the watched-path warnings. They never push out the
  fixed head lines (host, instance, SHA, the broken and held lines). Each
  is at most 220 characters after the escape, so all 10 take at most about
  2250 of the 3400 that the head may use: the watched-path warnings, root's
  own, always keep room. Path warnings that still do not fit are counted
  ("⚠ … and N more warnings"), as before. *Left:* a hostile summary can
  print decoy `WARN: ` lines first and push the owner's real ones into the
  "N more warnings from the summary" count; the owner's `summary` should
  print its own warnings first. They change nothing that root checks: the tap is bound to
  the code and the SHA in root's state.
- **The report file of `apply`.** `apply` runs as root and is the owner's
  own command, but it may copy text from the tree. Root makes the file
  itself, in its own trees directory, and reads it only when it is still
  that same file (same device and inode, a regular file, opened with
  `O_NOFOLLOW`), at most 64 KiB. So `apply` cannot make root show another
  file by a symlink or a hard link. Its lines are cleaned and shown as code, at most
  10 lines and 1500 characters. A rollback and `health` get no report file.
- **Hostile text shown as a link in a trusted message.** A commit picks its
  own file names, subjects and command output. Telegram turns text that
  looks like a domain or a URL (`README.md`, `login.example.com`), an
  `@username`, a `#tag` or a `/command` into something to tap, in a message
  from the owner's own bot, next to the Apply button. So all text from the
  repo is shown only inside `<code>` or `<pre>`, where Telegram makes none
  of these, and only through one function (DESIGN.md section 12). Link
  previews are off for every message, so a link that slips through still
  shows no preview. A test feeds hostile values into every field of every
  message kind.
- **A short SHA that looks right.** Messages show 12 characters of a SHA
  (48 bits). Someone could grind a commit whose short SHA matches one the
  owner expects. This does not change what is applied: root applies the
  full SHA from its own state, and the tap is bound to the question code,
  not to the text. The question also shows the full SHA once, on its last
  line, and `pullgate status` shows the full applied SHA.
- **Symlinks.** They are written as symlinks and never followed by
  pullgate. The tree is written with `O_NOFOLLOW|O_EXCL` into directories
  that pullgate made itself, so a symlink cannot make pullgate write
  outside the tree. A repeated name, or a path below a file or symlink,
  stops the export; git checks the objects on fetch (`transfer.fsckObjects`).
  A symlink whose target is absolute or holds `..` gets a warning in the
  head of the question, so the owner sees it before root's `apply` may
  follow it; `refuse_symlinks = true` refuses such a commit. A command that
  reads the tree must not follow symlinks blindly (the docs say so).
- **Large trees.** `max_files` and `max_tree_bytes` stop the export. The
  question has a fixed size. git output is capped.
- **`.gitattributes` filters.** pullgate never checks out and never runs
  `git archive`; it writes blobs itself, so no `filter`, `textconv`,
  `export-subst` or `export-ignore` runs. git runs without system and
  global config, so no filter is defined anyway.
- **Submodules.** Not fetched, not written (`--no-recurse-submodules`, mode
  160000 is skipped), listed as a warning.
- **LFS.** Not used. An LFS pointer file is written as the small text file
  it is.
- **A hostile commit that the owner approves.** The apply then runs as root
  on purpose. pullgate cannot stop a bad change that the owner approves; it
  makes sure the owner sees the full SHA, the subjects, the changed files,
  and a warning for each watched path.

### T4. Root runs git where another user can write

*Attack.* If another user can write the git directory, they can set
`core.fsmonitor`, hooks, or `core.sshCommand` in `config`, and git runs
it as root.

*Defense.* Rule 6: root owns the clone in a root-only directory, checks the
git directory, all its contents and all parents before every git command,
and refuses on any problem. git runs with `core.hooksPath=/dev/null`,
`core.fsmonitor=false`, no system or global config, and an empty
environment.

### T5. An approved apply that changes the gate itself

*Attack.* The repo holds the instance config, a unit, or the binary, and
`apply` installs them. A commit that weakens the gate (`mode = "auto"`, a
new owner ID, no watch list) is approved by mistake.

*Defense.* pullgate cannot know what `apply` installs. It reads its config
once per process, so the running apply and its rollback use the gate of
the process that started them. The owner should keep the gate files out of
the repo, or list them in `watch`, so the question shows "⚠ Watched path
changed" for them. The listener's owner ID and chat ID are in a separate
file that `apply` should not touch.

*A rollback after such an apply.* If the new gate fails `health`, the
rollback runs the **old** apply on the old tree, with the commands of the
current process (the old gate). So a rollback puts the old gate files back
when `apply` installs them. If the new gate did break pullgate itself (for
example a bad binary that `apply` installed), the next run fails to start
and the owner sees the failed unit; the gate does not repair what `apply`
installed. Let `apply` not install pullgate: `/update` does that, with
every check of `install.sh` and a way back (T20).

*Left.* An approved commit can change everything. This is the price of
having the host manage itself.

### T6. A rollback that fails

*Attack or accident.* `apply` fails, and running it again on the old tree
fails too. The host may be half changed.

*Defense.* With `on_failure = "rollback"`, a failed rollback makes the
instance **broken** (DESIGN.md section 10) and reports it once, with the
reason. Nothing is applied until the owner taps Apply on a **fix
forward**: a newer commit on top of the failed one, asked with a strong
warning. The failed commit and the old commit are never asked again, and
no rollback is tried against the half state. In `auto` mode the owner is
asked too. Only a fix forward that passes `health`, or root with
`pullgate status NAME --resume`, ends it. It never ends by itself, and no
message from Telegram ends it, so a stolen bot token or a compromised
listener (T2) cannot lift it: the most they can do is not deliver a tap.
A pause file still stops everything, also while broken. With `on_failure = "pause"` there is no
rollback: the failed SHA is held, and the owner can fix it forward. The
applied SHA does not change after a failure.

### T7. A force push or a rewritten branch

*Attack.* The branch is moved to a commit that does not descend from the
applied SHA, maybe to undo a fix or to hide history.

*Defense.* Forward only: the run refuses the tip, sends one notice, and marks
the SHA refused. Root checks it again at apply time. Only
`allow_older = true` in the host config allows it, and then the question
has a warning line.

### T8. An old commit offered again

*Attack.* The branch is set back to an older commit that was skipped,
failed, or was applied earlier (a downgrade).

*Defense.* An ancestor of the applied SHA is refused (T7). A SHA that was
skipped, failed its check, failed its apply, or whose question expired, is
not asked again unless `--ask-again` is used. The state keeps these marks.

### T9. A lost or stale approval

*Attack or accident.* A tap is lost; or a tap arrives late; or a tap is
replayed; or an old message is tapped.

*Defense.* A lost tap means no apply (fail closed); the question expires and
the SHA waits for a new commit or `--ask-again`. The listener forgets a code
after the first tap and after the question is closed, so a second tap, or a
tap on a closed or expired question, writes nothing. The listener also
keeps the last 200 update IDs it handled and ignores an update with one of
them, a tap or a `/check` alike. Root removes a question
from its state before it acts, so a replayed approval finds no question.
Root checks the time limit itself. `--ask-again` removes an open question
for the tip from root's state (its message may be lost), so a tap on it,
even one already written, finds no question; the new question has a new
code. A question lost in the outbox is not silent: the listener tells the
owner when it removes an outbox file that it cannot read, and names the
instance, but shows no part of the file. A tap on a superseded question that the
listener took before it handled the close still applies the old SHA
(rule 2); the forward-only check still holds then.

### T10. A crash between apply and the state write

*Accident.* Power is lost while `apply` runs, or just after it, before the
state is written.

*Defense.* Root syncs `in_progress` to disk before it starts `apply`. On the
next start, `in_progress` means "unknown result": pullgate writes the pause
file, marks the SHA failed, and reports. It does not guess. The state is
written to a temporary file, synced, and renamed, so it is never half
written.

*The update of pullgate itself* (T20) has the same rule. Root syncs
`in_progress` before it touches the binary. The old binary gets a second
name (`.pullgate.prev`, a hard link in the same directory). `install.sh`
writes the new file under a temporary name, syncs it, and renames it over
`pullgate`; a rollback renames `.pullgate.prev` back. A rename in one
directory replaces the name in one step, so after a crash or a power loss
the host has the old binary or the new one, never none and never a part of
one. The next start of `pullgate update` sees `in_progress`, reports "the
update was cut off" with what `pullgate version` says, and does not guess.
A test stops the update at each step and checks this
(`TestCrashInTheSwap`).

### T11. A wrong clock

*Accident or attack.* The host clock jumps forward or back.

*Defense.* Time limits are checked by root with its own clock against times
it recorded itself: an approval counts only when `asked <= now < expires`.
A clock that goes back before `asked` denies. A clock that jumps forward
expires questions early: this denies too. The listener checks expiry with its
own clock as well. Timeouts of commands use the monotonic clock.

`settle` counts from the time root first saw the tip; a clock that goes
back before that time starts the wait again. A clock that is wrong and then
corrected forward can make the wait shorter, like `approval_ttl` above.
That only asks sooner; it never applies anything.

*Left.* A clock that is set far back while a question is open can keep that
question valid longer than `approval_ttl`. The code still works only once.

### T12. A replayed or forged inbox file

*Attack.* A local user, or the listener, puts files in the inbox.

*Defense.* Only the listener user can write the inbox. Root needs the
listener as the file owner, a regular file, at most 1 KiB, valid JSON
without unknown fields, and a code that matches its state. A bad file is
removed and logged. The path unit then has nothing left and stops.

*The check directory.* A check request has no code, so a code cannot
protect it. It needs none, because it cannot cause an apply: it only
starts the normal run, which asks (or, in `auto` mode, does what the timer
does). Only the listener user can write the directory. Root needs the
listener as owner, a regular file through `O_NOFOLLOW`, at most 64 bytes,
the name `request` and the content `check\n`. Root removes every file in
the directory before it acts, and a bad file is not a request.
`pullgate apply` never reads this directory, and a check request put in
the inbox is a bad approval. A run that finds only bad files removes them
and goes on as a normal run, without a reply. The limit of T18 holds for
every valid request; the trigger limit of the path unit holds for every
file.

### T13. A hostile git server or network

*Attack.* A fake server, or a server that sends huge or broken data.

*Defense.* SSH with `StrictHostKeyChecking=yes` and a pinned known hosts file
from the host config; a read-only deploy key; no other protocols. git has
a timeout and an output cap. The tree export has caps.

*Downloads of releases* (T17, T20). For `/update`, root also downloads
from GitHub, with the built-in `install.sh`: HTTPS only, also after a
redirect (`--proto '=https' --proto-redir '=https'`), TLS 1.2 or newer, a
size limit for each file (1 MiB, 64 MiB for a tarball), a time limit for
each download and 20 minutes for the whole script. A redirect may go to
another HTTPS host (GitHub serves its files from other hosts); the
SHA-256 and the attestation, not the host name, prove the file. The
script takes only the member `pullgate` out of the tarball, and only
after the attestation passed. GNU tar refuses members with `..` and
removes a leading `/`; a `pullgate` that is a link, a device or not a
regular file is refused. Root never runs anything else from the tarball.

### T14. Weak gate files

*Attack.* A local user can write the config, the deploy key, the known
hosts file, the program of a command, or a directory above one of them.

*Defense.* pullgate refuses to start then (DESIGN.md section 4). A local
repo URL must pass the same check as the clone, because root runs
`git-upload-pack` in it. The listener checks its own config and token file.
`pullgate doctor` runs the same checks on demand, and also reads back the
owners and modes of the state and the spool, so a weak file shows before
a run needs it (DESIGN.md section 18). Doctor runs as root and starts
itself as the listener user only to read one byte of its own test file
and of `listener.toml` (`_doctor-read` prints nothing of the file). As
the listener user it can read only what the listener can read.

### T15. Too many questions

*Attack.* Someone pushes many commits to tire the owner into tapping Apply.

*Defense.* At most one open question per instance; a newer tip replaces it.
`questions_per_day` caps the questions. A SHA is asked once, or once more
per `reask_after` after a Skip or an expiry; each of these counts against
`questions_per_day`. With `settle`, a tip is asked only when root has seen
it unchanged for that long, by root's own clock (not the commit date, which
the writer sets). So a writer who keeps moving the branch gets no question
at all, and the owner always has a question that stays the same while he
reads it.

*Check exit 3.* `check` runs as `check_user` on the commit's tree, so a
hostile commit can make it exit 3. That only marks the SHA `nothing`: no
question, no apply, and the applied SHA stays. It cannot apply anything,
and it is no worse than a check that fails. The next real commit is asked
with every commit since the last real apply, so nothing is hidden from the
owner. Root takes only the exit status; it reads nothing else from the
check.

*Left.* A writer can delay a question by pushing more often than `settle`.
That only delays; it never applies.

### T16. A bad release binary

*Attack.* The `pullgate` binary runs as root. Whoever can publish a release
ships code to root on every host that installs it.

*Defense.* A release is the merge of the release pull request on `main`
(docs/releasing.md). Only the release App can create a `v*` tag, nobody can
move one, releases are immutable, and the `release` environment needs the
owner's approval. The build job is read-only and checks that the tag is the
commit of the run. Each tarball has a SHA-256 in `SHA256SUMS` and a build
provenance attestation tied to `release.yml` on `main`. Check both before
you install (docs/releasing.md, "Verify a download"), or let `install.sh`
check them (T17).

### T17. A bad install path

*Attack.* `install.sh` runs as root and puts a binary that root runs later.
An attacker tries to make it install a file that the release workflow did
not build, or an older build under a newer name. Or a local user changes a
file or a directory that the script uses while it runs. Or the
environment of root changes what `tar`, `curl` or `gh` do.

*What the script trusts.*

- **GitHub**: to serve the release files and the answer of its API over
  HTTPS, and to run the release workflow on its own runners.
- **The release workflow identity**: only
  `https://github.com/code6x/pullgate/.github/workflows/release.yml@refs/heads/main`,
  on a runner that GitHub hosts, may have built the tarball. Sigstore's
  public trusted root says whether the attestation is valid.
- **The pinned gh**: gh 2.102.0, checked against a SHA-256 for each CPU that
  is written in the script. Or a gh that the owner gives with `--gh`: it
  must be 2.93.0 or newer, and only root may change it.
- **The script itself**: the owner downloads it over HTTPS and reads it
  before running it.
- **The host**: root, and the programs in `/usr/sbin:/usr/bin:/sbin:/bin`.

*Defense.* (DESIGN.md section 16.) The script installs nothing unless every
check passes: exactly one line in `SHA256SUMS` for the tarball, and its
SHA-256 matches; `gh attestation verify` with the bundle of the release,
`--repo code6x/pullgate`, the identity above and
`--deny-self-hosted-runners`; the new binary prints `pullgate X.Y.Z` of the
version asked for. The attestation proves which workflow built a file, not
its version, so the version check stops an older build under a newer name.
The new binary runs only after the attestation passed. Every download has
a size limit, uses HTTPS only with TLS 1.2 or newer, and reads no
`.curlrc`. The script removes a list of variables that change `tar`,
`gzip`, `curl`, OpenSSL, gh, name lookups, libc and the dynamic loader, sets
`LC_ALL=C`, and as root uses a fixed `PATH`. pullgate and gh run with a time
limit (`timeout`), and curl has its own. It writes the new file to the disk before the rename. gh runs with no token and an empty config. Only root may
change the bin directory, the old binary, `TMPDIR`, the new temporary
directory and the `--gh` file, and each directory above them. A directory
above them that others can write is accepted only with the sticky bit, as
`/tmp` has. The old
binary must be a regular file, never a link. The new file replaces the old
one by a rename in the same directory, as the last step.

*What is left.* Whoever controls the release workflow on `main` (T16), or
GitHub itself, can ship a binary that passes. The script cannot check
itself: a changed copy of `install.sh` can do anything, so read it before
you run it. The list of removed variables is a block list: a variable that
is not on it, or one that acts before the first line runs (such as
`SHELLOPTS` when `sh` is bash), still reaches the programs. Who controls
root's environment controls root already; to be sure, start the script
with `env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin sh install.sh`. The proxy
variables stay on purpose: TLS and the attestation protect what comes
through a proxy. The old binary runs once, as root, to print its version; only
root can change it, so this gives nothing new to an attacker. The script
restarts nothing.

### T18. Many check requests

*Attack.* The owner, by mistake, or a taken-over listener sends many
`/check` requests, so the host fetches all the time and loads the git
server.

*Defense.* Root allows at most one run per minute from the check directory.
It keeps the time in its own state, not in the listener. A run that finds a
request inside that minute removes it, does not fetch, and sends at most one
reply per minute. A clock that goes back by less than a minute does not
pass the limit. Many `/check` messages before root reads them are one
file (a fixed name). The path unit starts the same run unit as the timer,
and systemd never starts it twice at once; the lock keeps a run and an
apply apart. A run that sees a request while the lock is held waits for
it, instead of starting again and again. The listener writes at most one
request per instance in 10 seconds, so a burst of `/check` from the owner
does not reach the trigger limit of the path unit.

*Left.* One fetch per minute, and one reply per minute. A taken-over
listener can write requests fast enough to hit the trigger limit of the
path unit; systemd then stops the path unit, and `/check` does nothing
until the owner resets it (DESIGN.md section 8). The run unit has no start
limit, so the timer goes on. This denies; it never approves. A clock jump back of more than
a minute allows one more run.

### T18b. `/status`, `/pause`, `/resume`, `/ask-again` from a stolen token or the listener

*Attack.* Someone with the bot token, or a compromised listener, sends
`/status` or `/pause`, or writes the request files directly.

*Defense and what is left.* The request files hold only their kind; root
reads its own state and acts. So the most they can do:

- **See a status**: the mode, the SHAs, the open question, the pause and
  broken reasons. With the token they can read the questions anyway, which
  show more. The status holds no key or token. It shows the error texts
  that were sent in notices already (the broken reason), and nothing new.
  Root answers at most once a minute, and a `/status` never skips the
  normal run, so a stream of them cannot stop the fetches.
- **Pause an instance.** This only stops work: it never applies or
  approves anything. A thief can deny work in other ways already (not
  deliver a tap). Root never changes a pause file that is there (it makes
  the file with a hard link, which fails if the name exists), so a
  `/pause` cannot replace the reason of a safety pause, and a later
  limited `/resume` (code6x/pullgate#33) cannot use it to lift one.

*`/resume` and `/ask-again`* (code6x/pullgate#33) can undo a stop, so
they are limited. `/resume` lifts only a pause whose file is exactly what
`/pause` writes (root's regular file, one link, one line in that form):
the only pause a thief could set anyway. So if the owner suspects the
token or the listener, the owner pauses as root (`pullgate status NAME
--pause`): a token holder can lift a `/pause`. `/resume` moves the file
aside and checks that it is the file it checked, so a pause that root
writes at the same moment is put back and stays. A pause that pullgate set after a
cut-off apply, or that root set, stays, and so does the broken state
(T6). `/ask-again` clears only an expired mark or an open question; it
never re-asks a commit that the owner skipped or that failed, was held or
was refused, so a thief cannot re-ask a skipped commit and hope for a
wrong tap. A new question still needs the owner's tap, and counts in
`questions_per_day`.

Apart from that, they cannot approve or change the state: the state directory is
root's (0700), and any other file in the check directory is removed and
does nothing (a test).

### T19. What the bot tells

*Attack or accident.* Someone sends the bot messages to learn what it runs,
or the owner's client sends many messages.

*Defense.* Only the owner, in the owner's chat, gets an answer: `/help`, the
usage text of `/check`, or "Not a command". Others get nothing, so the bot
does not tell strangers that it exists. The `/help` text is fixed: the
commands, the instance names and the `pullgate status` commands. It holds
no secret, no path and nothing about the host beyond the instance names,
which every question shows too (with the `host` value). It never repeats the text of a message.
The `/help` reply and the "Not a command" reply are each sent at most
once per minute.

*Left.* Someone with the bot token can read the help, as they can read
every question.

### T20. The update of pullgate itself (`/update`)

The update replaces the program that is the gate. A wrong or bad binary
removes the gate on that host. So it has the same rules as a commit, and
more checks.

*Attack: a bad binary.* An attacker serves a tarball that the release
workflow did not build, or an older build under a newer name.
*Defense.* Root runs the copy of `install.sh` that is built into the
running binary, so the code that checks is the code that was checked and
installed before. **Before the question is sent** it checks the SHA-256
from `SHA256SUMS` and runs `gh attestation verify` with the release bundle
and the release workflow identity. No code of the release runs before the
tap: `--check-only` does not run the new binary. On Apply it downloads the
files again and does every check of T17 again, and also runs the new
binary's `pullgate version`. On Apply, the tarball must also have the SHA-256 that the
question showed (`--sha256`), so Apply installs only that file (rule 2). The
attestation check is required. There is no setting that skips it.
`[updates] gh` names the gh that checks, as `install.sh --gh` does; it must
be gh 2.93.0 or newer and only root may change it. A release that fails a
check gets a warning notice, never a question and never the "/update" hint.

*Attack: a stolen bot token.* The thief can send `/update` as a message,
but only the owner's user ID in the owner's chat counts (rule 4), and a
message holds nothing: the listener writes the six bytes `check\n`. A
forged request only makes root look for a release and ask. The thief
cannot tap as the owner (T1). Root takes the version and the SHA-256 from
its own state, never from the approval.

*Attack: a compromised listener.* It can write requests and approvals in
`/var/spool/pullgate/_update/`, as for an instance (T2, T12). An approval
counts only with the code of the open question, within 12 hours, once. It
cannot name a version or a file.

*Attack: a published release that is bad* (T16 failed). Forward only:
root never installs the same or an older version from Telegram (its own
check, and `install.sh --forward-only`). The owner taps Apply after reading
the release notes. A release is never installed without a tap: the daily
notice only says "send /update". The owner can install a pinned version by
hand with `install.sh --version X.Y.Z`.

*Accident: the new binary does not start.* Root keeps the old binary as
`.pullgate.prev` until the new one works: it runs `pullgate version`, gets
the units of the new binary (`pullgate units`), installs the ones that
changed, reloads systemd, restarts the units, restarts the listener last,
and checks that the listener is still active 5 seconds later. When a step
fails, it puts the old binary and the old unit files back, reloads,
restarts the listener, and reports. The new listener sends the report of
a good update, so the report also shows that it runs.

*The units.* Root writes only files named `pullgate-*.service`, `.timer`
or `.path` in `/etc/systemd/system`, at most 32 of 64 KiB, that the new
(attested) binary carries. A unit that is a link (for example masked) is
left alone. Drop-ins are never changed: they are listed in the report, so
the owner sees a workaround that a release made unneeded. A unit that the
owner disabled stays disabled; only a unit that is new in this version is
enabled.

*Name space.* The updates use `/var/lib/pullgate/_update/` and
`/var/spool/pullgate/_update/`. An instance name matches
`[a-z0-9][a-z0-9-]{0,31}`, so no instance can be named `_update`, and an
instance named `pullgate` has its own directories (`TestUpdateQuestionsAreTheirOwn`).

*Left.* Whoever controls the release workflow on `main`, or GitHub, can
ship a binary that passes (T16); the owner's tap is the last check. A
build from the source (`pullgate dev`) does not update itself.

### T21. Setup: a planted owner ID, the token entry, the pairing code

*Attack.* The person who sets up a host pastes a script that someone else
wrote. The script writes its own `owner_id` into `listener.toml`, and so
gets the bot and every Apply. Or the token leaks: into the shell history,
a process list, a log, or the environment of a child program. Or someone
else sends the pairing code first, or guesses it.

*Defense.* `pullgate bootstrap` (DESIGN.md section 17):

- The IDs come only from the Telegram message that carries the one-time
  code. No flag, config key or variable sets them; a planted
  `listener.toml` is replaced at the pairing. Bootstrap refuses unknown
  options, such as `--owner-id`. `--api-url` takes only the real Bot API
  or a test server on `127.0.0.1`: a pasted script cannot send the
  pairing through a server that changes the sender ID. The listener's
  `api_url` is parsed the same way, so `http://127.0.0.1:80@host` is
  refused.
- The code is 8 random digits, valid for 10 minutes, printed only on the
  console of the host. 5 wrong codes stop the pairing, so a guess works
  with a chance of 5 in 100 million. The right code counts only from a
  private chat with a person; from a group it stops the pairing, because
  others saw it. A wrong code, a group or a timeout writes nothing.
- The token is read with `systemd-ask-password` (no echo, no history, no
  argument) or from stdin. It is checked with `getMe`, stored with mode
  0600, and never printed. Programs that bootstrap starts get only `PATH`
  and `LC_ALL`.
- The deploy key is made on the host; only the public key is printed. The
  host key of `github.com` comes from the binary, with its fingerprint
  shown; another git host needs a file that the owner checked.

*Left.* Someone who sees the console of the host during the pairing can
send the code first. Check the chat: bootstrap sends "paired" to the chat
that won. A pasted script can still do anything else as root; bootstrap
only takes away the quiet way to plant the owner ID. If the failed run
leaves an old `listener.toml`, bootstrap says that it did not write it.

## 6. Out of scope

- An attacker who is root on the host.
- A program that a command runs and that pullgate did not check (for
  example a tool that `apply` calls by name). The owner writes `apply`.
- An attacker who controls the owner's Telegram account.
- A bad change that the owner approves on purpose.
- Signed commits (planned later).
