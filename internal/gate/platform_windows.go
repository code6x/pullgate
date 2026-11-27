package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/gitx"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
	"github.com/code6x/pullgate/internal/winsecure"
)

const reportReadFlags = int(windows.O_FILE_FLAG_OPEN_REPARSE_POINT)

// NewWindows builds a gate inside a root whose ancestors have been
// checked by the Windows entry point.
func NewWindows(cfg *config.Instance, root, git string, log *slog.Logger, out io.Writer) (*Env, error) {
	if err := winsecure.Check(root, true); err != nil {
		return nil, err
	}
	if cfg.Mode != config.ModeAsk && cfg.Mode != config.ModeOff {
		return nil, errors.New("mode requires approval on Windows. Fix: change the instance config")
	}
	if len(cfg.Commands.Check) > 0 || len(cfg.Commands.Summary) > 0 {
		return nil, errors.New("check and summary require a sandbox that Windows does not provide. Fix: remove these commands")
	}
	for _, argv := range [][]string{cfg.Commands.Apply, cfg.Commands.Health} {
		if len(argv) > 0 {
			if err := winsecure.CheckManaged(root, argv[0], false); err != nil {
				return nil, err
			}
		}
	}
	dir := state.Dir{Path: filepath.Join(root, "state", cfg.Name)}
	return &Env{
		Cfg: cfg, WindowsRoot: root, Dir: dir, Log: log, CmdOut: out,
		Spool: spool.Dirs{Base: filepath.Join(root, "spool"), Name: cfg.Name},
		Git:   &gitx.Git{Dir: dir.Repo(), Home: dir.Path, WindowsRoot: root, WindowsGit: git, Key: cfg.Repo.DeployKey, KnownHosts: cfg.Repo.KnownHosts},
	}, nil
}

func (e *Env) takeAuto() (*spool.Approval, error) { return nil, nil }

func (e *Env) runSandboxed(_ context.Context, _ []string, _ cmdEnv) (string, error) {
	return "", errors.New("cannot sandbox a pre-approval command on Windows. Fix: remove check and summary")
}

func (e *Env) runApplyCommand(ctx context.Context, argv []string, c cmdEnv, out io.Writer) error {
	if err := winsecure.CheckManaged(e.WindowsRoot, argv[0], false); err != nil {
		return err
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return fmt.Errorf("find Windows PowerShell: %w. Fix: check the Windows installation", err)
	}
	prog := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	args := append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}, argv...)
	env := append(e.environ(c), "PATH="+system, "SystemRoot="+filepath.Dir(system))
	return runInJob(ctx, prog, args, env, c.Tree, out)
}

func (e *Env) showRecipe(_ func(string, string)) {}
