package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/ghdeploy"
	"github.com/code6x/pullgate/internal/message"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

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
	e.ghOpen()
	defer e.ghDone()
	st, err := e.Dir.Load()
	if err != nil {
		return "", err
	}
	e.ghCheck(st)
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
		why = "the instance is paused. " + e.resumeHint()
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
		e.reply(q.ID, "Skipped "+code(q.SHA)+". "+e.skipHint())
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
	if b := st.Broken; b != nil {
		// Only a fix forward: a commit on top of the last failed one.
		// An auto question is never applied here: the owner must tap.
		ok := sha != b.SHA && sha != b.OldSHA && !q.Auto
		if ok {
			var err error
			if ok, err = e.Git.IsAncestor(ctx, b.SHA, sha); err != nil {
				return "", err
			}
		}
		if !ok {
			st.SetMark(sha, state.MarkRefused, now)
			e.Log.Warn("broken: refused, not a fix forward", "sha", sha, "failed", b.SHA)
			e.reply(q.ID, "Refused "+code(sha)+": the instance is broken, and only a commit on top of the failed commit "+code(b.SHA)+" is applied.")
			return Refused, nil
		}
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
	// Each GitHub post is queued before the state save of its step
	// (DESIGN.md section 22).
	gk := e.ghStart("apply", sha)
	st.InProgress = &state.InProgress{Phase: "apply", SHA: sha, OldSHA: old, Started: now}
	if err := e.Dir.Save(st); err != nil {
		// The command never runs: the save at the end of Apply must not
		// write in_progress, or the next start would see a cut-off.
		st.InProgress = nil
		return "", err
	}
	e.ghSend() // the create and in_progress go out before the apply command
	t, err := e.newTrees()
	if err != nil {
		return "", err
	}
	defer t.remove()

	wasBroken := st.Broken
	report, err := e.tryApply(ctx, t, "apply", sha, old, failedSHA(st), false)
	if stopped(ctx, err) {
		return "", err // in_progress stays: the next start pauses
	}
	if err == nil {
		st.Applied, st.AppliedAt = sha, e.now()
		st.InProgress, st.Held, st.Broken = nil, "", nil
		st.Marks = map[string]*state.Mark{}
		if gerr := e.Git.SetApplied(ctx, sha); gerr != nil {
			e.Log.Warn("keep the applied ref", "err", gerr)
		}
		e.afterApply(st)
		if wasBroken != nil {
			e.ghStatus(gk, ghdeploy.Success, ghdeploy.DescFixedForward)
		} else {
			e.ghStatus(gk, ghdeploy.Success, ghdeploy.DescApplied)
		}
		if err := e.Dir.Save(st); err != nil {
			return "", err
		}
		e.Log.Info("applied", "sha", sha)
		if wasBroken != nil {
			e.reply(q.ID, withReport("✅ Applied "+code(sha)+". The fix forward worked: the instance is no longer broken.", report))
			return Applied, nil
		}
		e.reply(q.ID, withReport("✅ Applied "+code(sha)+".", report))
		return Applied, nil
	}

	m := st.SetMark(sha, state.MarkFailed, e.now())
	m.Tries++
	if err := e.Dir.Save(st); err != nil {
		return "", err
	}
	failure := message.Reason(err.Error())
	if b := st.Broken; b != nil {
		// A failed fix forward: no rollback against a half state.
		b.SHA, b.Reason = sha, "the fix forward "+message.Short(sha)+" failed: "+message.CutLine(err.Error())
		st.InProgress = nil
		e.ghStatus(gk, ghdeploy.Error, ghdeploy.DescFixForwardFailed)
		if err := e.Dir.Save(st); err != nil {
			return "", err
		}
		e.Log.Error("the fix forward failed; the instance stays broken", "sha", sha)
		e.reply(q.ID, withReport("❌ The fix forward "+code(sha)+" failed ("+failure+"). No rollback was tried. The instance stays broken.\n"+e.waysOut(), report))
		return StillBroken, nil
	}
	if e.Cfg.OnFailure == config.OnFailureRollback && old != "" {
		e.ghStatus(gk, ghdeploy.Failure, ghdeploy.DescRollbackStarts)
		rk := e.ghStart("rollback", old)
		applying := st.InProgress
		st.InProgress = &state.InProgress{Phase: "rollback", SHA: sha, OldSHA: old, Started: e.now()}
		if err := e.Dir.Save(st); err != nil {
			// The rollback never runs: the save at the end of Apply keeps
			// the apply phase, which the next start cuts off.
			st.InProgress = applying
			return "", err
		}
		e.ghSend() // the failure and the rollback's create go out before the rollback
		_, rerr := e.tryApply(ctx, t, "rb", old, sha, sha, true)
		if stopped(ctx, rerr) {
			return "", rerr
		}
		if rerr == nil {
			st.InProgress = nil
			e.ghStatus(rk, ghdeploy.Success, ghdeploy.DescRolledBack)
			if err := e.Dir.Save(st); err != nil {
				return "", err
			}
			e.reply(q.ID, withReport("❌ The apply of "+code(sha)+" failed ("+failure+"). Rolled back to "+code(old)+".", report))
			return RolledBack, nil
		}
		// The broken state (DESIGN.md section 10). If this save fails,
		// in_progress stays in the file, and the next start pauses.
		st.InProgress = nil
		st.Broken = &state.Broken{
			Since: e.now(), SHA: sha, OldSHA: old,
			Reason: "the apply of " + message.Short(sha) + " failed: " + message.CutLine(err.Error()) +
				"; the rollback to " + message.Short(old) + " failed: " + message.CutLine(rerr.Error()),
		}
		// Questions asked before have no warning (or are auto ones): drop
		// them all, so no old tap applies. The next run asks again.
		e.dropQuestions(st, "the instance is broken. A fix forward is asked again, with a warning.")
		e.ghStatus(rk, ghdeploy.Error, ghdeploy.DescBroken)
		if err := e.Dir.Save(st); err != nil {
			// Keep in_progress, so the next start pauses (a crash).
			st.Broken = nil
			st.InProgress = &state.InProgress{Phase: "rollback", SHA: sha, OldSHA: old, Started: e.now()}
			return RollbackError, err
		}
		e.Log.Error("the apply and its rollback failed; the instance is broken", "sha", sha, "old", old)
		e.reply(q.ID, withReport("❌ The apply of "+code(sha)+" failed ("+failure+"), and the rollback to "+code(old)+" failed too ("+message.Reason(rerr.Error())+"). The host may be in a half state. The instance is now broken.\n"+e.waysOut(), report))
		return RollbackError, nil
	}
	// on_failure = "pause", or nothing to roll back to: hold the SHA.
	st.Held = sha
	st.InProgress = nil
	e.ghStatus(gk, ghdeploy.Failure, ghdeploy.DescHeld)
	if err := e.Dir.Save(st); err != nil {
		return "", err
	}
	e.reply(q.ID, withReport("❌ The apply of "+code(sha)+" failed ("+failure+"). It is held and not tried again. A newer commit will be asked, so you can fix it forward.", report))
	return Held, nil
}

