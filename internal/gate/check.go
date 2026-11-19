package gate

import (
	"strings"
	"time"

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
		e.Log.Info("requests taken", "check", r.Check, "status", r.Status, "pause", r.Pause)
	}
	return r
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
		text = "⏸ " + e.Cfg.Name + " is paused. Nothing is fetched, asked or applied. To go on, run as root: <code>pullgate status " + e.Cfg.Name + " --resume</code>"
	}
	if d := now.Sub(st.PauseNote); d >= CheckInterval || d <= -CheckInterval {
		st.PauseNote = now
		e.notice(text)
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
		case st.Marks[tip] != nil && st.Marks[tip].Mark == state.MarkRefused:
			e.notice("No new question: " + code(tip) + " is not a descendant of the applied commit " + code(st.Applied) + ", so it was refused. To allow it, set <code>allow_older = true</code> in [limits] of the host config.")
		default:
			mark := ""
			if m := st.Marks[tip]; m != nil {
				mark = " (" + m.Mark + ")"
			}
			e.notice("No new question: " + code(tip) + " was handled before" + mark + ". To ask again, run as root: <code>pullgate status " + e.Cfg.Name + " --ask-again</code>")
		}
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
