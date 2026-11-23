package gate

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/gitx"
	"github.com/code6x/pullgate/internal/message"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

// Result says what a run or an apply did, for tests and logs.
type Result string

// Results.
const (
	Busy          Result = "busy"
	Paused        Result = "paused"
	Off           Result = "off"
	CutOff        Result = "cut-off"
	Nothing       Result = "nothing"
	Settling      Result = "settling"
	NothingToDo   Result = "nothing-to-apply"
	Refused       Result = "refused"
	LimitReached  Result = "limit"
	CheckLimited  Result = "check-limited"
	CheckFailed   Result = "check-failed"
	Asked         Result = "asked"
	Sent          Result = "sent"          // in the outbox; no receipt from the listener yet
	NotDelivered  Result = "not-delivered" // the receipt says the listener could not send it
	AutoQueued    Result = "auto"
	Applied       Result = "applied"
	Skipped       Result = "skipped"
	Ignored       Result = "ignored"
	RolledBack    Result = "rolled-back"
	RollbackError Result = "rollback-failed"
	Held          Result = "held"
	StillBroken   Result = "still-broken"
)

func (e *Env) head() string { return message.Head(e.Cfg.Host, e.Cfg.Name) }

func (e *Env) notice(text string) {
	if _, err := e.Spool.Send(e.Owners.ListenerGID, spool.Message{Kind: spool.KindNotice, Text: e.head() + "\n" + text}); err != nil {
		e.Log.Error("write a notice to the outbox", "err", err)
	}
}

func (e *Env) reply(q int64, text string) {
	if _, err := e.Spool.Send(e.Owners.ListenerGID, spool.Message{Kind: spool.KindReply, Question: q, Text: e.head() + "\n" + text}); err != nil {
		e.Log.Error("write a reply to the outbox", "err", err)
	}
}

func (e *Env) edit(q int64, text string) {
	if _, err := e.Spool.Send(e.Owners.ListenerGID, spool.Message{Kind: spool.KindEdit, Question: q, Text: e.head() + "\n" + text}); err != nil {
		e.Log.Error("write an edit to the outbox", "err", err)
	}
}

// code shows a SHA in a message: short, in <code> (DESIGN.md section 12).
func code(sha string) string { return message.SHA(sha) }

// cutOff handles an apply in progress found at start (DESIGN.md section 10).
func (e *Env) cutOff(st *state.State) error {
	ip := st.InProgress
	e.Log.Error("an apply was cut off; pausing", "phase", ip.Phase, "sha", ip.SHA)
	if err := e.Dir.Pause("the " + ip.Phase + " of " + ip.SHA + " was cut off"); err != nil {
		return err
	}
	m := st.SetMark(ip.SHA, state.MarkFailed, e.now())
	m.Tries++
	st.InProgress = nil
	if err := e.Dir.Save(st); err != nil {
		return err
	}
	e.notice(fmt.Sprintf("⚠ The %s of %s was cut off. The host may be part way. The instance is paused.\nCheck the host, then run as root: <code>pullgate status %s --resume</code>", ip.Phase, code(ip.SHA), e.Cfg.Name))
	return nil
}

// expire removes questions whose time is up (DESIGN.md section 7, step 4).
func (e *Env) expire(st *state.State) {
	now := e.now()
	keep := st.Questions[:0]
	for _, q := range st.Questions {
		if now.Before(q.Expires) && !now.Before(q.Asked) {
			keep = append(keep, q)
			continue
		}
		if m := st.Marks[q.SHA]; m != nil && m.Mark == state.MarkAsked {
			st.SetMark(q.SHA, state.MarkExpired, now)
		}
		if q.Open && !q.Auto {
			e.edit(q.ID, "This question for "+code(q.SHA)+" expired. "+e.againHint())
		}
		e.Log.Info("question expired", "question", q.ID, "sha", q.SHA)
	}
	st.Questions = keep
}

