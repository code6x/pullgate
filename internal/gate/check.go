package gate

import (
	"strings"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/message"
	"github.com/code6x/pullgate/internal/spool"

	"github.com/code6x/pullgate/internal/state"
)

// CheckInterval is the least time between two runs that check requests
// start (DESIGN.md section 8). Root keeps the time in its own state, so a
// listener that writes many requests cannot make the host fetch more often.
const CheckInterval = time.Minute

// takeChecks reads and removes the files in the check directory, and
// returns the valid requests. A bad file is logged and removed. An error
// of the directory counts as no request (fail closed).
func (e *Env) takeChecks() spool.Requests {
	r, bad, err := e.Spool.TakeRequests(e.Owners.ListenerUID)
	for _, b := range bad {
		e.Log.Warn("bad check request, removed", "err", b)
	}
	if err != nil {
		e.Log.Error("read the check requests", "err", err)
	}
	if r.Any() {
		e.Log.Info("requests taken", "check", r.Check, "status", r.Status, "pause", r.Pause, "resume", r.Resume, "ask-again", r.AskAgain)
	}
	return r
}

// resumeHint says how to go on after a pause: /resume for a /pause, else
// root.
func (e *Env) resumeHint() string {
	if e.Dir.TelegramPaused(e.UID) {
		return "Send <code>/resume " + e.Cfg.Name + "</code>"
	}
	return "Run as root: <code>pullgate status " + e.Cfg.Name + " --resume</code>"
}

// pauseRequest acts on /pause: it writes the pause file, unless there is
// one. A pause that is there already is never changed, so that a later
// /resume (code6x/pullgate#33) cannot lift it. The pause itself is never
// rate-limited; its reply is, once a minute.
func (e *Env) pauseRequest(st *state.State) {
	now := e.now()
	var text string
	wrote, err := e.Dir.PauseIfNone(state.TelegramPause + now.UTC().Format("2006-01-02 15:04 MST"))
	switch {
	case err != nil:
		e.Log.Error("write the pause file", "err", err)
		text = "Error: " + e.Cfg.Name + " could not be paused. See the journal of the host."
	case !wrote:
		text = "⏸ " + e.Cfg.Name + " was paused already: " + message.Code(e.Dir.PauseReason())
	default:
		e.Log.Info("paused from Telegram")
		text = "⏸ " + e.Cfg.Name + " is paused. Nothing is fetched, asked or applied. To go on, send <code>/resume " + e.Cfg.Name + "</code>."
	}
	if d := now.Sub(st.PauseNote); d >= CheckInterval || d <= -CheckInterval {
		st.PauseNote = now
		e.notice(text)
	}
}

// resumeRequest acts on /resume: it lifts only a pause that /pause wrote
// (code6x/pullgate#33). A pause by pullgate itself (a cut-off apply) or by
// root stays: a compromised listener must not undo a safety stop.
func (e *Env) resumeRequest(st *state.State) {
	var text string
	switch {
	case !e.Dir.Paused():
		text = e.Cfg.Name + " is not paused."
	default:
		lifted, err := e.Dir.ResumeTelegram(e.UID)
		if err != nil {
			e.Log.Error("remove the pause file", "err", err)
			text = "Error: " + e.Cfg.Name + " could not be resumed. See the journal of the host."
			break
		}
		if !lifted {
			text = "⏸ " + e.Cfg.Name + " stays paused: this pause was not set from Telegram (" + message.Code(e.Dir.PauseReason()) + "), so /resume does not lift it. Check the host, then run as root: <code>pullgate status " + e.Cfg.Name + " --resume</code>"
			break
		}
		e.Log.Info("resumed from Telegram")
		text = "▶ " + e.Cfg.Name + " goes on."
		if st.Broken != nil {
			text += " It is still broken: only a fix forward is asked."
		}
	}
	e.notice(text)
}

// askAgainRequest acts on /ask-again: it asks again only about a question
// that expired or was lost (an open one), never about a commit that was
// skipped, failed, held, refused or failed its check (code6x/pullgate#33).
func (e *Env) askAgainRequest(st *state.State) {
	tip := st.LastTip
	m := st.Marks[tip]
	switch {
	case e.Dir.Paused() || e.Cfg.Mode == config.ModeOff:
		e.notice("Not asked again: " + e.Cfg.Name + " is paused or off. Resume it first.")
	case tip == "" || tip == st.Applied:
		e.notice("Nothing to ask again: there is no new commit.")
	case st.Broken != nil && (tip == st.Broken.SHA || tip == st.Broken.OldSHA):
		e.notice("Not asked again: " + code(tip) + " is the failed commit of the broken state. Push a fix forward, or check the host and run as root: <code>pullgate status " + e.Cfg.Name + " --resume</code>")
	case tip == st.Held:
		e.notice("Not asked again: the apply of " + code(tip) + " failed and it is held. To try it again, run as root: <code>pullgate status " + e.Cfg.Name + " --ask-again</code>")
	case m != nil && m.Mark == state.MarkAsked:
		q := forgetOpen(st, tip)
		delete(st.Marks, tip)
		// Save first: the listener forgets the code only when root has.
		if err := e.Dir.Save(st); err != nil {
			e.Log.Error("save the state", "err", err)
			e.notice("Error: the question could not be closed. See the journal of the host.")
			return
		}
		if q != nil && !q.Auto {
			e.edit(q.ID, "This question for "+code(q.SHA)+" is closed: the owner sent /ask-again. The next question has a new code.")
		}
		e.Log.Info("ask again from Telegram: open question", "sha", tip)
		e.notice("Asking again about " + code(tip) + ".")
	case m != nil && m.Mark == state.MarkExpired:
		delete(st.Marks, tip)
		e.Log.Info("ask again from Telegram: expired question", "sha", tip)
		e.notice("Asking again about " + code(tip) + ".")
	case m == nil:
		e.notice("Nothing to ask again: " + code(tip) + " has no question that expired or was lost.")
	default:
		hint := ""
		if m.Mark == state.MarkSkipped && e.reaskOn() {
			hint = " It is asked once more " + e.Cfg.Limits.ReaskAfter.String() + " after the Skip (reask_after)."
		}
		e.notice("Not asked again: " + code(tip) + " was " + m.Mark + ". /ask-again asks only about a question that expired or was lost. To ask anyway, run as root: <code>pullgate status " + e.Cfg.Name + " --ask-again</code>" + hint)
	}
}

