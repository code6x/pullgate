package gate

import (
	"fmt"
	"io"

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
	show := func(k, v string) { _, _ = fmt.Fprintf(w, "%-12s %s\n", k+":", v) }
	show("instance", e.Cfg.Name)
	show("mode", e.Cfg.Mode)
	show("on_failure", e.Cfg.OnFailure)
	show("applied", orNone(st.Applied))
	show("last tip", orNone(st.LastTip))
	if q := st.Open(); q != nil {
		show("question", fmt.Sprintf("%s, open until %s", q.SHA, q.Expires.Format("2006-01-02 15:04 MST")))
	} else {
		show("question", "none")
	}
	if st.Held != "" {
		show("held", st.Held+" (the last apply failed)")
	}
	if m := st.Marks[st.LastTip]; m != nil && st.LastTip != "" {
		show("tip mark", fmt.Sprintf("%s (tries %d)", m.Mark, m.Tries))
	}
	if b := st.Broken; b != nil {
		show("broken", fmt.Sprintf("since %s, last error: %s", b.Since.Format("2006-01-02 15:04 MST"), b.Reason))
	}
	if e.Dir.Paused() {
		show("paused", "yes: "+e.Dir.PauseFile())
	} else {
		show("paused", "no")
	}
	if st.InProgress != nil {
		show("in progress", fmt.Sprintf("%s of %s since %s", st.InProgress.Phase, st.InProgress.SHA, st.InProgress.Started.Format("2006-01-02 15:04 MST")))
	}
	return nil
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
