package gate

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/recipe"
	"github.com/code6x/pullgate/internal/state"
)

// StatusFlags are the flags of "pullgate status".
type StatusFlags struct {
	AskAgain, Pause, Resume bool
}

// Status prints the state, and does what the flags ask (DESIGN.md
// section 13).
func (e *Env) Status(w io.Writer, f StatusFlags) error {
	if err := e.Dir.Ensure(); err != nil {
		return err
	}
	if f.Pause {
		// The pause file needs no lock, so it works while an apply runs.
		if err := e.Dir.Pause("paused by the owner"); err != nil {
			return err
		}
	}
	if f.Resume || f.AskAgain {
		lock, err := e.Dir.Lock(true)
		if err != nil {
			return err
		}
		defer func() { _ = lock.Close() }()
	}
	st, err := e.Dir.Load()
	if err != nil {
		return err
	}
	changed := false
	var closed *state.Question
	if f.Resume {
		if err := e.Dir.Resume(); err != nil {
			return err
		}
		if st.InProgress != nil {
			st.InProgress = nil
			changed = true
		}
		if st.Broken != nil {
			// Root checked the host: the broken state ends here.
			st.Broken = nil
			changed = true
			_, _ = fmt.Fprintln(w, "The broken state is ended. A new commit, or --ask-again, asks again.")
		}
	}
	if f.AskAgain && st.LastTip != "" {
		delete(st.Marks, st.LastTip)
		if st.Held == st.LastTip {
			st.Held = ""
		}
		closed = forgetOpen(st, st.LastTip)
		changed = true
	}
	if changed {
		if err := e.Dir.Save(st); err != nil {
			return err
		}
	}
	if closed != nil && !closed.Auto {
		// After the save: the listener forgets the code only when root
		// has forgotten it too.
		e.edit(closed.ID, "This question for "+code(closed.SHA)+" is closed: the owner ran --ask-again. The next run asks again.")
		_, _ = fmt.Fprintf(w, "The open question %d for %s is closed. The next run asks again.\n", closed.ID, closed.SHA)
	}
	e.statusText(w, st)
	return nil
}

// statusText writes the facts of the state, for "pullgate status" and
// /status.
func (e *Env) statusText(w io.Writer, st *state.State) {
	show := func(k, v string) { _, _ = fmt.Fprintf(w, "%-12s %s\n", k+":", v) }
	show("instance", e.Cfg.Name)
	show("mode", e.Cfg.Mode)
	show("on_failure", e.Cfg.OnFailure)
	if e.Cfg.Path != "" {
		p := recipe.RecordPath(filepath.Dir(e.Cfg.Path), e.Cfg.Name)
		if r, err := recipe.ReadInstanceRecord(p); err == nil {
			show("recipe", fmt.Sprintf("%s v%s (%s, instance %s, in %s)", r.Repo, r.Version, r.File, r.Instance, r.Dir))
		} else if !errors.Is(err, fs.ErrNotExist) {
			show("recipe", fmt.Sprintf("the record %s cannot be read: %v", p, err))
		}
	}
	show("applied", orNone(st.Applied))
	show("last tip", orNone(st.LastTip))
	if q := st.Open(); q != nil {
		show("question", fmt.Sprintf("%s, open until %s", q.SHA, q.Expires.Format("2006-01-02 15:04 MST")))
		if !q.Auto {
			show("delivery", e.delivery(q, e.now()))
		}
	} else {
		show("question", "none")
	}
	if st.Held != "" {
		show("held", st.Held+" (the last apply failed)")
	}
	m := st.Marks[st.LastTip]
	switch {
	case st.LastTip == "":
	case m != nil && m.Mark == state.MarkNothing:
		show("tip mark", "nothing (the check found nothing to apply on this host)")
	case m != nil:
		show("tip mark", fmt.Sprintf("%s (tries %d)", m.Mark, m.Tries))
	}
	if e.settleShown(st, m) {
		if until, ok := e.settling(st, e.now()); ok && !st.TipSince.IsZero() {
			show("settle", "settling until "+until.Format("2006-01-02 15:04 MST"))
		}
	}
	if b := st.Broken; b != nil {
		show("broken", fmt.Sprintf("since %s, last error: %s", b.Since.Format("2006-01-02 15:04 MST"), b.Reason))
	}
	if e.Dir.Paused() {
		show("paused", "yes: "+e.Dir.PauseReason()+" ("+e.Dir.PauseFile()+")")
	} else {
		show("paused", "no")
	}
	if st.InProgress != nil {
		show("in progress", fmt.Sprintf("%s of %s since %s", st.InProgress.Phase, st.InProgress.SHA, st.InProgress.Started.Format("2006-01-02 15:04 MST")))
	}
}

// forgetOpen removes the open question for sha from the state and returns
// it, so that the next run asks again with a new question and a new code.
// Its message may never have reached the owner (a lost outbox file).
// Unlike a superseded question, it does not stay in the list: an approval
// for its code, even one written before this, finds no question and is
// ignored (fail closed).
func forgetOpen(st *state.State, sha string) *state.Question {
	for i := range st.Questions {
		if q := st.Questions[i]; q.Open && q.SHA == sha {
			st.Remove(i)
			return &q
		}
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// settleShown says whether the last tip waits for settle: a tip that a
// run would check and ask, but for the settle time.
func (e *Env) settleShown(st *state.State, m *state.Mark) bool {
	tip, open := st.LastTip, st.Open()
	switch {
	case tip == "" || tip == st.Applied || tip == st.Held || m != nil || e.Cfg.Mode == config.ModeOff:
		return false
	case open != nil && open.SHA == tip:
		return false
	case st.Broken != nil && (tip == st.Broken.SHA || tip == st.Broken.OldSHA):
		return false
	}
	return true
}