func (e *Env) closeOpen(st *state.State, why string) {
	if q := st.Open(); q != nil {
		q.Open = false
		if m := st.Marks[q.SHA]; m != nil && m.Mark == state.MarkAsked {
			delete(st.Marks, q.SHA) // a closed question does not block a later ask
		}
		if !q.Auto {
			e.edit(q.ID, "This question for "+code(q.SHA)+" is closed: "+why)
		}
	}
}

// Run is one "pullgate run" (DESIGN.md section 7). It also takes the check
// requests of /check (DESIGN.md section 8).
func (e *Env) Run(ctx context.Context) (Result, error) {
	if err := e.Dir.Ensure(); err != nil {
		return "", err
	}
	lock, err := e.Dir.Lock(false)
	if errors.Is(err, state.ErrBusy) {
		if !e.Spool.HasCheck() {
			e.Log.Info("busy: another run or apply holds the lock")
			return Busy, nil
		}
		// A check request waits for the lock, so that it is not lost and
		// the path unit does not start the run again and again.
		e.Log.Info("busy: a check request waits for the lock")
		lock, err = e.Dir.Lock(true)
		if err == nil && !e.Spool.HasCheck() {
			// Another run took the request (and replied, if it was a good one).
			_ = lock.Close()
			e.Log.Info("busy: the run before this one took the check request")
			return Busy, nil
		}
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Close() }()
	paused, off := e.Dir.Paused(), e.Cfg.Mode == config.ModeOff
	if (paused || off) && !e.Spool.HasCheck() {
		if paused {
			e.Log.Info("paused: the pause file exists", "file", e.Dir.PauseFile())
			return Paused, nil
		}
		e.Log.Info("off: mode is off")
		return Off, nil
	}
	if err := e.Spool.Ensure(e.UID, e.Owners); err != nil {
		return "", err
	}
	// A bad file is removed and is not a request: the run goes on as a
	// normal run, with no reply and no mark against the limit.
	requests := e.takeChecks()
	st, err := e.Dir.Load()
	if err != nil {
		return "", err
	}
	// Saved at once: a paused or off run below may end without a save,
	// and the receipts are gone.
	if r, w := e.takeReceipts(st), e.warnLate(st, e.now()); r || w {
		if err := e.Dir.Save(st); err != nil {
			return "", err
		}
	}
	// Resume before pause: when both came, the pause wins (fail closed).
	if requests.Resume > 0 {
		e.resumeRequest(st)
		paused = e.Dir.Paused()
	}
	if requests.Pause > 0 {
		e.pauseRequest(st)
		paused = e.Dir.Paused()
	}
	if requests.AskAgain > 0 {
		e.askAgainRequest(st)
	}
	if requests.Status > 0 {
		e.statusRequest(st)
	}
	// Then the normal run goes on: /status never skips a fetch, and after
	// /pause the paused check below ends the run.
	requested := requests.Check > 0
	if requested {
		if r := e.limitCheck(st); r != "" {
			return r, e.Dir.Save(st)
		}
		if err := e.Dir.Save(st); err != nil {
			return "", err
		}
	}
	if paused || off {
		why := e.Cfg.Name + " is off (mode = \"off\"), so nothing was fetched."
		res := Off
		if paused {
			why = e.Cfg.Name + " is paused, so nothing was fetched. " + e.resumeHint()
			res = Paused
		}
		e.Log.Info(string(res))
		if requested {
			e.notice(why)
		}
		return res, nil
	}
	if st.InProgress != nil {
		return CutOff, e.cutOff(st)
	}
	limitNote := st.LimitNote
	res, err := e.fetchAndDecide(ctx, st)
	if requested {
		e.checkReply(st, res, err, !st.LimitNote.Equal(limitNote))
	}
	if serr := e.Dir.Save(st); serr != nil && err == nil {
		err = serr
	}
	return res, err
}

func (e *Env) fetchAndDecide(ctx context.Context, st *state.State) (Result, error) {
	if err := e.Git.Init(ctx); err != nil {
		return "", err
	}
	e.Log.Info("fetch", "branch", e.Cfg.Repo.Branch)
	tip, err := e.Git.Fetch(ctx, e.Cfg.Repo.URL, e.Cfg.Repo.Branch)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	e.Log.Info("fetched", "tip", tip, "applied", st.Applied)
	if now := e.now(); tip != st.LastTip || st.TipSince.IsZero() || st.TipSince.After(now) {
		// A new tip, or a clock that went back: the settle time starts
		// now (fail closed: never sooner).
		st.TipSince = now
	}
	st.LastTip = tip
	e.expire(st)
	return e.decide(ctx, st, tip)
}

