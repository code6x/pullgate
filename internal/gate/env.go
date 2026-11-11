// Package gate is root's side: "pullgate run", "pullgate apply" and
// "pullgate status" (DESIGN.md sections 7, 9, 10 and 13).
package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

// Sandbox is the user that runs check and summary (rule 3).
type Sandbox struct {
	UID, GID int
	Switch   bool // set the credential (pullgate runs as root)
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

// Env holds what root needs for one instance.
type Env struct {
	Cfg     *config.Instance
	Dir     state.Dir
	Spool   spool.Dirs
	UID     uint32 // the user that owns the gate files (root)
	Owners  spool.Owners
	Sandbox Sandbox
	Git     *gitx.Git
	Log     *slog.Logger
	CmdOut  io.Writer // where the output of apply and health goes (the journal)
	Now     func() time.Time
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

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// MaxCmdOutput is the most output kept from a command.
const MaxCmdOutput = 64 << 10

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

// cmdEnv is the environment of a command (DESIGN.md section 11).
type cmdEnv struct {
	SHA, OldSHA, Tree, OldTree, Changed, Failed string
	Rollback                                    bool
}

func (e *Env) environ(c cmdEnv) []string {
	rb := ""
	if c.Rollback {
		rb = "1"
	}
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"PULLGATE_INSTANCE=" + e.Cfg.Name,
		"PULLGATE_SHA=" + c.SHA,
		"PULLGATE_OLD_SHA=" + c.OldSHA,
		"PULLGATE_TREE=" + c.Tree,
		"PULLGATE_OLD_TREE=" + c.OldTree,
		"PULLGATE_CHANGED_FILES=" + c.Changed,
		"PULLGATE_FAILED_SHA=" + c.Failed,
		"PULLGATE_ROLLBACK=" + rb,
	}
}

// command builds the exec.Cmd of argv. With sandbox it runs as check_user
// with no extra groups (rule 3).
func (e *Env) command(ctx context.Context, argv []string, c cmdEnv, sandbox bool) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv comes from the host config (rule 1)
	cmd.Env = e.environ(c)
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
func (e *Env) runRoot(parent context.Context, what string, argv []string, c cmdEnv) error {
	ctx, cancel := context.WithTimeout(parent, e.Cfg.Commands.ApplyTimeout.Duration)
	defer cancel()
	cmd := e.command(ctx, argv, c, false)
	w := e.CmdOut
	if w == nil {
		w = io.Discard
	}
	cmd.Stdout = w
	cmd.Stderr = w
	e.Log.Info("start "+what, "sha", c.SHA, "rollback", c.Rollback)
	err := cmd.Run()
	if err != nil && parent.Err() != nil {
		return fmt.Errorf("%s stopped: %w", what, ErrStopped)
	}
	if ctx.Err() != nil && err != nil {
		err = fmt.Errorf("timed out after %s", e.Cfg.Commands.ApplyTimeout.Duration)
	}
	if err != nil {
		e.Log.Warn(what+" failed", "sha", c.SHA, "err", err)
		return fmt.Errorf("%s: %w", what, err)
	}
	e.Log.Info(what+" done", "sha", c.SHA)
	return nil
}

// ErrStopped means pullgate itself was stopped (a signal) during a
// command. The result of an apply is then unknown: in_progress stays, and
// the next start pauses (DESIGN.md section 10).
var ErrStopped = errors.New("pullgate was stopped")

// trees is the per-process directory of trees.
type trees struct {
	dir string
}

// newTrees removes trees that an earlier process left, and makes a new
// directory for this process. The caller holds the lock.
func (e *Env) newTrees() (*trees, error) {
	if err := e.Dir.Ensure(); err != nil {
		return nil, err
	}
	old, _ := filepath.Glob(filepath.Join(e.Dir.Trees(), "*"))
	hidden, _ := filepath.Glob(filepath.Join(e.Dir.Trees(), ".*"))
	for _, p := range append(old, hidden...) {
		if err := gitx.RemoveTree(p); err != nil {
			return nil, fmt.Errorf("remove an old tree %s: %w", p, err)
		}
	}
	d, err := os.MkdirTemp(e.Dir.Trees(), "p-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(d, 0o711); err != nil {
		return nil, err
	}
	return &trees{dir: d}, nil
}

func (t *trees) remove() { _ = gitx.RemoveTree(t.dir) }

func (e *Env) gid() int {
	if e.Sandbox.Switch {
		return e.Sandbox.GID
	}
	return -1
}

func (e *Env) limits() gitx.Limits {
	return gitx.Limits{MaxFiles: e.Cfg.Limits.MaxFiles, MaxBytes: e.Cfg.Limits.MaxTreeBytes}
}

// prepared is a SHA with its trees and the list of changed files.
type prepared struct {
	env   cmdEnv
	files []string
	tree  *gitx.Tree
}

// prepare exports sha (and old, if set) into t under the name tag, and
// writes the changed files list.
func (e *Env) prepare(ctx context.Context, t *trees, tag, sha, old string) (*prepared, error) {
	p := &prepared{env: cmdEnv{SHA: sha, OldSHA: old}}
	var err error
	p.env.Tree = filepath.Join(t.dir, tag+"-new")
	p.tree, err = e.Git.Export(ctx, sha, p.env.Tree, e.limits(), e.gid())
	if err != nil {
		return nil, err
	}
	if e.Cfg.Limits.RefuseSymlinks && len(p.tree.Symlinks) > 0 {
		return nil, &gitx.HostileError{Msg: fmt.Sprintf("the commit has a symlink out of the tree (%s), and refuse_symlinks is on", p.tree.Symlinks[0].Path)}
	}
	if old != "" {
		p.env.OldTree = filepath.Join(t.dir, tag+"-old")
		if _, err := e.Git.Export(ctx, old, p.env.OldTree, e.limits(), e.gid()); err != nil {
			return nil, err
		}
	}
	p.files, err = e.Git.ChangedFiles(ctx, old, sha)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, f := range p.files {
		b.WriteString(f)
		b.WriteByte(0)
	}
	p.env.Changed = filepath.Join(t.dir, tag+"-changed")
	if err := os.WriteFile(p.env.Changed, []byte(b.String()), 0o444); err != nil { //nolint:gosec // read-only list for the commands
		return nil, err
	}
	return p, nil
}
