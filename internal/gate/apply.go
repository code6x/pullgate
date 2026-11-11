package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/message"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

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

// Apply is one "pullgate apply" (DESIGN.md sections 9 and 10).
func (e *Env) Apply(ctx context.Context) (Result, error) {
	if err := e.Dir.Ensure(); err != nil {
		return "", err
	}
	lock, err := e.Dir.Lock(true)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Close() }()
	st, err := e.Dir.Load()
	if err != nil {
		return "", err
	}
	if st.InProgress != nil {
		return CutOff, e.cutOff(st)
	}
	paused, off := e.Dir.Paused(), e.Cfg.Mode == config.ModeOff
	if paused || off {
		return e.ignoreAll(st, paused)
	}
	res := Nothing
	for {
		a, err := e.next()
		var bad badFile
		if errors.As(err, &bad) {
			e.Log.Warn("bad approval, removed", "err", err)
			continue
		}
		if err != nil {
			return "", err
		}
		if a == nil {
			break
		}
		r, done, err := e.handle(ctx, st, *a)
		if r != "" {
			res = r
		}
		if err != nil || done {
			if serr := e.Dir.Save(st); serr != nil && err == nil {
				err = serr
			}
			return res, err
		}
	}
	return res, e.Dir.Save(st)
}

// badFile is a bad approval file; it was removed already.
type badFile struct{ error }

// next returns the next approval: the auto approval first, then the inbox
// in name order. It returns nil when there is none. A bad file comes back
// as a badFile error; other errors mean the inbox cannot be read.
func (e *Env) next() (*spool.Approval, error) {
	if a, err := e.takeAuto(); a != nil || err != nil {
		if err != nil {
			return nil, badFile{err}
		}
		return a, nil
	}
	es, err := e.Spool.TakeApprovals(e.Owners.ListenerUID, 1)
	if err != nil {
		return nil, err
	}
	if len(es) == 0 {
		return nil, nil
	}
	if es[0].Err != nil {
		return nil, badFile{fmt.Errorf("inbox file %s: %w", es[0].Name, es[0].Err)}
	}
	return &es[0].Approval, nil
}

// ignoreAll removes every approval while the instance is paused or off.
func (e *Env) ignoreAll(st *state.State, paused bool) (Result, error) {
	why := "the instance is off (mode = \"off\")."
	res := Off
	if paused {
		why = "the instance is paused. Run as root: <code>pullgate status " + e.Cfg.Name + " --resume</code>"
		res = Paused
	}
	for {
		a, err := e.next()
		var bad badFile
		if errors.As(err, &bad) {
			continue
		}
		if err != nil {
			return "", err
		}
		if a == nil {
			break
		}
		if i := st.FindCode(a.Code); i >= 0 {
			q := st.Remove(i)
			if m := st.Marks[q.SHA]; m != nil && m.Mark == state.MarkAsked {
				delete(st.Marks, q.SHA) // ask again after resume
			}
			e.reply(q.ID, "The tap for "+code(q.SHA)+" was ignored: "+why)
		}
		e.Log.Info("approval ignored", "reason", string(res))
	}
	return res, e.Dir.Save(st)
}

// handle acts on one approval. done is true after an apply.
func (e *Env) handle(ctx context.Context, st *state.State, a spool.Approval) (Result, bool, error) {
	i := st.FindCode(a.Code)
	if i < 0 {
		e.Log.Warn("approval with an unknown code ignored")
		return Ignored, false, nil
	}
	now := e.now()
	q := st.Remove(i) // the code works once
	if now.Before(q.Asked) || !now.Before(q.Expires) {
		if m := st.Marks[q.SHA]; m != nil && m.Mark == state.MarkAsked {
			st.SetMark(q.SHA, state.MarkExpired, now)
		}
		e.Log.Warn("approval out of its time; ignored", "question", q.ID, "sha", q.SHA)
		e.reply(q.ID, "The tap for "+code(q.SHA)+" came outside the time limit of the question, so it was ignored.")
		return Ignored, false, e.Dir.Save(st)
	}
	if err := e.Dir.Save(st); err != nil {
		return "", true, err
	}
	if a.Action == "skip" {
		st.SetMark(q.SHA, state.MarkSkipped, now)
		e.Log.Info("skipped", "sha", q.SHA)
		e.reply(q.ID, "Skipped "+code(q.SHA)+". A new commit asks again.")
		return Skipped, false, e.Dir.Save(st)
	}
	r, err := e.applySHA(ctx, st, q)
	ran := r != Ignored && r != Refused
	return r, ran || err != nil, err
}

