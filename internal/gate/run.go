package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/code6x/pullgate/internal/config"
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
	Refused       Result = "refused"
	LimitReached  Result = "limit"
	CheckFailed   Result = "check-failed"
	Asked         Result = "asked"
	AutoQueued    Result = "auto"
	Applied       Result = "applied"
	Skipped       Result = "skipped"
	Ignored       Result = "ignored"
	RolledBack    Result = "rolled-back"
	RollbackError Result = "rollback-failed"
	Held          Result = "held"
)

func (e *Env) head() string { return message.Head(e.Cfg.Host, e.Cfg.Name) }

func (e *Env) notice(text string) {
	if err := e.Spool.Send(spool.Message{Kind: spool.KindNotice, Text: e.head() + "\n" + text}); err != nil {
		e.Log.Error("write a notice to the outbox", "err", err)
	}
}

func (e *Env) reply(q int64, text string) {
	if err := e.Spool.Send(spool.Message{Kind: spool.KindReply, Question: q, Text: e.head() + "\n" + text}); err != nil {
		e.Log.Error("write a reply to the outbox", "err", err)
	}
}

func (e *Env) edit(q int64, text string) {
	if err := e.Spool.Send(spool.Message{Kind: spool.KindEdit, Question: q, Text: e.head() + "\n" + text}); err != nil {
		e.Log.Error("write an edit to the outbox", "err", err)
	}
}

func code(sha string) string { return "<code>" + sha + "</code>" }

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
			e.edit(q.ID, "This question for "+code(q.SHA)+" expired. A new commit, or <code>pullgate status "+e.Cfg.Name+" --ask-again</code>, asks again.")
		}
		e.Log.Info("question expired", "question", q.ID, "sha", q.SHA)
	}
	st.Questions = keep
}

func (e *Env) closeOpen(st *state.State, why string) {
	if q := st.Open(); q != nil {
		q.Open = false
		if !q.Auto {
			e.edit(q.ID, "This question for "+code(q.SHA)+" is closed: "+why)
		}
	}
}

// Run is one "pullgate run" (DESIGN.md section 7).
func (e *Env) Run(ctx context.Context) (Result, error) {
	if err := e.Dir.Ensure(); err != nil {
		return "", err
	}
	lock, err := e.Dir.Lock(false)
	if errors.Is(err, state.ErrBusy) {
		e.Log.Info("busy: another run or apply holds the lock")
		return Busy, nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Close() }()
	if e.Dir.Paused() {
		e.Log.Info("paused: the pause file exists", "file", e.Dir.PauseFile())
		return Paused, nil
	}
	if e.Cfg.Mode == config.ModeOff {
		e.Log.Info("off: mode is off")
		return Off, nil
	}
	if err := e.Spool.Ensure(e.UID, e.Owners); err != nil {
		return "", err
	}
	st, err := e.Dir.Load()
	if err != nil {
		return "", err
	}
	if st.InProgress != nil {
		return CutOff, e.cutOff(st)
	}
	if err := e.Git.Init(ctx); err != nil {
		return "", err
	}
	e.Log.Info("fetch", "branch", e.Cfg.Repo.Branch)
	tip, err := e.Git.Fetch(ctx, e.Cfg.Repo.URL, e.Cfg.Repo.Branch)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	e.Log.Info("fetched", "tip", tip, "applied", st.Applied)
	st.LastTip = tip
	e.expire(st)
	res, err := e.decide(ctx, st, tip)
	if serr := e.Dir.Save(st); serr != nil && err == nil {
		err = serr
	}
	return res, err
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
	if m := st.Marks[tip]; m != nil {
		retry := m.Mark == state.MarkFailed && e.Cfg.OnFailure == config.OnFailureRollback && m.Tries < e.Cfg.Limits.MaxFailedTries
		if !retry {
			e.Log.Info("the tip was handled before; waiting for a new commit", "sha", tip, "mark", m.Mark)
			return Nothing, nil
		}
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
	// Questions per day.
	var recent []time.Time
	for _, t := range st.AskTimes {
		if now.Sub(t) < 24*time.Hour && !t.After(now) {
			recent = append(recent, t)
		}
	}
	st.AskTimes = recent
	if e.Cfg.Mode == config.ModeAsk && len(recent) >= e.Cfg.Limits.QuestionsPerDay {
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
		st.SetMark(tip, state.MarkCheckFailed, now)
		e.notice("⚠ " + code(tip) + " could not be read, so it is not asked: " + message.Esc(err.Error()))
		return CheckFailed, fmt.Errorf("export %s: %w", tip, err)
	}
	p.env.Failed = st.Held
	if argv := e.Cfg.Commands.Check; len(argv) > 0 {
		out, err := e.runSandboxed(ctx, argv, p.env)
		if err != nil {
			st.SetMark(tip, state.MarkCheckFailed, now)
			e.Log.Warn("check failed; not asking", "sha", tip, "err", err)
			e.notice("⚠ The check of " + code(tip) + " failed (" + message.Esc(err.Error()) + "), so it is not asked. It is not checked again.\n" + lastLines(out, 10))
			return CheckFailed, nil
		}
		e.Log.Info("check passed", "sha", tip)
	}
	q := &message.Question{
		Host: e.Cfg.Host, Instance: e.Cfg.Name, SHA: tip, Applied: st.Applied,
		Held: st.Held, NotDescendant: notDesc, Files: p.files, Watch: e.Cfg.Watch.Paths,
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
		Auto: e.Cfg.Mode == config.ModeAuto,
	}
	st.NextID++
	st.Questions = append(st.Questions, nq)
	st.SetMark(tip, state.MarkAsked, now)
	if err := e.Dir.Save(st); err != nil {
		return "", err
	}
	if nq.Auto {
		if err := state.WriteFileAtomic(e.Dir.AutoFile(), []byte(c+"\n"), 0o600); err != nil {
			return "", err
		}
		e.Log.Info("auto mode: apply queued", "sha", tip)
		return AutoQueued, nil
	}
	st.AskTimes = append(st.AskTimes, now)
	if err := e.Spool.Send(spool.Message{Kind: spool.KindAsk, Question: nq.ID, Code: c, Expires: nq.Expires, Text: text}); err != nil {
		return "", err
	}
	e.Log.Info("asked", "sha", tip, "question", nq.ID)
	return Asked, nil
}

// lastLines returns the last n lines of a command's output, cleaned, in a
// <pre> block.
func lastLines(out string, n int) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = message.Esc(l)
	}
	s := strings.Join(lines, "\n")
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "<pre>" + s + "</pre>"
}
