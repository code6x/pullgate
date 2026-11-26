package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

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
	cmd := exec.CommandContext(ctx, prog, args...) //nolint:gosec // built-in PowerShell and checked script from host config
	cmd.Env = append(e.environ(c), "PATH="+system)
	cmd.Dir = c.Tree
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second

	// Suspend the first thread until the job contains the process. This
	// prevents it from starting children before the job can track them.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	return runInJob(ctx, cmd)
}

func runInJob(ctx context.Context, cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create a process job: %w. Fix: run on a supported Windows version", err)
	}
	defer func() { _ = windows.CloseHandle(job) }()
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))) //nolint:gosec // documented Job Object structure
	if err != nil {
		return fmt.Errorf("set process job limits: %w. Fix: check the task's process permissions", err)
	}
	cmd.Cancel = func() error { return windows.TerminateJobObject(job, 1) }
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // the Windows process ID is a DWORD
	if err := attachAndResume(ctx, job, pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("start PowerShell inside the process job: %w. Fix: check that the task allows nested Job Objects", err)
	}
	return cmd.Wait()
}

func attachAndResume(ctx context.Context, job windows.Handle, pid uint32) error {
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(p) }()
	if err := windows.AssignProcessToJobObject(job, p); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err := windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		defer func() { _ = windows.CloseHandle(thread) }()
		count, err := windows.ResumeThread(thread)
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("unexpected suspension count")
		}
		return nil
	}
	return errors.New("cannot find the suspended PowerShell thread")
}

func (e *Env) showRecipe(_ func(string, string)) {}
