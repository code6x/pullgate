package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/gitx"
	"github.com/code6x/pullgate/internal/recipe"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

const reportReadFlags = syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

func (e *Env) runApplyCommand(ctx context.Context, argv []string, c cmdEnv, w io.Writer) error {
	cmd := e.command(ctx, argv, c, false)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// ResolveSandbox finds check_user. As root (euid 0), the user must not be
// root, and the commands switch to it. Not as root, pullgate cannot switch
// user, so check_user must be the user that runs pullgate.
func ResolveSandbox(euid int, name string) (Sandbox, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return Sandbox{}, fmt.Errorf("check_user %q: %w. Fix: make the user (useradd --system %s), or set [commands] check_user", name, err, name)
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return Sandbox{}, fmt.Errorf("check_user %q has no numeric uid or gid", name)
	}
	if uid == 0 || gid == 0 {
		return Sandbox{}, fmt.Errorf("check_user %q is root (uid or gid 0); check and summary must never run as root (rule 3). Fix: set [commands] check_user to an unprivileged user", name)
	}
	if euid == 0 {
		return Sandbox{UID: uid, GID: gid, Switch: true}, nil
	}
	if uid != euid {
		return Sandbox{}, fmt.Errorf("pullgate does not run as root, so it cannot run check as %q. Fix: run it as root, or set check_user to the user that runs it", name)
	}
	return Sandbox{UID: uid, GID: gid}, nil
}

// ResolveOwners finds the listener user, for the spool.
func ResolveOwners(name string) (spool.Owners, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return spool.Owners{}, fmt.Errorf("listener user %q: %w. Fix: make the user (useradd --system %s), or set [listener] user", name, err, name)
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return spool.Owners{}, fmt.Errorf("listener user %q has no numeric uid or gid", name)
	}
	if uid == 0 && os.Geteuid() == 0 {
		return spool.Owners{}, fmt.Errorf("the listener user %q is root; the listener must run as its own user (rule 4). Fix: set [listener] user", name)
	}
	return spool.Owners{ListenerUID: uid, ListenerGID: gid}, nil
}

// New makes an Env. Paths: the state of NAME is stateBase/NAME, the spool
// is spoolBase/NAME.
func New(cfg *config.Instance, stateBase, spoolBase string, uid uint32, log *slog.Logger, cmdOut io.Writer) (*Env, error) {
	sb, err := ResolveSandbox(int(uid), cfg.Commands.CheckUser)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.Path, err)
	}
	owners, err := ResolveOwners(cfg.Listener.User)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.Path, err)
	}
	if uid == 0 && (sb.UID == owners.ListenerUID || sb.GID == owners.ListenerGID) {
		return nil, fmt.Errorf("%s: check_user and the listener user must be different users with different groups: a check that is taken over could write approvals (THREAT-MODEL.md T2). Fix: set [commands] check_user to its own user", cfg.Path)
	}
	if err := cfg.CheckGateFiles(uid); err != nil {
		return nil, err
	}
	dir := state.Dir{Path: filepath.Join(stateBase, cfg.Name)}
	return &Env{
		Cfg: cfg, Dir: dir, Spool: spool.Dirs{Base: spoolBase, Name: cfg.Name},
		UID: uid, Owners: owners, Sandbox: sb, Log: log, CmdOut: cmdOut,
		Git: &gitx.Git{Dir: dir.Repo(), Home: dir.Path, UID: uid, Key: cfg.Repo.DeployKey, KnownHosts: cfg.Repo.KnownHosts},
	}, nil
}

// command builds the exec.Cmd of argv. With sandbox it runs as check_user
// with no extra groups (rule 3).
func (e *Env) command(ctx context.Context, argv []string, c cmdEnv, sandbox bool) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv comes from the host config (rule 1)
	cmd.Env = append(e.environ(c), "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	cmd.Dir = c.Tree
	attr := &syscall.SysProcAttr{Setpgid: true}
	if sandbox && e.Sandbox.Switch {
		attr.Credential = &syscall.Credential{Uid: uint32(e.Sandbox.UID), Gid: uint32(e.Sandbox.GID), Groups: []uint32{}} //nolint:gosec // checked ids
	}
	cmd.SysProcAttr = attr
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// runSandboxed runs check or summary as check_user and returns its output.
func (e *Env) runSandboxed(parent context.Context, argv []string, c cmdEnv) (string, error) {
	ctx, cancel := context.WithTimeout(parent, e.Cfg.Commands.CheckTimeout.Duration)
	defer cancel()
	cmd := e.command(ctx, argv, c, true)
	out := &capBuf{}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	e.killSandbox()
	if err != nil && parent.Err() != nil {
		return "", fmt.Errorf("stopped: %w", parent.Err())
	}
	if ctx.Err() != nil && err != nil {
		err = fmt.Errorf("timed out after %s", e.Cfg.Commands.CheckTimeout.Duration)
	}
	return out.b.String(), err
}

// killSandbox stops every process of check_user, so that nothing of a
// check lives on to change a later check or summary.
func (e *Env) killSandbox() {
	if !e.Sandbox.Switch {
		return
	}
	kill, err := exec.LookPath("kill")
	if err != nil {
		e.Log.Warn("no kill program; processes of check_user may live on", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, kill, "-KILL", "-1") //nolint:gosec // fixed program
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(e.Sandbox.UID), Gid: uint32(e.Sandbox.GID), Groups: []uint32{}}} //nolint:gosec // checked ids
	_ = cmd.Run()
}

// runRoot runs apply or health as root. The output goes to the journal.
// takeAuto reads and removes the auto approval: a root-owned regular file.
func (e *Env) takeAuto() (*spool.Approval, error) {
	p := e.Dir.AutoFile()
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // root's own state directory
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		_ = os.Remove(p)
		return nil, fmt.Errorf("read the auto approval: %w", err)
	}
	defer func() { _ = f.Close() }()
	_ = os.Remove(p)
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Uid != e.UID || fi.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("the auto approval is not a private file of root; ignored")
	}
	b, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil {
		return nil, err
	}
	return &spool.Approval{Code: strings.TrimSpace(string(b)), Action: "apply"}, nil
}

func (e *Env) showRecipe(show func(string, string)) {
	if e.Cfg.Path != "" {
		p := recipe.RecordPath(filepath.Dir(e.Cfg.Path), e.Cfg.Name)
		if r, err := recipe.ReadInstanceRecord(p); err == nil {
			show("recipe", fmt.Sprintf("%s v%s (%s, instance %s, in %s)", r.Repo, r.Version, r.File, r.Instance, r.Dir))
		} else if !errors.Is(err, fs.ErrNotExist) {
			show("recipe", fmt.Sprintf("the record %s cannot be read: %v", p, err))
		}
	}
}

type capBuf struct {
	b   bytes.Buffer
	cut bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	if room := MaxCmdOutput - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
			c.cut = true
		} else {
			c.b.Write(p)
		}
	} else {
		c.cut = true
	}
	return len(p), nil
}