func (e *Env) decide(ctx context.Context, st *state.State, tip string) (Result, error) {
	now := e.now()
	open := st.Open()
	switch {
	case tip == st.Applied:
		e.closeOpen(st, "the branch is back to the applied commit.")
		return Nothing, nil
	case open != nil && open.SHA == tip:
		return Nothing, nil
	case tip == st.Held:
		e.Log.Info("the tip is the held SHA; waiting for a new commit", "sha", tip)
		return Nothing, nil
	}
	if b := st.Broken; b != nil && (tip == b.SHA || tip == b.OldSHA) {
		e.Log.Info("broken: the tip is the failed or the old commit; waiting for a fix forward", "sha", tip)
		return Nothing, nil
	}
	// In the broken state the owner is always asked, also in auto mode.
	ask := e.Cfg.Mode == config.ModeAsk || st.Broken != nil
	if m := st.Marks[tip]; m != nil {
		// No retry in the broken state: a rollback is never tried there.
		retry := st.Broken == nil && m.Mark == state.MarkFailed && e.Cfg.OnFailure == config.OnFailureRollback && m.Tries < e.Cfg.Limits.MaxFailedTries
		reask := e.reaskOn() && e.reaskDue(m, now)
		if !retry && !reask {
			e.Log.Info("the tip was handled before; waiting for a new commit", "sha", tip, "mark", m.Mark)
			return Nothing, nil
		}
		if reask {
			e.Log.Info("reask_after: asking once more", "sha", tip, "mark", m.Mark, "since", m.At)
		}
	}
	if until, ok := e.settling(st, now); ok {
		e.Log.Info("settling: the tip changed not long ago; nothing is asked yet", "sha", tip, "until", until)
		return Settling, nil
	}
	notDesc := false
	if st.Applied != "" {
		ok, err := e.Git.IsAncestor(ctx, st.Applied, tip)
		if err != nil {
			return "", err
		}
		if !ok {
			if !e.Cfg.Limits.AllowOlder {
				st.SetMark(tip, state.MarkRefused, now)
				e.Log.Warn("refused: the tip does not descend from the applied SHA", "tip", tip, "applied", st.Applied)
				e.notice("⚠ Refused " + code(tip) + ": it is not a descendant of the applied commit " + code(st.Applied) + " (an older commit, a force push, or a rewritten branch). Nothing was asked.\nTo allow it, set <code>allow_older = true</code> in [limits] of the host config.")
				return Refused, nil
			}
			notDesc = true
		}
	}
	if b := st.Broken; b != nil {
		ok, err := e.Git.IsAncestor(ctx, b.SHA, tip)
		if err != nil {
			return "", err
		}
		if !ok {
			st.SetMark(tip, state.MarkRefused, now)
			e.Log.Warn("broken: refused, the tip does not descend from the failed commit", "tip", tip, "failed", b.SHA)
			e.notice("⚠ Refused " + code(tip) + ": the instance is broken, and only a fix forward is asked: a commit on top of the failed commit " + code(b.SHA) + ". Nothing was asked.\nPush a fix on top of it, or check the host and run as root: <code>pullgate status " + e.Cfg.Name + " --resume</code>")
			return Refused, nil
		}
	}
	// Questions per day.
	var recent []time.Time
	for _, t := range st.AskTimes {
		if now.Sub(t) < 24*time.Hour && !t.After(now) {
			recent = append(recent, t)
		}
	}
	st.AskTimes = recent
	if ask && len(recent) >= e.Cfg.Limits.QuestionsPerDay {
		e.Log.Warn("questions_per_day reached; not asking", "tip", tip)
		if now.Sub(st.LimitNote) >= 24*time.Hour {
			st.LimitNote = now
			e.notice(fmt.Sprintf("⚠ %d questions in the last 24 hours: the limit questions_per_day is reached. %s waits.", len(recent), code(tip)))
		}
		return LimitReached, nil
	}

	t, err := e.newTrees()
	if err != nil {
		return "", err
	}
	defer t.remove()
	p, err := e.prepare(ctx, t, "run", tip, st.Applied)
	if err != nil {
		var hostile *gitx.HostileError
		if errors.As(err, &hostile) {
			// The commit itself is bad: it is not tried again.
			st.SetMark(tip, state.MarkCheckFailed, now)
			e.notice("⚠ " + code(tip) + " is refused, so it is not asked: " + message.Code(err.Error()))
			return CheckFailed, nil
		}
		return "", fmt.Errorf("export %s: %w", tip, err)
	}
	p.env.Failed = failedSHA(st)
	if argv := e.Cfg.Commands.Check; len(argv) > 0 {
		out, err := e.runSandboxed(ctx, argv, p.env)
		if exitCode(err) == CheckExitNothing && st.Broken == nil {
			// The check says this commit changes nothing on this host.
			// No message, and the applied SHA stays, so that the next
			// question lists every commit since the last real apply. In
			// the broken state exit 3 is a failed check with a notice:
			// the owner must not miss that a fix forward was not asked.
			st.SetMark(tip, state.MarkNothing, now)
			e.Log.Info("check: nothing to apply on this host; not asking", "sha", tip)
			return NothingToDo, nil
		}
		if err != nil {
			st.SetMark(tip, state.MarkCheckFailed, now)
			e.Log.Warn("check failed; not asking", "sha", tip, "err", err)
			e.notice("⚠ The check of " + code(tip) + " failed, so it is not asked and not checked again: " + message.Code(err.Error()) + "\n" + message.Output(shortTrees(out, p.env.Tree, p.env.OldTree), 10))
			return CheckFailed, nil
		}
		e.Log.Info("check passed", "sha", tip)
	}
	q := &message.Question{
		Host: e.Cfg.Host, Instance: e.Cfg.Name, SHA: tip, Applied: st.Applied,
		Held: st.Held, Broken: brokenReason(st), NotDescendant: notDesc, Files: p.files, Watch: e.Cfg.Watch.Paths,
		Submodules: p.tree.Submodules,
	}
	for _, s := range p.tree.Symlinks {
		q.Symlinks = append(q.Symlinks, message.Symlink{Path: s.Path, Target: s.Target})
	}
	q.Subjects, q.TotalCommits, err = e.Git.Subjects(ctx, st.Applied, tip, 50)
	if err != nil {
		return "", err
	}
	if argv := e.Cfg.Commands.Summary; len(argv) > 0 {
		out, err := e.runSandboxed(ctx, argv, p.env)
		if err != nil {
			q.SummaryFailed = err.Error()
		} else {
			q.Summary = out
		}
	}
	text := q.Text()

	e.closeOpen(st, "a newer commit replaced this question.")
	c, err := state.NewCode()
	if err != nil {
		return "", err
	}
	nq := state.Question{
		ID: st.NextID, Code: c, SHA: tip, Base: st.Applied, Asked: now,
		Expires: now.Add(e.Cfg.Limits.ApprovalTTL.Duration), Open: true,
		Auto: !ask,
	}
	if ask {
		// Saved with the question before the file exists, so a receipt
		// always finds its question, also after a crash.
		nq.Outbox = spool.NewMessageName()
	}
	st.NextID++
	st.Questions = append(st.Questions, nq)
	st.SetMark(tip, state.MarkAsked, now)
	if err := e.Dir.Save(st); err != nil {
		return "", err
	}
	// undo forgets the question when it could not be delivered, so the
	// next run tries again.
	undo := func(err error) (Result, error) {
		if i := st.FindCode(c); i >= 0 {
			st.Remove(i)
		}
		delete(st.Marks, tip)
		return "", err
	}
	if nq.Auto {
		if err := state.WriteFileAtomic(e.Dir.AutoFile(), []byte(c+"\n"), 0o600); err != nil {
			return undo(err)
		}
		e.Log.Info("auto mode: apply queued", "sha", tip)
		return AutoQueued, nil
	}
	if _, err := e.Spool.SendAs(e.Owners.ListenerGID, nq.Outbox, spool.Message{Kind: spool.KindAsk, Question: nq.ID, Code: c, Expires: nq.Expires, Text: text}); err != nil {
		return undo(err)
	}
	st.AskTimes = append(st.AskTimes, now)
	// "asked" is logged only after the receipt of the listener (takeReceipts).
	switch e.waitReceipt(st, c, nq.Outbox) {
	case state.DeliverySent:
		return Asked, nil
	case state.DeliveryFailed:
		return NotDelivered, fmt.Errorf("the listener could not read or send question %d for %s, and removed it. Fix: run \"pullgate doctor\" as root, then \"pullgate status %s --ask-again\"", nq.ID, tip, e.Cfg.Name)
	}
	e.Log.Info("sent: the question is in the outbox; no receipt from the listener yet. The next run checks it", "sha", tip, "question", nq.ID)
	return Sent, nil
}

