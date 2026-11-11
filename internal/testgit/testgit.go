// Package testgit makes local git repos for tests: a bare remote and a
// work tree that pushes to it. Only tests use it.
package testgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// NeedGit skips the test when git is missing, except on CI, where it fails.
func NeedGit(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("git is missing; CI must have git")
		}
		t.Skip("git is missing")
	}
}

// PrivateDir returns a test directory that only this user can write, and
// whose parent only this user can write, whatever the umask is.
func PrivateDir(t testing.TB) string {
	t.Helper()
	d := t.TempDir()
	for _, p := range []string{filepath.Dir(d), d} {
		if err := os.Chmod(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// Remote is a bare repo and a work tree that pushes to it.
type Remote struct {
	t    testing.TB
	Bare string
	Work string
}

// New makes an empty bare repo with branch main, and a work tree.
func New(t testing.TB) *Remote {
	t.Helper()
	NeedGit(t)
	d := t.TempDir()
	r := &Remote{t: t, Bare: filepath.Join(d, "remote.git"), Work: filepath.Join(d, "work")}
	r.git(d, "init", "--quiet", "--bare", "--initial-branch=main", r.Bare)
	r.git(d, "init", "--quiet", "--initial-branch=main", r.Work)
	return r
}

func (r *Remote) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Git runs git in the work tree and returns its output.
func (r *Remote) Git(args ...string) string { r.t.Helper(); return r.git(r.Work, args...) }

// Commit writes files (path -> content; an empty content removes the
// file), commits them with the subject, pushes to main, and returns the SHA.
func (r *Remote) Commit(subject string, files map[string]string) string {
	r.t.Helper()
	for p, c := range files {
		full := filepath.Join(r.Work, p)
		if c == "" {
			if err := os.Remove(full); err != nil {
				r.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.Git("add", "-A")
	r.Git("commit", "--quiet", "--allow-empty", "-m", subject)
	r.Git("push", "--quiet", "--force", r.Bare, "HEAD:refs/heads/main")
	return r.Git("rev-parse", "HEAD")
}

// Push force-pushes rev to main of the bare repo.
func (r *Remote) Push(rev string) {
	r.t.Helper()
	r.Git("push", "--quiet", "--force", r.Bare, rev+":refs/heads/main")
}