// statusRequest answers /status with the facts of "pullgate status",
// at most once a minute.
func (e *Env) statusRequest(st *state.State) {
	now := e.now()
	if d := now.Sub(st.StatusAt); d < CheckInterval && d > -CheckInterval {
		e.Log.Warn("status request dropped: the last one was less than a minute ago")
		return
	}
	st.StatusAt = now
	var b strings.Builder
	e.statusText(&b, st)
	e.notice("Status:\n" + message.Output(b.String(), 30))
}

// limitCheck enforces CheckInterval. It returns CheckLimited when the
// request must be dropped; else it records the time of this request. The
// drop gets one reply in each interval.
func (e *Env) limitCheck(st *state.State) Result {
	now := e.now()
	since := now.Sub(st.CheckAt)
	if since < CheckInterval && since > -CheckInterval {
		e.Log.Warn("check request dropped: the last one was less than a minute ago")
		if d := now.Sub(st.CheckNote); d >= CheckInterval || d <= -CheckInterval {
			st.CheckNote = now
			e.notice("A check ran less than a minute ago, so this /check was dropped. Try again in a minute.")
		}
		return CheckLimited
	}
	st.CheckAt = now
	return ""
}

// checkReply tells the owner what a run that a check request started found,
// when no other message says it already. limitSent is true when the run
// sent the questions_per_day notice.
func (e *Env) checkReply(st *state.State, res Result, err error, limitSent bool) {
	if err != nil {
		e.notice("The check failed. See the journal of the host: <code>journalctl -u pullgate-run@" + e.Cfg.Name + ".service</code>")
		return
	}
	tip := st.LastTip
	switch res {
	case Nothing:
		switch open := st.Open(); {
		case tip == st.Applied:
			e.notice("No new commit. The applied commit is " + code(tip) + ".")
		case open != nil && open.SHA == tip && !open.Auto:
			e.notice("No new commit. The question for " + code(tip) + " is still open.")
		case open != nil && open.SHA == tip:
			e.notice("No new commit. " + code(tip) + " is waiting for its auto apply.")
		case tip == st.Held:
			e.notice("No new question: the last apply of " + code(tip) + " failed and it is held. A newer commit is asked.")
		case st.Marks[tip] != nil && st.Marks[tip].Mark == state.MarkNothing:
			e.notice("No new question: the check said that " + code(tip) + " has nothing to apply on this host. The applied commit is still " + code(st.Applied) + ".")
		case st.Marks[tip] != nil && st.Marks[tip].Mark == state.MarkRefused:
			e.notice("No new question: " + code(tip) + " is not a descendant of the applied commit " + code(st.Applied) + ", so it was refused. To allow it, set <code>allow_older = true</code> in [limits] of the host config.")
		default:
			mark := ""
			if m := st.Marks[tip]; m != nil {
				mark = " (" + m.Mark + ")"
			}
			hint := ""
			if m := st.Marks[tip]; m != nil && (m.Mark == state.MarkSkipped || m.Mark == state.MarkExpired) && e.reaskOn() {
				hint = " It is asked once more " + e.Cfg.Limits.ReaskAfter.String() + " after the mark (reask_after)."
			}
			e.notice("No new question: " + code(tip) + " was handled before" + mark + ". To ask again, run as root: <code>pullgate status " + e.Cfg.Name + " --ask-again</code>" + hint)
		}
	case Settling:
		until, _ := e.settling(st, e.now())
		e.notice("New commit " + code(tip) + ". It is settling: it is asked when the branch has not changed until " + until.UTC().Format("2006-01-02 15:04 MST") + " ([limits] settle).")
	case NothingToDo:
		e.notice("New commit " + code(tip) + ". The check says it has nothing to apply on this host, so nothing is asked. The applied commit is still " + code(st.Applied) + ".")
	case AutoQueued:
		e.notice("New commit " + code(tip) + ". The mode is auto (mode = \"auto\"), so it is applied now, without a tap.")
	case LimitReached:
		if !limitSent {
			e.notice("No new question for " + code(tip) + ": the limit questions_per_day is reached.")
		}
	case Asked, Refused, CheckFailed:
		// The question or a notice says it already.
	}
}