// CheckExitNothing is the exit status of check that means "this commit
// has nothing to apply on this host" (DESIGN.md section 11).
const CheckExitNothing = 3

// exitCode is the exit status of a command that ran to its end and
// failed, else -1 (no error, a timeout, a signal, or no start).
func exitCode(err error) int {
	var ee *exec.ExitError
	if err == nil || !errors.As(err, &ee) {
		return -1
	}
	return ee.ExitCode()
}

// settling says whether the tip changed less than settle ago, and until
// when it waits ([limits] settle).
func (e *Env) settling(st *state.State, now time.Time) (time.Time, bool) {
	s := e.Cfg.Limits.Settle.Duration
	if s <= 0 {
		return time.Time{}, false
	}
	until := st.TipSince.Add(s)
	return until, st.TipSince.IsZero() || now.Before(until)
}

// reaskDue says whether a skipped or expired tip is asked once more
// ([limits] reask_after). Each new Skip or expiry starts the wait again.
func (e *Env) reaskDue(m *state.Mark, now time.Time) bool {
	r := e.Cfg.Limits.ReaskAfter.Duration
	if !e.reaskOn() || (m.Mark != state.MarkSkipped && m.Mark != state.MarkExpired) {
		return false
	}
	return !m.At.After(now) && now.Sub(m.At) >= r
}

