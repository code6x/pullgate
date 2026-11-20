package gate

import (
	"strings"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

// Tests of code6x/pullgate#41: settle, check exit 3, reask_after.

func (f *fixture) status() string {
	f.t.Helper()
	var out strings.Builder
	if err := f.env.Status(&out, StatusFlags{}); err != nil {
		f.t.Fatal(err)
	}
	return out.String()
}

func asks(ms []spool.Message) int {
	n := 0
	for _, m := range ms {
		if m.Kind == spool.KindAsk {
			n++
		}
	}
	return n
}

func TestSettleTwoTipsGiveOneQuestionForTheSecond(t *testing.T) {
	f := newFixture(t, ``, "[limits]\nsettle = \"30m\"\n")
	f.commit("first", "one", nil)
	if r := f.run(); r != Settling {
		t.Fatalf("first run: %s, want settling", r)
	}
	if ms := f.outbox(); len(ms) != 0 {
		t.Fatalf("a settling run sends nothing: %v", ms)
	}
	if f.read("check-count") != "" {
		t.Fatal("a settling tip is not checked yet")
	}
	if s := f.status(); !strings.Contains(s, "settling until 2026-11-11 12:30") {
		t.Fatalf("status must show the settle time:\n%s", s)
	}
	f.now = f.now.Add(10 * time.Minute)
	second := f.commit("second", "two", nil)
	if r := f.run(); r != Settling {
		t.Fatalf("a new tip settles again: %s", r)
	}
	if s := f.status(); !strings.Contains(s, "settling until 2026-11-11 12:40") {
		t.Fatalf("the settle time starts again with the new tip:\n%s", s)
	}
	f.now = f.now.Add(25 * time.Minute) // 35m after the first tip, 25m after the second
	if r := f.run(); r != Settling {
		t.Fatalf("the second tip has not settled: %s", r)
	}
	f.now = f.now.Add(5 * time.Minute)
	if r := f.run(); r != Asked {
		t.Fatalf("the settled tip is asked: %s", r)
	}
	ms := f.outbox()
	if asks(ms) != 1 || !contains(ms, second[:12]) {
		t.Fatalf("one question, for the second tip: %v", ms)
	}
	st := f.state()
	if len(st.AskTimes) != 1 || st.Open() == nil || st.Open().SHA != second {
		t.Fatalf("one question counts against questions_per_day: %+v", st)
	}
	if strings.Contains(f.status(), "settling") {
		t.Fatal("an asked tip is not settling")
	}
}

func TestSettleTipSeenLongerAgoIsAskedAtOnce(t *testing.T) {
	f := newFixture(t, ``, "[limits]\nsettle = \"30m\"\n")
	f.commit("first", "one", nil)
	if r := f.run(); r != Settling {
		t.Fatalf("first run: %s", r)
	}
	f.now = f.now.Add(31 * time.Minute)
	if r := f.run(); r != Asked {
		t.Fatalf("a tip older than settle is asked at once: %s", r)
	}
}

func TestSettleClockBackStartsAgain(t *testing.T) {
	f := newFixture(t, ``, "[limits]\nsettle = \"30m\"\n")
	f.commit("first", "one", nil)
	f.run()
	f.now = f.now.Add(-2 * time.Hour)
	if r := f.run(); r != Settling {
		t.Fatalf("a clock that went back never asks sooner: %s", r)
	}
	f.now = f.now.Add(29 * time.Minute)
	if r := f.run(); r != Settling {
		t.Fatalf("still settling: %s", r)
	}
}

func TestSettleWithoutItAsksAtOnce(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	if r := f.run(); r != Asked {
		t.Fatalf("no settle asks at once: %s", r)
	}
}

func TestSettleInAutoMode(t *testing.T) {
	f := newFixture(t, `mode = "auto"`, "[limits]\nsettle = \"30m\"\n")
	f.commit("first", "one", nil)
	if r := f.run(); r != Settling {
		t.Fatalf("auto waits for the branch to settle too: %s", r)
	}
	f.now = f.now.Add(30 * time.Minute)
	if r := f.run(); r != AutoQueued {
		t.Fatalf("then it is queued: %s", r)
	}
}

func TestCheckExit3IsNothingToApply(t *testing.T) {
	f := newFixture(t, ``, "")
	first := f.applyNew("one")
	docs := f.commit("docs only", "one", map[string]string{"nothing-here": "1"})
	if r := f.run(); r != NothingToDo {
		t.Fatalf("run: %s, want %s", r, NothingToDo)
	}
	if ms := f.outbox(); len(ms) != 0 {
		t.Fatalf("exit 3 sends no message: %v", ms)
	}
	st := f.state()
	if m := st.Marks[docs]; m == nil || m.Mark != state.MarkNothing {
		t.Fatalf("the tip must be marked nothing: %+v", st.Marks[docs])
	}
	if st.Applied != first || st.Open() != nil || len(st.AskTimes) != 1 {
		t.Fatalf("the applied SHA stays and nothing is asked: %+v", st)
	}
	if s := f.status(); !strings.Contains(s, "tip mark:    nothing (the check found nothing to apply on this host)") {
		t.Fatalf("status must show the nothing mark:\n%s", s)
	}
	if r := f.run(); r != Nothing {
		t.Fatalf("a tip marked nothing is not checked again: %s", r)
	}
	if got := strings.Count(f.read("check-count"), "x"); got != 2 {
		t.Fatalf("the check ran %d times, want 2", got)
	}
	// The next real commit lists every commit since the last real apply.
	f.commit("real change", "two", map[string]string{"nothing-here": ""}) // "" removes the file
	if r := f.run(); r != Asked {
		t.Fatalf("next commit: %s", r)
	}
	ms := f.outbox()
	if asks(ms) != 1 || !contains(ms, "docs only") || !contains(ms, "real change") {
		t.Fatalf("the question lists both commits: %v", ms)
	}
	// Exit 1 is still a failed check.
	f.commit("bad", "three", map[string]string{"fail-check": "1"})
	if r := f.run(); r != CheckFailed {
		t.Fatalf("exit 1: %s", r)
	}
	if !contains(f.outbox(), "The check of") {
		t.Fatal("exit 1 sends the failed check notice")
	}
}

func TestCheckExit3InAutoMode(t *testing.T) {
	f := newFixture(t, `mode = "auto"`, "")
	f.commit("docs only", "one", map[string]string{"nothing-here": "1"})
	if r := f.run(); r != NothingToDo {
		t.Fatalf("run: %s", r)
	}
	if f.state().Open() != nil || f.state().Applied != "" {
		t.Fatal("exit 3 queues no apply in auto mode")
	}
}

func TestReaskAfterSkip(t *testing.T) {
	f := newFixture(t, ``, "[limits]\nreask_after = \"24h\"\n")
	sha := f.commit("first", "one", nil)
	f.run()
	f.tap(f.ask().Code, "skip")
	if r := f.apply(); r != Skipped {
		t.Fatalf("skip: %s", r)
	}
	if !contains(f.outbox(), "once more after 24h0m0s") {
		t.Fatal("the skip reply says when the tip is asked again")
	}
	f.now = f.now.Add(23 * time.Hour)
	if r := f.run(); r != Nothing {
		t.Fatalf("before reask_after: %s", r)
	}
	f.now = f.now.Add(time.Hour)
	if r := f.run(); r != Asked {
		t.Fatalf("after reask_after the skipped tip is asked once more: %s", r)
	}
	q := f.ask()
	if r := f.run(); r != Nothing {
		t.Fatalf("asked once: %s", r)
	}
	if asks(f.outbox()) != 0 {
		t.Fatal("no second question while the first is open")
	}
	// A second Skip starts the wait again.
	f.tap(q.Code, "skip")
	if r := f.apply(); r != Skipped {
		t.Fatalf("second skip: %s", r)
	}
	f.outbox()
	f.now = f.now.Add(12 * time.Hour)
	if r := f.run(); r != Nothing {
		t.Fatalf("after a second Skip, not again before the next wait: %s", r)
	}
	f.now = f.now.Add(12 * time.Hour)
	if r := f.run(); r != Asked {
		t.Fatalf("after the next wait: %s", r)
	}
	if st := f.state(); st.Open() == nil || st.Open().SHA != sha {
		t.Fatal("the question is for the same tip")
	}
}

func TestReaskAfterExpiry(t *testing.T) {
	f := newFixture(t, ``, "[limits]\napproval_ttl = \"1h\"\nreask_after = \"24h\"\n")
	f.commit("first", "one", nil)
	f.run()
	f.outbox()
	f.now = f.now.Add(2 * time.Hour)
	if r := f.run(); r != Nothing {
		t.Fatalf("expiry run: %s", r)
	}
	if !contains(f.outbox(), "once more after 24h0m0s") {
		t.Fatal("the expired question says when it is asked again")
	}
	f.now = f.now.Add(24 * time.Hour)
	if r := f.run(); r != Asked {
		t.Fatalf("an expired tip is asked once more: %s", r)
	}
}

func TestReaskCountsAgainstQuestionsPerDay(t *testing.T) {
	f := newFixture(t, ``, "[limits]\nquestions_per_day = 1\nreask_after = \"1h\"\n")
	f.commit("first", "one", nil)
	f.run()
	f.tap(f.ask().Code, "skip")
	f.apply()
	f.outbox()
	f.now = f.now.Add(2 * time.Hour)
	if r := f.run(); r != LimitReached {
		t.Fatalf("a reask counts against questions_per_day: %s", r)
	}
}

func TestWithoutReaskNothingChanges(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	f.run()
	f.tap(f.ask().Code, "skip")
	f.apply()
	ms := f.outbox()
	if !contains(ms, "Skipped") || contains(ms, "reask_after") {
		t.Fatalf("the skip reply is as before: %v", ms)
	}
	f.now = f.now.Add(30 * 24 * time.Hour)
	if r := f.run(); r != Nothing {
		t.Fatalf("without reask_after a skipped tip is not asked again: %s", r)
	}
}

func TestCheckRequestRepliesSettlingAndNothing(t *testing.T) {
	f := newFixture(t, ``, "[limits]\nsettle = \"30m\"\n")
	f.commit("first", "one", map[string]string{"nothing-here": "1"})
	f.request()
	if r := f.run(); r != Settling {
		t.Fatalf("run: %s", r)
	}
	if ms := f.outbox(); !contains(ms, "It is settling") || !contains(ms, "2026-11-11 12:30 UTC") {
		t.Fatalf("the /check reply says the tip settles: %v", ms)
	}
	f.now = f.now.Add(31 * time.Minute)
	f.request()
	if r := f.run(); r != NothingToDo {
		t.Fatalf("run: %s", r)
	}
	if ms := f.outbox(); !contains(ms, "nothing to apply on this host") {
		t.Fatalf("the /check reply says there is nothing to apply: %v", ms)
	}
	f.now = f.now.Add(31 * time.Minute)
	f.request()
	if r := f.run(); r != Nothing {
		t.Fatalf("run: %s", r)
	}
	if ms := f.outbox(); !contains(ms, "the check said that") {
		t.Fatalf("the /check reply names the nothing mark: %v", ms)
	}
}

func TestStatusShowsNoSettleForAHandledTip(t *testing.T) {
	f := newFixture(t, ``, "[limits]\nsettle = \"30m\"\n")
	f.commit("first", "one", map[string]string{"nothing-here": "1"})
	f.run()
	f.now = f.now.Add(31 * time.Minute)
	if r := f.run(); r != NothingToDo {
		t.Fatalf("run: %s", r)
	}
	// A new run of the same tip: no settle shown, the mark is shown.
	if s := f.status(); strings.Contains(s, "settling") || !strings.Contains(s, "nothing (the check") {
		t.Fatalf("status:\n%s", s)
	}
}

func TestReaskNotInAutoMode(t *testing.T) {
	f := newFixture(t, `mode = "auto"`, "[limits]\nreask_after = \"1h\"\n")
	sha := f.commit("first", "one", nil)
	if err := f.env.Dir.Ensure(); err != nil {
		t.Fatal(err)
	}
	st := f.state()
	st.LastTip = sha
	st.SetMark(sha, state.MarkExpired, f.now)
	if err := f.env.Dir.Save(st); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Hour)
	if r := f.run(); r != Nothing {
		t.Fatalf("reask_after does nothing in auto mode: %s", r)
	}
}

func TestCheckExit3InTheBrokenStateIsAFailedCheck(t *testing.T) {
	f := newFixture(t, ``, "")
	first := f.applyNew("one")
	st := f.state()
	st.Broken = &state.Broken{Since: f.now, SHA: first, Reason: "test"}
	if err := f.env.Dir.Save(st); err != nil {
		t.Fatal(err)
	}
	fix := f.commit("fix", "one", map[string]string{"nothing-here": "1"})
	if r := f.run(); r != CheckFailed {
		t.Fatalf("exit 3 in the broken state: %s, want %s", r, CheckFailed)
	}
	if !contains(f.outbox(), "The check of") {
		t.Fatal("the owner gets a notice")
	}
	if m := f.state().Marks[fix]; m == nil || m.Mark != state.MarkCheckFailed {
		t.Fatalf("mark: %+v", m)
	}
}
