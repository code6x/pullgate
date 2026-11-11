// Package gitx runs git as root, safely (rule 6, DESIGN.md section 6).
//
// Every call checks the git directory first, runs git with an empty
// environment and no system or global config, with hooks and fsmonitor
// off, only the ssh and file protocols, a timeout and an output cap.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/code6x/pullgate/internal/safedir"
)

// MaxOutput is the most output that pullgate keeps from one git command
// (the tree export streams and has its own caps).
const MaxOutput = 16 << 20

var (
	shaRE    = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// ValidSHA reports whether s is a full SHA-1 or SHA-256 object name.
func ValidSHA(s string) bool { return shaRE.MatchString(s) }

// ValidBranch reports whether b is a safe branch name: it cannot be read as
// an option, and has no "..", "//", "@{" or ".lock" end.
func ValidBranch(b string) bool {
	return branchRE.MatchString(b) && !strings.Contains(b, "..") &&
		!strings.Contains(b, "//") && !strings.HasSuffix(b, "/") &&
		!strings.HasSuffix(b, ".lock") && !strings.HasSuffix(b, ".")
}

// Git runs git on one bare repo.
type Git struct {
	Dir        string        // the bare git directory, owned by UID
	Home       string        // HOME for git and ssh
	UID        uint32        // the user that must own Dir (root in production)
	Key        string        // the deploy key, for ssh URLs
	KnownHosts string        // the pinned host keys, for ssh URLs
	Timeout    time.Duration // per command; 0 means 10 minutes
}

func (g *Git) timeout() time.Duration {
	if g.Timeout <= 0 {
		return 10 * time.Minute
	}
	return g.Timeout
}

func (g *Git) env() []string {
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + g.Home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"LC_ALL=C",
	}
	if g.Key != "" {
		env = append(env, "GIT_SSH_COMMAND="+sshCommand(g.Key, g.KnownHosts))
	}
	return env
}

// sshCommand is the ssh command line for git. The paths come from the host
// config; they are quoted for the shell that git uses for GIT_SSH_COMMAND.
func sshCommand(key, knownHosts string) string {
	return "ssh -F /dev/null -i " + shellQuote(key) +
		" -o IdentitiesOnly=yes -o IdentityAgent=none" +
		" -o UserKnownHostsFile=" + shellQuote(knownHosts) +
		" -o GlobalKnownHostsFile=/dev/null" +
		" -o StrictHostKeyChecking=yes -o BatchMode=yes"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// baseArgs come before every git subcommand.
func (g *Git) baseArgs() []string {
	return []string{
		"--git-dir=" + g.Dir,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.ssh.allow=always",
		"-c", "protocol.file.allow=always",
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "core.attributesFile=/dev/null",
		"-c", "transfer.fsckObjects=true",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
	}
}

func (g *Git) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(g.baseArgs(), args...)...) //nolint:gosec // fixed program, args built here
	cmd.Env = g.env()
	cmd.Dir = g.Home
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// capWriter keeps at most n bytes and drops the rest.
type capWriter struct {
	buf bytes.Buffer
	n   int
	cut bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.n - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.cut = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.cut = true
	}
	return len(p), nil
}

// Run checks the git directory and runs one git command.
func (g *Git) Run(ctx context.Context, args ...string) ([]byte, error) {
	if err := safedir.CheckTree(g.Dir, g.UID); err != nil {
		return nil, fmt.Errorf("git refused: %w", err)
	}
	return g.runUnchecked(ctx, args...)
}

// runUnchecked runs git without the directory check. Only Init uses it,
// on a new, empty directory under checked parents.
func (g *Git) runUnchecked(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	cmd := g.command(ctx, args...)
	out := &capWriter{n: MaxOutput}
	errOut := &capWriter{n: 4096}
	cmd.Stdout = out
	cmd.Stderr = errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errOut.buf.String()))
	}
	if out.cut {
		return nil, fmt.Errorf("git %s: the output is larger than %d bytes", args[0], MaxOutput)
	}
	return out.buf.Bytes(), nil
}

// Init makes the bare repo when it does not exist yet. The directory above
// it must be safe (rule 6).
func (g *Git) Init(ctx context.Context) error {
	if _, err := os.Lstat(g.Dir); err == nil {
		return safedir.CheckTree(g.Dir, g.UID)
	}
	if err := safedir.CheckParents(filepath.Dir(g.Dir), g.UID); err != nil {
		return fmt.Errorf("git init refused: %w", err)
	}
	if err := os.Mkdir(g.Dir, 0o700); err != nil {
		return fmt.Errorf("make %s: %w", g.Dir, err)
	}
	if _, err := g.runUnchecked(ctx, "init", "--quiet", "--bare", "--template=", g.Dir); err != nil {
		return err
	}
	// git follows the umask; nobody else may write here.
	if err := filepath.WalkDir(g.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return os.Chmod(p, fi.Mode().Perm()&^0o077) //nolint:gosec // a new directory that git just made, under checked parents
	}); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	return safedir.CheckTree(g.Dir, g.UID)
}

