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
| the listener process and its user | Trusted only to pass on the owner's taps. If it is taken over, it can approve an open question without the owner (T2). It can never choose the SHA. |
| `check_user` and the `check`/`summary` commands | Not trusted with root. They read hostile data (T3). |
| the git server and everyone who can push | **Not trusted.** A commit may be hostile. |
| the network | Not trusted. SSH with a pinned host key protects the fetch. |
| other local users | Not trusted. |
| the `apply` and `health` commands | Trusted: the owner wrote them. They run as root on the approved tree. |

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
user can write the outbox too, but that only changes what the listener
itself sends.

*Left.* It can approve any open question (or a superseded one that has not
expired) without the owner, and it can hide or fake messages. So the
attacker still needs a hostile commit in the repo **and** the listener.
Run the listener as its own user, with the hardened unit.

### T3. A hostile commit

*Attack.* Someone who can push writes a commit made to hurt the host before
or during the review.

- **Its content.** Before the tap, no file of the commit runs. git only
  reads objects. `check` and `summary` read files as data, as `check_user`
  (not root), in a read-only tree, inside the run unit's sandbox. A parser
  bug in them hits only `check_user`. They have a timeout and an output cap.
- **File names.** Each path part is checked: no `..`, no `.git`, no empty
  part, no NUL. Names with control or invisible characters, or with HTML,
  are cleaned and escaped before they are shown, so a name cannot fake a
  line of the question.
- **Commit subjects and summary output.** The same cleaning; HTML-escaped,
  each line capped. The head of the question (host, instance, SHA, the
  warnings) always comes first, before any text from the commit.
- **Symlinks.** They are written as symlinks and never followed by
  pullgate. The tree is written with `O_NOFOLLOW|O_EXCL` into directories
  that pullgate made itself, so a symlink cannot make pullgate write
  outside the tree. A command that reads the tree must not follow symlinks
  blindly (the docs say so).
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
example a bad binary), the next run fails to start and the owner sees the
failed unit; pullgate does not try to repair itself (self-update is not in
0.1).

*Left.* An approved commit can change everything. This is the price of
having the host manage itself.

### T6. A rollback that fails

*Attack or accident.* `apply` fails, and running it again on the old tree
fails too. The host may be half changed.

*Defense.* With `on_failure = "rollback"`, a failed rollback writes the pause
file and reports it. Nothing more runs until the owner looks and runs
`pullgate status NAME --resume`. With `on_failure = "pause"` there is no
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
tap on a closed or expired question, writes nothing. Root removes a question
from its state before it acts, so a replayed approval finds no question.
Root checks the time limit itself. A tap on a superseded question that the
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

### T11. A wrong clock

*Accident or attack.* The host clock jumps forward or back.

*Defense.* Time limits are checked by root with its own clock against times
it recorded itself: an approval counts only when `asked <= now < expires`.
A clock that goes back before `asked` denies. A clock that jumps forward
expires questions early: this denies too. The listener checks expiry with its
own clock as well. Timeouts of commands use the monotonic clock.

*Left.* A clock that is set far back while a question is open can keep that
question valid longer than `approval_ttl`. The code still works only once.

### T12. A replayed or forged inbox file

*Attack.* A local user, or the listener, puts files in the inbox.

*Defense.* Only the listener user can write the inbox. Root needs the
listener as the file owner, a regular file, at most 1 KiB, valid JSON
without unknown fields, and a code that matches its state. A bad file is
removed and logged. The path unit then has nothing left and stops.

### T13. A hostile git server or network

*Attack.* A fake server, or a server that sends huge or broken data.

*Defense.* SSH with `StrictHostKeyChecking=yes` and a pinned known hosts file
from the host config; a read-only deploy key; no other protocols. git has
a timeout and an output cap. The tree export has caps.

### T14. Too many questions

*Attack.* Someone pushes many commits to tire the owner into tapping Apply.

*Defense.* At most one open question per instance; a newer tip replaces it.
`questions_per_day` caps the questions. A SHA is asked once.

## 6. Out of scope

- An attacker who is root on the host.
- An attacker who controls the owner's Telegram account.
- A bad change that the owner approves on purpose.
- Signed commits (planned later).
