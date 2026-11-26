package gate

import (
	"strings"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/message"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

// breakIt applies "one", then fails the apply of "bad" and its rollback.
// It returns the applied and the failed SHA.
func breakIt(t *testing.T, f *fixture) (string, string) {
	t.Helper()
	a := f.applyNew("one")
	b := f.commit("bad", "two", map[string]string{"fail-health": "1"})
	f.run()
	f.tap(f.ask().Code, "apply")
	f.touch("fail-rollback")
	if r := f.apply(); r != RollbackError {
		t.Fatalf("apply: %s", r)
	}
	return a, b
}

func TestFailedRollbackIsBroken(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "[limits]\nmax_failed_tries = 3\n")
	a, b := breakIt(t, f)
	st := f.state()
	if st.Broken == nil || st.Broken.SHA != b || st.Broken.OldSHA != a || st.InProgress != nil || st.Applied != a {
		t.Fatalf("state after a failed rollback: %+v", st)
	}
	if f.env.Dir.Paused() {
		t.Fatal("broken is not a pause: no pause file")
	}
	out := f.outbox()
	if len(out) != 1 || !strings.Contains(out[0].Text, "The instance is now broken") ||
		!strings.Contains(out[0].Text, "Two ways out") || !strings.Contains(out[0].Text, "exit status 3") {
		t.Fatalf("one message with the reason and the ways out, got %+v", out)
	}
	// The failed commit is not asked again (even with tries left), and
	// neither is the old one.
	if r := f.run(); r != Nothing {
		t.Fatalf("run on the failed tip: %s", r)
	}
	f.r.Push(a)
	if r := f.run(); r != Nothing {
		t.Fatalf("run on the old commit: %s", r)
	}
	if len(f.outbox()) != 0 {
		t.Fatal("no message for the failed or the old commit")
	}
	// The notice went out once: later runs send nothing.
	f.r.Push(b)
	f.run()
	if len(f.outbox()) != 0 {
		t.Fatal("the broken notice goes out once")
	}
	var w strings.Builder
	if err := f.env.Status(&w, StatusFlags{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.String(), "broken:      since 2026-11-11 12:00 UTC, last error: the apply of") {
		t.Fatalf("status:\n%s", w.String())
	}
}

func TestBrokenFixForward(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "")
	_, b := breakIt(t, f)
	f.outbox()
	f.r.Git("rm", "--quiet", "fail-health")
	c := f.commit("fix", "three", nil)
	if r := f.run(); r != Sent {
		t.Fatalf("a fix forward is asked: %s", r)
	}
	q := f.ask()
	if !strings.Contains(q.Text, "This commit is a fix forward.\nReason: <code>the apply of "+message.Short(b)) ||
		!strings.Contains(q.Text, "This commit is a fix forward.") {
		t.Fatalf("the question has no warning:\n%s", q.Text)
	}
	f.tap(q.Code, "apply")
	if r := f.apply(); r != Applied {
		t.Fatalf("apply: %s", r)
	}
	st := f.state()
	if st.Broken != nil || st.Applied != c {
		t.Fatalf("a good fix forward ends the broken state: %+v", st)
	}
	if !contains(f.outbox(), "no longer broken") {
		t.Fatal("no report of the end")
	}
	// PULLGATE_FAILED_SHA is the failed commit during the fix forward.
	if !strings.Contains(f.read("log"), "apply "+c+" old=") || !strings.Contains(f.read("log"), "failed="+b) {
		t.Fatalf("env of the fix forward:\n%s", f.read("log"))
	}
}

func TestBrokenBadFixForwardStaysBroken(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "[limits]\nmax_failed_tries = 3\n")
	a, _ := breakIt(t, f)
	f.outbox()
	c := f.commit("still bad", "three", nil) // fail-health is still there
	f.run()
	f.tap(f.ask().Code, "apply")
	before := strings.Count(f.read("log"), "rb=1")
	if r := f.apply(); r != StillBroken {
		t.Fatalf("apply: %s", r)
	}
	if strings.Count(f.read("log"), "rb=1") != before {
		t.Fatal("no rollback is tried in the broken state")
	}
	st := f.state()
	if st.Broken == nil || st.Broken.SHA != c || st.Applied != a || !strings.Contains(st.Broken.Reason, "fix forward") {
		t.Fatalf("a bad fix forward stays broken: %+v", st.Broken)
	}
	if !contains(f.outbox(), "The instance stays broken") {
		t.Fatal("no report")
	}
	if r := f.run(); r != Nothing {
		t.Fatalf("the failed fix forward is not asked again: %s", r)
	}
	d := f.commit("next", "four", nil)
	if r := f.run(); r != Sent {
		t.Fatalf("the next newer commit is asked: %s", r)
	}
	if q := f.ask(); !strings.Contains(q.Text, "fix forward "+message.Short(c)+" failed") {
		t.Fatalf("the warning has the last reason:\n%s", q.Text)
	}
	_ = d
}