// SetApplied moves refs/pullgate/applied to sha, so git keeps that commit.
func (g *Git) SetApplied(ctx context.Context, sha string) error {
	if !ValidSHA(sha) {
		return errors.New("set applied: not a full SHA")
	}
	_, err := g.Run(ctx, "update-ref", "refs/pullgate/applied", sha)
	return err
}

// TipRef is the ref that holds the fetched branch tip.
const TipRef = "refs/pullgate/tip"

var pathRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)

// SafePath reports whether p is an absolute path with only A-Za-z0-9._/-,
// so it can be given to the shell that runs GIT_SSH_COMMAND.
func SafePath(p string) bool { return pathRE.MatchString(p) && !strings.Contains(p, "..") }

// LocalPath returns the directory of a local repo URL ("/path" or
// "file:///path"), or "" for an SSH URL. Other URLs are an error.
func LocalPath(url string) (string, error) {
	switch {
	case url == "" || strings.HasPrefix(url, "-"):
		return "", errors.New("the repo url is empty or starts with '-'. Fix: set [repo] url in the config")
	case strings.HasPrefix(url, "file://"):
		p := strings.TrimPrefix(url, "file://")
		if !strings.HasPrefix(p, "/") {
			return "", fmt.Errorf("the repo url %q is not an absolute file:// url. Fix: use file:///path", url)
		}
		return filepath.Clean(p), nil
	case strings.HasPrefix(url, "/"):
		return filepath.Clean(url), nil
	case strings.HasPrefix(url, "ssh://"):
		return "", nil
	case strings.Contains(url, "://"):
		return "", fmt.Errorf("the repo url %q uses a protocol that pullgate does not allow. Fix: use an ssh:// url or user@host:path", url)
	case strings.Contains(url, ":") && !strings.Contains(strings.SplitN(url, ":", 2)[0], "/"):
		return "", nil // user@host:path
	default:
		return "", fmt.Errorf("the repo url %q is not an ssh url or an absolute path. Fix: use an ssh:// url, user@host:path, or /path", url)
	}
}

// Fetch fetches branch from url and returns the full SHA of its tip. A
// local repo must pass the same check as the clone (rule 6), because git
// runs git-upload-pack in it.
func (g *Git) Fetch(ctx context.Context, url, branch string) (string, error) {
	local, err := LocalPath(url)
	if err != nil {
		return "", err
	}
	if local != "" {
		if err := safedir.CheckTree(local, g.UID); err != nil {
			return "", fmt.Errorf("the local repo %s is not safe for root: %w", local, err)
		}
	} else if g.Key == "" || !SafePath(g.Key) || !SafePath(g.KnownHosts) {
		return "", errors.New("an ssh url needs deploy_key and known_hosts, as absolute paths of A-Za-z0-9._/- only. Fix: set them in [repo] of the config")
	}
	if !ValidBranch(branch) {
		return "", fmt.Errorf("the branch name %q is not allowed. Fix: set [repo] branch to a plain branch name", branch)
	}
	if _, err := g.Run(ctx, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules",
		"--no-write-fetch-head", "--no-auto-gc", "--", url, "+refs/heads/"+branch+":"+TipRef); err != nil {
		return "", err
	}
	return g.RevParse(ctx, TipRef)
}

// RevParse returns the full SHA of the commit that rev names.
func (g *Git) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if !ValidSHA(sha) {
		return "", fmt.Errorf("git rev-parse gave %q, not a full SHA", sha)
	}
	return sha, nil
}

// IsAncestor reports whether a is an ancestor of b (or equal to it).
func (g *Git) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	if !ValidSHA(a) || !ValidSHA(b) {
		return false, errors.New("is-ancestor: not a full SHA")
	}
	_, err := g.Run(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// Subjects returns the subjects of the commits in old..sha (all commits of
// sha when old is empty), newest first, at most max, and the total count.
func (g *Git) Subjects(ctx context.Context, old, sha string, max int) ([]string, int, error) {
	rng := sha
	if old != "" {
		rng = old + ".." + sha
	}
	out, err := g.Run(ctx, "rev-list", "--count", rng)
	if err != nil {
		return nil, 0, err
	}
	total, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, 0, fmt.Errorf("git rev-list --count: %w", err)
	}
	out, err = g.Run(ctx, "log", "-z", "--no-color", "--no-mailmap", "--format=%s", "--max-count="+strconv.Itoa(max), rng, "--")
	if err != nil {
		return nil, 0, err
	}
	var subs []string
	for _, s := range strings.Split(string(out), "\x00") {
		if s != "" {
			subs = append(subs, s)
		}
	}
	return subs, total, nil
}

// ChangedFiles returns the paths that differ between old and sha. With an
// empty old, it returns every path of sha.
func (g *Git) ChangedFiles(ctx context.Context, old, sha string) ([]string, error) {
	var out []byte
	var err error
	if old == "" {
		out, err = g.Run(ctx, "ls-tree", "-r", "-z", "--full-tree", "--name-only", sha)
	} else {
		out, err = g.Run(ctx, "diff-tree", "-r", "-z", "--no-renames", "--no-commit-id", "--name-only", old, sha, "--")
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// discard reads and drops n bytes.
func discard(r io.Reader, n int64) error {
	_, err := io.CopyN(io.Discard, r, n)
	return err
}