// applySHA applies the SHA of question q, taken from root's own state.
func (e *Env) applySHA(ctx context.Context, st *state.State, q state.Question) (Result, error) {
	sha, now := q.SHA, e.now()
	if sha == st.Applied {
		e.reply(q.ID, code(sha)+" is applied already.")
		return Ignored, nil
	}
	if st.Applied != "" && !e.Cfg.Limits.AllowOlder {
		ok, err := e.Git.IsAncestor(ctx, st.Applied, sha)
		if err != nil {
			return "", err
		}
		if !ok {
			st.SetMark(sha, state.MarkRefused, now)
			e.Log.Warn("refused: not a descendant of the applied SHA", "sha", sha, "applied", st.Applied)
			e.reply(q.ID, "Refused "+code(sha)+": it is not a descendant of the applied commit "+code(st.Applied)+".")
			return Refused, nil
		}
	}
	old := st.Applied
	st.InProgress = &state.InProgress{Phase: "apply", SHA: sha, OldSHA: old, Started: now}
	if err := e.Dir.Save(st); err != nil {
		return "", err
	}
	t, err := e.newTrees()
	if err != nil {
		return "", err
	}
	defer t.remove()

	err = e.tryApply(ctx, t, "apply", sha, old, st.Held, false)
	if stopped(ctx, err) {
		return "", err // in_progress stays: the next start pauses
	}
	if err == nil {
		st.Applied, st.AppliedAt = sha, e.now()
		st.InProgress, st.Held = nil, ""
		st.Marks = map[string]*state.Mark{}
		if gerr := e.Git.SetApplied(ctx, sha); gerr != nil {
			e.Log.Warn("keep the applied ref", "err", gerr)
		}
		e.afterApply(st)
		if err := e.Dir.Save(st); err != nil {
			return "", err
		}
		e.Log.Info("applied", "sha", sha)
		e.reply(q.ID, "✅ Applied "+code(sha)+".")
		return Applied, nil
	}

	m := st.SetMark(sha, state.MarkFailed, e.now())
	m.Tries++
	if err := e.Dir.Save(st); err != nil {
		return "", err
	}
	failure := esc(err)
	if e.Cfg.OnFailure == config.OnFailureRollback && old != "" {
		st.InProgress = &state.InProgress{Phase: "rollback", SHA: sha, OldSHA: old, Started: e.now()}
		if err := e.Dir.Save(st); err != nil {
			return "", err
		}
		rerr := e.tryApply(ctx, t, "rb", old, sha, sha, true)
		if stopped(ctx, rerr) {
			return "", rerr
		}
		if rerr == nil {
			st.InProgress = nil
			if err := e.Dir.Save(st); err != nil {
				return "", err
			}
			e.reply(q.ID, "❌ The apply of "+code(sha)+" failed ("+failure+"). Rolled back to "+code(old)+".")
			return RolledBack, nil
		}
		if perr := e.Dir.Pause("the rollback to " + old + " failed after the apply of " + sha + " failed"); perr != nil {
			// in_progress stays, so the next start pauses (cutOff).
			e.Log.Error("write the pause file", "err", perr)
			e.reply(q.ID, "❌ The apply of "+code(sha)+" failed, the rollback failed too, and the pause file could not be written. Check the host now.")
			return RollbackError, perr
		}
		st.InProgress = nil
		if err := e.Dir.Save(st); err != nil {
			return "", err
		}
		e.reply(q.ID, "❌ The apply of "+code(sha)+" failed ("+failure+"), and the rollback to "+code(old)+" failed too ("+esc(rerr)+"). The instance is paused. Check the host, then run as root: <code>pullgate status "+e.Cfg.Name+" --resume</code>")
		return RollbackError, nil
	}
	// on_failure = "pause", or nothing to roll back to: hold the SHA.
	st.Held = sha
	st.InProgress = nil
	if err := e.Dir.Save(st); err != nil {
		return "", err
	}
	e.reply(q.ID, "❌ The apply of "+code(sha)+" failed ("+failure+"). It is held and not tried again. A newer commit will be asked, so you can fix it forward.")
	return Held, nil
}

// stopped reports whether err comes from a stop of pullgate itself.
func stopped(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, ErrStopped))
}

// tryApply exports fresh trees and runs apply, then health.
func (e *Env) tryApply(ctx context.Context, t *trees, tag, sha, old, failed string, rollback bool) error {
	p, err := e.prepare(ctx, t, tag, sha, old)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	p.env.Failed = failed
	p.env.Rollback = rollback
	if err := e.runRoot(ctx, "apply", e.Cfg.Commands.Apply, p.env); err != nil {
		return err
	}
	if argv := e.Cfg.Commands.Health; len(argv) > 0 {
		if err := e.runRoot(ctx, "health", argv, p.env); err != nil {
			return err
		}
	}
	return nil
}

// afterApply closes questions whose base is no longer the applied SHA, so
// the next run asks again with a right summary.
func (e *Env) afterApply(st *state.State) {
	keep := st.Questions[:0]
	for _, q := range st.Questions {
		if q.Base == st.Applied {
			keep = append(keep, q)
			continue
		}
		if !q.Auto {
			e.edit(q.ID, "This question for "+code(q.SHA)+" is closed: the applied commit changed. The next run asks again.")
		}
	}
	st.Questions = keep
}

func esc(err error) string { return message.Esc(err.Error()) }