func TestBrokenRefusesNonDescendant(t *testing.T) {
	// Even with allow_older, only a commit on top of the failed one.
	f := newFixture(t, `on_failure = "rollback"`, "[limits]\nallow_older = true\n")
	a, _ := breakIt(t, f)
	f.outbox()
	f.r.Push(a)
	f.r.Git("reset", "--quiet", "--hard", a)
	f.commit("other", "five", nil)
	if r := f.run(); r != Refused {
		t.Fatalf("run: %s", r)
	}
	if !contains(f.outbox(), "only a fix forward is asked") {
		t.Fatal("no notice")
	}
}

func TestBrokenAsksInAutoMode(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "")
	_, _ = breakIt(t, f)
	f.outbox()
	f.env.Cfg.Mode = config.ModeAuto
	f.r.Git("rm", "--quiet", "fail-health")
	f.commit("fix", "three", nil)
	if r := f.run(); r != Sent {
		t.Fatalf("broken in auto mode asks the owner: %s", r)
	}
	if m := f.ask(); m.Kind != spool.KindAsk {
		t.Fatal("no question")
	}
}

func TestRootResumeEndsBroken(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "")
	breakIt(t, f)
	var w strings.Builder
	if err := f.env.Status(&w, StatusFlags{Resume: true}); err != nil {
		t.Fatal(err)
	}
	if f.state().Broken != nil || !strings.Contains(w.String(), "The broken state is ended") {
		t.Fatalf("root --resume ends the broken state:\n%s", w.String())
	}
}

func TestBrokenDropsOlderQuestions(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "")
	f.applyNew("one")
	f.commit("bad", "two", map[string]string{"fail-health": "1"})
	f.run()
	q1 := f.ask()
	f.commit("next", "three", nil) // fail-health is still in the tree
	f.run()
	q2 := f.ask()
	f.tap(q1.Code, "apply")
	f.touch("fail-rollback")
	if r := f.apply(); r != RollbackError {
		t.Fatalf("apply: %s", r)
	}
	if st := f.state(); len(st.Questions) != 0 {
		t.Fatalf("the questions from before are dropped: %+v", st.Questions)
	}
	if !contains(f.outbox(), "is closed: the instance is broken") {
		t.Fatal("the open question is not closed in the chat")
	}
	// The tap on the question without the warning does nothing.
	f.tap(q2.Code, "apply")
	if r := f.apply(); r != Ignored {
		t.Fatalf("an old tap while broken: %s", r)
	}
	// The next run asks it again, with the warning.
	if r := f.run(); r != Sent {
		t.Fatalf("run: %s", r)
	}
	if !strings.Contains(f.ask().Text, "This commit is a fix forward.") {
		t.Fatal("no warning")
	}
	// A pause file still stops everything while broken.
	if err := f.env.Dir.Pause("test"); err != nil {
		t.Fatal(err)
	}
	f.commit("more", "four", nil)
	if r := f.run(); r != Paused {
		t.Fatalf("paused and broken: %s", r)
	}
}

func TestBrokenTapRefusesNonFixForward(t *testing.T) {
	for _, which := range []string{"failed", "other", "auto"} {
		t.Run(which, func(t *testing.T) {
			f := newFixture(t, `on_failure = "rollback"`, "[limits]\nallow_older = true\n")
			a, b := breakIt(t, f)
			f.outbox()
			sha, auto := b, false
			switch which {
			case "other":
				f.r.Git("reset", "--quiet", "--hard", a)
				sha = f.commit("other", "five", nil)
				f.r.Push(sha)
			case "auto":
				f.r.Git("rm", "--quiet", "fail-health")
				sha, auto = f.commit("fix", "three", nil), true
			}
			f.run() // fetch it
			f.outbox()
			// A question that root's state holds, as if asked before.
			c, err := state.NewCode()
			if err != nil {
				t.Fatal(err)
			}
			st := f.state()
			st.Questions = append(st.Questions, state.Question{ID: 99, Code: c, SHA: sha, Base: a,
				Asked: f.now, Expires: f.now.Add(time.Hour), Open: true, Auto: auto})
			if err := f.env.Dir.Save(st); err != nil {
				t.Fatal(err)
			}
			f.tap(c, "apply")
			if r := f.apply(); r != Refused {
				t.Fatalf("apply: %s", r)
			}
			if st := f.state(); st.Applied != a || st.Broken == nil {
				t.Fatalf("state: %+v", st)
			}
		})
	}
}
