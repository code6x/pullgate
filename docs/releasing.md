# Releasing pullgate

pullgate uses [release-please](https://github.com/googleapis/release-please).
A release is the merge of the release pull request. **Only the owner merges
it.**

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

Until this setup is done, the `release-please` job skips itself. In the
**Release** workflow runs it shows as "skipped". Nothing else breaks.

### Why a GitHub App

release-please could use the built-in `GITHUB_TOKEN`. But GitHub does not
start workflows for a pull request or a push that `GITHUB_TOKEN` made. So CI
and the PR title check would not run on the release pull request, and it
could not pass the required checks. A token from a GitHub App does start
workflows.

An App is also better than a personal access token: it does not belong to a
person, it has only the permissions it needs, and each token from it expires
after one hour.

### Steps

One App can serve several repositories of the organization. Give each
repository its own private key. Then you can delete one key without
breaking the others. The keys do not keep the repositories apart: any key
of the App can get a token for every repository the App is installed on.
If a repository must be kept apart, give it its own App.

If the organization already has a release App, skip step 1, and check in
step 2 that its settings match step 1.

1. Create the App. In the organization, open
   **Settings → Developer settings → GitHub Apps → New GitHub App**.
   - **GitHub App name**: any free name, for example `<organization>-release`.
   - **Homepage URL**: the URL of this repository.
   - **Webhook**: clear the **Active** box. The App needs no webhook.
   - **Repository permissions**:
     - **Contents**: Read and write. It creates the release branch, the tag
       and the release.
     - **Pull requests**: Read and write. It opens the release pull request
       and adds labels and comments to it.
     - Leave all other permissions at "No access". GitHub adds
       **Metadata: Read-only** by itself.
   - **Where can this GitHub App be installed?**: **Only on this account**.
   - Click **Create GitHub App**.
2. Open the App page: **Settings → Developer settings → GitHub Apps →
   Edit** next to the App. Copy the **Client ID**.
3. Open **Credentials → Key pairs** and click **New key**. Your browser
   downloads a `.pem` file. Use this key only for this repository.
4. Find the key's fingerprint:

   ```sh
   echo "SHA256:$(openssl rsa -in path/to/key.pem -pubout -outform DER | openssl sha256 -binary | openssl base64 -A)"
   ```

   Check that the **Key pairs** page shows this exact value next to the new
   key. If it does not, stop: the path is wrong, or the file is not the new
   key.
5. Install the App: **Install App** → your organization →
   **Only select repositories** → add `<organization>/pullgate` →
   **Install** (or **Save**, if the App is already installed on other
   repositories).
6. Make the `release` environment first: **Settings → Environments → New
   environment**, name `release`, and set **Deployment branches and tags**
   to **Selected branches** with only `main`. Then only runs on `main` can
   read its secrets. Save the private key as a secret of this environment,
   then the Client ID and the fingerprint as repository variables:

   ```sh
   gh secret set RELEASE_APP_PRIVATE_KEY --repo <organization>/pullgate --env release < path/to/key.pem
   gh variable set RELEASE_APP_CLIENT_ID --repo <organization>/pullgate --body '<Client ID>'
   gh variable set RELEASE_APP_KEY_FINGERPRINT --repo <organization>/pullgate --body '<the line from step 4>'
   ```

   Do not save the key as a repository secret: then workflow runs on other
   branches could read it too. The workflow does not use the fingerprint.
   It tells you which key on the **Key pairs** page belongs to this
   repository.
7. Delete the `.pem` file from your computer.
8. Repository settings:
   - a ruleset for `main`: pull requests only, the required checks, no
     force push;
   - two rulesets for `v*` tags. **release tags**: **Restrict updates**
     and **Restrict deletions**, with an empty bypass list. **release tag
     creation**: **Restrict creations**, with only the App in the bypass
     list. Two rulesets are needed because a bypass entry skips every rule
     of its ruleset; with one, the App could also move or delete tags;
   - immutable releases on.

The `release` environment has **no required reviewer**. The
`release-please` job uses it on every push to `main`, so a reviewer would
hold every merge, not only releases. The gate is the merge of the release
pull request, which only the owner does. This holds only while the owner is
the only person who can merge to `main`. If you give someone else write
access, limit who can merge in the ruleset for `main`.

The next push to `main` runs release-please with the App.

If a key leaks, delete it on the **Key pairs** page at once, make a new key
(steps 3 to 7), and look for tags, releases and merged pull requests that
you did not expect. A token made with the old key can still work for up to
one hour.

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

`install.sh` makes the same checks, and checks the version of the new
binary too (README.md, section "Install").