// dropQuestions removes every question from the state, and closes the
// open one in the chat.
func (e *Env) dropQuestions(st *state.State, why string) {
	for _, q := range st.Questions {
		if m := st.Marks[q.SHA]; m != nil && m.Mark == state.MarkAsked {
			delete(st.Marks, q.SHA)
		}
		if q.Open && !q.Auto {
			e.edit(q.ID, "This question for "+code(q.SHA)+" is closed: "+why)
		}
	}
	st.Questions = nil
}

// waysOut says how the broken state ends.
func (e *Env) waysOut() string {
	return "Two ways out: push a fix forward (a commit on top of the failed one; it is asked with a warning), or check the host and run as root: <code>pullgate status " + e.Cfg.Name + " --resume</code>"
}

// stopped reports whether err comes from a stop of pullgate itself.
func stopped(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, ErrStopped))
}

// tryApply exports fresh trees and runs apply, then health. Outside a
// rollback, apply gets a report file; report is its text for the
// message (DESIGN.md section 11), "" when it is empty.
func (e *Env) tryApply(ctx context.Context, t *trees, tag, sha, old, failed string, rollback bool) (report string, err error) {
	p, err := e.prepare(ctx, t, tag, sha, old)
	if err != nil {
		return "", fmt.Errorf("prepare: %w", err)
	}
	p.env.Failed = failed
	p.env.Rollback = rollback
	if !rollback {
		p.env.Report = filepath.Join(t.dir, tag+"-report")
		f, err := os.OpenFile(p.env.Report, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", fmt.Errorf("prepare: make the report file: %w", err)
		}
		made, serr := f.Stat()
		_ = f.Close()
		if serr != nil {
			return "", fmt.Errorf("prepare: the report file: %w", serr)
		}
		defer func() { report = e.readReport(p.env.Report, made) }()
	}
	if err := e.runRoot(ctx, "apply", e.Cfg.Commands.Apply, p.env); err != nil {
		return "", err
	}
	if argv := e.Cfg.Commands.Health; len(argv) > 0 {
		h := p.env
		h.Report = "" // the report file is for apply only
		if err := e.runRoot(ctx, "health", argv, h); err != nil {
			return "", err
		}
	}
	return "", nil
}

// maxReport is the most bytes of the report file that root reads.
const maxReport = 64 << 10

// readReport reads the report file of apply: the same file that root
// made (made), not a link, at most maxReport bytes. A file that cannot be read is logged and
// shown as a short note; it never stops the report of the apply.
func (e *Env) readReport(path string, made os.FileInfo) string {
	f, err := os.OpenFile(path, reportReadFlags, 0) //nolint:gosec // root made this file in its own trees directory
	if errors.Is(err, fs.ErrNotExist) {
		return "" // apply removed it: no report
	}
	if err != nil {
		e.Log.Warn("read the report file of apply", "err", err)
		return "(The report file of apply could not be read. See the journal.)"
	}
	defer func() { _ = f.Close() }()
	// Only the file that root made: not a link, not another file put in
	// its place (a hard link to another file has another inode).
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || !os.SameFile(fi, made) {
		e.Log.Warn("the report file of apply is not the file that root made", "path", path)
		return "(The report file of apply is not the file that pullgate made, so it is not shown.)"
	}
	b, err := io.ReadAll(io.LimitReader(f, maxReport+1))
	if err != nil {
		e.Log.Warn("read the report file of apply", "err", err)
		return "(The report file of apply could not be read. See the journal.)"
	}
	truncated := len(b) > maxReport
	if truncated {
		b = b[:maxReport]
	}
	return message.Report(string(b), truncated)
}

// withReport adds the report of apply to a message.
func withReport(text, report string) string {
	if report == "" {
		return text
	}
	return text + "\n" + report
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
