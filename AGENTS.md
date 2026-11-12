# Working on pullgate

pullgate lets a Linux host pull a git repo by itself and apply one exact
commit only after its owner taps Apply on Telegram. Read
[DESIGN.md](DESIGN.md) and [THREAT-MODEL.md](THREAT-MODEL.md) before you
change code. They are the rules the code must follow.

## Layout

- `cmd/pullgate/` - the one binary. Subcommands: `run`, `listen`, `apply`,
  `status`, `version`.
- `internal/` - one package per part. Packages do not import `cmd/`.
  - `internal/config/` - the instance and listener configs, and the checks
    of the files they name (DESIGN.md sections 4 and 5).
  - `internal/safedir/` - the check that no other user can write a
    directory (rule 6, DESIGN.md section 6).
  - `internal/gitx/` - git, run safely: fetch, ancestry, log, diff, and the
    export of a tree without filters (DESIGN.md section 6).
  - `internal/state/` - the state file, the lock and the pause file.
  - `internal/spool/` - the outbox and inbox files between root and the
    listener (DESIGN.md section 8).
  - `internal/telegram/` - a small Bot API client. The token never shows in
    an error.
  - `internal/listener/` - `pullgate listen` (DESIGN.md section 9).
  - `internal/message/` - the text of the messages: cleaning, the watch
    list, the cut to one message (DESIGN.md section 12).
  - `internal/gate/` - `run`, `apply` and `status` (DESIGN.md sections 7,
    9, 10, 13).
- `contrib/` - example systemd units and example configs.
- `install.sh` - installs or updates a release on a host, after its checks
  (DESIGN.md section 16). Its tests are `install_linux_test.go`, at the
  root. CI runs a pinned ShellCheck on it.
- `test/e2e/` - the end-to-end test. Build tag `e2e`.
- `docs/releasing.md` - how releases are made and checked. Only the owner
  merges a release pull request (`chore(main): release …`).
- `.claude/agents/security-reviewer.md` - the security review every change
  gets.

## Commands

```sh
go test -race ./...
go vet ./...
golangci-lint run --build-tags e2e
go test -race -tags e2e ./test/e2e/...   # needs git
```

## Rules

- **This repository is public.** Never add a real host name, domain name,
  IP address, VPN or tailnet name, token, key, user ID, private name of a
  person, household or company, or a link to a private repository. Use
  example names: `example-host`, `config-repo`, `git.example.com`, and IP
  addresses from `192.0.2.0/24`. Test keys and tokens are made by the test.
- **Never log, print or return a secret value** (the bot token, the deploy
  key, a one-time code), not even in an error or a test failure.
- **Fail closed.** Every error, timeout, cancel or unclear case means: do
  not apply, do not approve.
- **Root trusts nothing from the listener, the repo, or the commit** except
  a code that matches its own state. It takes the SHA from its own state.
- **Every change has tests**, and they test the deny paths, not only the
  happy path.
- **Every change gets a security review** by the agent in
  `.claude/agents/security-reviewer.md` before it is committed. Fix what it
  finds. If you do not fix a finding, say why in the pull request.
- **If a change alters** what a message shows, what root or the listener
  checks, what runs as which user, or where a file goes, update DESIGN.md
  and THREAT-MODEL.md in the same pull request. If it changes the config, a
  unit or a command, update the README and `contrib/` too.
- **Dependencies:** the standard library and `github.com/BurntSushi/toml`.
  Anything else needs a reason in the pull request.
- **Git:** one feature branch and one pull request per change. Pull request
  titles follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`, `test:`, `ci:`, `chore:`). Pull requests are
  squash merged. Never push to `main` directly.
- **Open work is a GitHub issue** with a "Done when" checklist, never a TODO
  file.
- **Every error says how to fix it**: what is wrong, where (the file and key,
  or the step), and how to fix it.
- **Docs** use plain English and short sentences. Many readers are not
  native speakers. Dates are absolute (`2026-11-11`).