// reaskOn says whether reask_after is used: only in ask mode, where a
// question has Skip.
func (e *Env) reaskOn() bool {
	return e.Cfg.Limits.ReaskAfter.Duration > 0 && e.Cfg.Mode == config.ModeAsk
}

// againHint says when a skipped or expired tip is asked again.
func (e *Env) againHint() string {
	h := "A new commit, or <code>pullgate status " + e.Cfg.Name + " --ask-again</code>, asks again."
	if e.reaskOn() {
		h += " Else it is asked once more after " + e.Cfg.Limits.ReaskAfter.String() + " (reask_after)."
	}
	return h
}

// skipHint says when a skipped tip is asked again.
func (e *Env) skipHint() string {
	if e.reaskOn() {
		return "A new commit asks again, or this one once more after " + e.Cfg.Limits.ReaskAfter.String() + " (reask_after)."
	}
	return "A new commit asks again."
}

// shortTrees replaces the long tree paths in command output with NEW and
// OLD: they say nothing to the owner and push the reason out of the
// message (code6x/pullgate#25).
func shortTrees(out, tree, oldTree string) string {
	if tree != "" {
		out = strings.ReplaceAll(out, tree, "NEW")
	}
	if oldTree != "" {
		out = strings.ReplaceAll(out, oldTree, "OLD")
	}
	return out
}

// failedSHA is PULLGATE_FAILED_SHA: the last commit that failed in the
// broken state, else the held SHA.
func failedSHA(st *state.State) string {
	if st.Broken != nil {
		return st.Broken.SHA
	}
	return st.Held
}

func brokenReason(st *state.State) string {
	if st.Broken == nil {
		return ""
	}
	return st.Broken.Reason
}
