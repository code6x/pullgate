// Package gate is root's side: "pullgate run", "pullgate apply" and
// "pullgate status" (DESIGN.md sections 7, 9, 10 and 13).
package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// Env holds what root needs for one instance.
type Env struct {
	Cfg         *config.Instance
	WindowsRoot string // protected data root on Windows
	Dir         state.Dir
	Spool       spool.Dirs
	UID         uint32 // the user that owns the gate files (root)
	Owners      spool.Owners
	Sandbox     Sandbox
	Git         *gitx.Git
	Log         *slog.Logger
	CmdOut      io.Writer // where the output of apply and health goes (the journal)
	Now         func() time.Time
	// ReceiptWait is how long a run waits for the receipt of a new
	// question: 0 is DefaultReceiptWait, below 0 is no wait (tests).
	ReceiptWait time.Duration
	// Doctor runs the checks of pullgate doctor that need no secret, for
	// this instance, and returns the redacted text (doctor.RunPublic,
	// code6x/pullgate#50). nil: /doctor answers that it is not available.
	Doctor func(ctx context.Context) string
	// GitHubHTTP is the HTTP client of the GitHub posts; nil is
	// ghdeploy.NewHTTPClient (tests give one for their fake server).
	GitHubHTTP *http.Client
	// GitHubSendLimit is the most time one process sends to GitHub; 0 is
	// ghdeploy.SendLimit.
	GitHubSendLimit time.Duration

	ghToken string // the token of [github]; read by New, never printed
	gh      *ghRun
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// MaxCmdOutput is the most output kept from a command.
const MaxCmdOutput = 64 << 10

// cmdEnv is the environment of a command (DESIGN.md section 11).
type cmdEnv struct {
	SHA, OldSHA, Tree, OldTree, Changed, Failed string
	Report                                      string // PULLGATE_REPORT_FILE; only for apply
	Rollback                                    bool
}

func (e *Env) environ(c cmdEnv) []string {
	rb := ""
	if c.Rollback {
		rb = "1"
	}
	env := []string{
		"PULLGATE_INSTANCE=" + e.Cfg.Name,
		"PULLGATE_SHA=" + c.SHA,
		"PULLGATE_OLD_SHA=" + c.OldSHA,
		"PULLGATE_TREE=" + c.Tree,
		"PULLGATE_OLD_TREE=" + c.OldTree,
		"PULLGATE_CHANGED_FILES=" + c.Changed,
		"PULLGATE_FAILED_SHA=" + c.Failed,
		"PULLGATE_ROLLBACK=" + rb,
	}
	if c.Report != "" {
		env = append(env, "PULLGATE_REPORT_FILE="+c.Report)
	}
	return env
}

func (e *Env) runRoot(parent context.Context, what string, argv []string, c cmdEnv) error {
	ctx, cancel := context.WithTimeout(parent, e.Cfg.Commands.ApplyTimeout.Duration)
	defer cancel()
	w := e.CmdOut
	if w == nil {
		w = io.Discard
	}
	e.Log.Info("start "+what, "sha", c.SHA, "rollback", c.Rollback)
	err := e.runApplyCommand(ctx, argv, c, w)
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
