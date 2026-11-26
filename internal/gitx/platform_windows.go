package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/code6x/pullgate/internal/winsecure"
)

const gitNull = "/dev/null"
const syscallNoFollow = int(windows.O_FILE_FLAG_OPEN_REPARSE_POINT)

func (g *Git) checkCommand() error {
	if !SafePath(g.WindowsGit) || !strings.EqualFold(filepath.Base(g.WindowsGit), "git.exe") {
		return errors.New("invalid Git for Windows executable. Fix: configure an absolute path to git.exe in a protected Git installation")
	}
	if err := g.checkDir(g.Home); err != nil {
		return err
	}
	for _, path := range []string{g.Key, g.KnownHosts} {
		if path != "" {
			if err := winsecure.CheckManaged(g.WindowsRoot, path, false); err != nil {
				return err
			}
		}
	}
	if g.Key != "" {
		return winsecure.CheckSystemPath(g.sshExecutable(), false)
	}
	return nil
}

func (g *Git) checkDir(path string) error {
	return winsecure.CheckManaged(g.WindowsRoot, path, true)
}

func (g *Git) checkTree(path string) error {
	if err := g.checkDir(path); err != nil {
		return err
	}
	return filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == path {
			return err
		}
		return winsecure.Check(p, d.IsDir())
	})
}

func (g *Git) makeRepoDir() error { return winsecure.Mkdir(g.Dir) }

func (g *Git) checkNewRepo() error { return g.checkTree(g.Dir) }

func (g *Git) sshAllowed() bool {
	return SafePath(g.Key) && SafePath(g.KnownHosts) &&
		!strings.ContainsAny(g.Key, "%$") && !strings.ContainsAny(g.KnownHosts, "%$")
}

func (g *Git) sshExecutable() string {
	return filepath.Join(filepath.Dir(filepath.Dir(g.WindowsGit)), "usr", "bin", "ssh.exe")
}

func (g *Git) sshCommand() string {
	return shellQuote(filepath.ToSlash(g.sshExecutable())) + " -F /dev/null -i " + shellQuote(filepath.ToSlash(g.Key)) +
		" -o IdentitiesOnly=yes -o IdentityAgent=none" +
		" -o " + shellQuote("UserKnownHostsFile="+strconv.Quote(filepath.ToSlash(g.KnownHosts))) +
		" -o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes -o BatchMode=yes"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (g *Git) fetchURL(url, local string) string {
	if local != "" {
		return filepath.ToSlash(local)
	}
	return url
}

func (g *Git) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.WindowsGit, append(g.baseArgs(), args...)...) //nolint:gosec // path comes from protected host config
	cmd.Env = []string{
		"PATH=" + filepath.Dir(g.WindowsGit),
		"HOME=" + g.Home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + gitNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"LC_ALL=C",
	}
	if g.Key != "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_VARIANT=ssh", "GIT_SSH_COMMAND="+g.sshCommand())
	}
	cmd.Dir = g.Home
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func SafePath(p string) bool {
	volume := filepath.VolumeName(p)
	return len(volume) == 2 && volume[1] == ':' && filepath.IsAbs(p) && filepath.Clean(p) == p
}

func LocalPath(repoURL string) (string, error) {
	p := filepath.FromSlash(repoURL)
	if SafePath(p) {
		return p, nil
	}
	if strings.HasPrefix(repoURL, "ssh://") {
		u, err := url.Parse(repoURL)
		if err == nil && u.Hostname() != "" && u.Path != "" && u.RawQuery == "" && u.Fragment == "" {
			if _, password := u.User.Password(); !password {
				return "", nil
			}
		}
	}
	return "", errors.New("invalid repo URL. Fix: use an ssh:// URL with no password, query or fragment, or an absolute Windows drive path under the protected root")
}

func checkExportPath(path, mode string) error {
	if err := CheckPath(path); err != nil {
		return err
	}
	if mode == "120000" {
		return fmt.Errorf("the commit contains a symlink at %q. Fix: replace the symlink with a regular file", path)
	}
	for _, part := range strings.Split(path, "/") {
		if !winsecure.ValidPathPart(part) || strings.ContainsRune(part, '~') {
			return fmt.Errorf("the commit contains a path Windows cannot safely export: %q. Fix: rename the path", path)
		}
	}
	return nil
}

func seal(_ string, _ int) error {
	// Files already have the read-only attribute. Windows directory access
	// is controlled by the checked ACL of the tree's private parent.
	return nil
}
