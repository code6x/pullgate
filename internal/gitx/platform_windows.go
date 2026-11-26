package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
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
	return g.checkDir(g.Home)
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

func (g *Git) sshAllowed() bool { return false }

func (g *Git) fetchURL(_, local string) string { return filepath.ToSlash(local) }

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
	cmd.Dir = g.Home
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func SafePath(p string) bool {
	volume := filepath.VolumeName(p)
	return len(volume) == 2 && volume[1] == ':' && filepath.IsAbs(p) && filepath.Clean(p) == p
}

func LocalPath(url string) (string, error) {
	p := filepath.FromSlash(url)
	if SafePath(p) {
		return p, nil
	}
	return "", fmt.Errorf("invalid local repo path %q. Fix: choose a clean absolute Windows drive path under the protected root", url)
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
