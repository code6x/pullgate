# Releasing pullgate

pullgate uses [release-please](https://github.com/googleapis/release-please)
and the same release setup as nodkey. A release is the merge of the release
pull request. **Only the owner merges it.**

## Pull request titles

Every pull request is squash merged, and its title becomes the commit
message on `main`. So the title must follow
[Conventional Commits](https://www.conventionalcommits.org/). The PR title
workflow checks it.

| type | release | changelog section |
|---|---|---|
| `security` | patch | Security |
| `feat` | minor (patch before 1.0) | Features |
| `fix` | patch | Bug Fixes |
| `perf` | patch | Performance Improvements |
| `revert` | patch | Reverts |
| `docs` | patch | Documentation |
| `chore`, `ci`, `test`, `refactor`, `build`, `style` | none | hidden |

## What happens

1. After every push to `main`, the **Release** workflow runs release-please.
   When a commit makes a release, it opens a release pull request, titled
   like `chore(main): release 0.1.0`. It changes `CHANGELOG.md` and
   `.release-please-manifest.json`.
2. **Before you merge it, read the version and every line of the
   changelog.** This text becomes the release notes.
3. When you merge it, release-please makes the tag (`v0.1.0`) and a
   **draft** release.
4. The `build` job (read-only) checks that the tag is the commit of the run,
   builds static binaries for Linux amd64 and arm64 (`CGO_ENABLED=0`,
   `-trimpath`), packs each with `LICENSE` and `README.md` into
   `pullgate_<version>_linux_<arch>.tar.gz`, and writes `SHA256SUMS`.
5. The `publish` job checks the files and the tag again, makes a signed
   build provenance attestation (`pullgate_<version>.sigstore.json`),
   checks the tarballs with it, uploads everything to the draft release and
   publishes it. The release is never public without its files.

If `build` or `publish` fails, the release stays a draft. Fix the problem
in a new `fix:` pull request and merge the next release pull request.

## One-time setup

Until this is done, the `release-please` job skips itself.

1. Install the organization's release GitHub App (the one nodkey uses) on
   `code6x/pullgate`, and make a new private key for this repository. The
   steps are in [nodkey's `docs/releasing.md`](https://github.com/code6x/nodkey/blob/main/docs/releasing.md), section "One-time setup: a
   GitHub App".
2. Save the App's Client ID and the key's fingerprint as repository
   variables, and the key as a secret of the `release` environment:

   ```sh
   gh variable set RELEASE_APP_CLIENT_ID --repo code6x/pullgate --body '<Client ID>'
   gh variable set RELEASE_APP_KEY_FINGERPRINT --repo code6x/pullgate --body '<fingerprint>'
   gh secret set RELEASE_APP_PRIVATE_KEY --repo code6x/pullgate --env release < key.pem
   ```

   The workflow does not use the fingerprint. It is only a record, so you
   can check later which key belongs to this repository.

3. Settings: a ruleset for `main` (pull requests only, required checks, no
   force push), two rulesets for `v*` tags (nobody moves or deletes a tag;
   only the App creates one), immutable releases on, and the `release`
   environment limited to `main`. Add the owner as a required reviewer of the `release`
   environment, so every release waits for the owner's approval in the
   Actions page, whoever merged the release pull request.

## Verify a download

```sh
VERSION=0.1.0
ARCH=amd64
U="https://github.com/code6x/pullgate/releases/download/v$VERSION"
curl -fLO "$U/pullgate_${VERSION}_linux_${ARCH}.tar.gz"
curl -fLO "$U/SHA256SUMS"
curl -fLO "$U/pullgate_${VERSION}.sigstore.json"
sha256sum --ignore-missing -c SHA256SUMS
gh attestation verify "pullgate_${VERSION}_linux_${ARCH}.tar.gz" \
  --bundle "pullgate_${VERSION}.sigstore.json" \
  --repo code6x/pullgate \
  --cert-identity "https://github.com/code6x/pullgate/.github/workflows/release.yml@refs/heads/main" \
  --deny-self-hosted-runners
```

Both checks must say OK before you install the binary.
