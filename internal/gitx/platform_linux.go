package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/code6x/pullgate/internal/safedir"
)

const gitNull = "/dev/null"

func (g *Git) checkCommand() error         { return nil }
func (g *Git) checkDir(path string) error  { return safedir.CheckParents(path, g.UID) }
func (g *Git) checkTree(path string) error { return safedir.CheckTree(path, g.UID) }
func (g *Git) makeRepoDir() error          { return os.Mkdir(g.Dir, 0o700) }
func (g *Git) sshAllowed() bool            { return g.Key != "" && SafePath(g.Key) && SafePath(g.KnownHosts) }

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

func (g *Git) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(g.baseArgs(), args...)...) //nolint:gosec // fixed program, args built here
	cmd.Env = g.env()
	cmd.Dir = g.Home
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

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

func (g *Git) checkNewRepo() error {
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

func checkExportPath(path, _ string) error { return CheckPath(path) }

// seal makes every directory of the tree read-only (0555), and the top
// directory 0750 with group gid.
func seal(dest string, gid int) error {
	var dirs []string
	err := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != dest {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("seal the tree: %w", err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if err := os.Chmod(d, 0o555); err != nil {
			return fmt.Errorf("seal the tree: %w", err)
		}
	}
	if gid >= 0 && gid != os.Getegid() {
		if err := os.Lchown(dest, -1, gid); err != nil {
			return fmt.Errorf("seal the tree: %w", err)
		}
	}
	if err := os.Chmod(dest, 0o750); err != nil {
		return fmt.Errorf("seal the tree: %w", err)
	}
	return nil
}

func (g *Git) fetchURL(url, _ string) string { return url }
