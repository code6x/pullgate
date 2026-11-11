package gate

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

func TestFirstRunAsksWithAllFilesThenApplies(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "")
	sha := f.commit("first", "one", map[string]string{"policy/main.toml": "x = 1\n"})
	if r := f.run(); r != Asked {
		t.Fatalf("run: %s", r)
	}
	q := f.ask()
	for _, want := range []string{"<code>" + sha + "</code>", "First apply: there is no applied commit yet. All 2 files are listed.",
		"⚠ Watched path changed: <code>policy/main.toml</code>", "value one", "example-host", "config-repo"} {
		if !strings.Contains(q.Text, want) {
			t.Errorf("the question misses %q:\n%s", want, q.Text)
		}
	}
	if strings.Contains(q.Text, sha[:0]+"old value") {
		t.Error("no old tree on the first run")
	}
	if len(q.Code) != 22 {
		t.Fatalf("code %q", q.Code)
	}
	f.tap(q.Code, "apply")
	if r := f.apply(); r != Applied {
		t.Fatalf("apply: %s", r)
	}
	if f.read("live") != "one" || f.state().Applied != sha {
		t.Fatalf("not applied: live %q, state %q", f.read("live"), f.state().Applied)
	}
	if !contains(f.outbox(), "✅ Applied <code>"+sha) {
		t.Error("no report")
	}
	// The next run has nothing to do.
	if r := f.run(); r != Nothing {
		t.Fatalf("run after apply: %s", r)
	}
}

func TestSecondCommitGetsOldTreeAndForwardOnly(t *testing.T) {
	f := newFixture(t, ``, "")
	a := f.applyNew("one")
	b := f.commit("second", "two", nil)
	if r := f.run(); r != Asked {
		t.Fatalf("run: %s", r)
	}
	q := f.ask()
	if !strings.Contains(q.Text, "Since <code>"+a+"</code>") || !strings.Contains(q.Text, "old value one") || !strings.Contains(q.Text, "• second") {
		t.Fatalf("the question must show the old tree and the commits:\n%s", q.Text)
	}
	f.tap(q.Code, "apply")
	if r := f.apply(); r != Applied || f.read("live") != "two" {
		t.Fatalf("apply: %s, live %q", r, f.read("live"))
	}
	if !strings.Contains(f.read("log"), "apply "+b+" old="+a) {
		t.Fatalf("PULLGATE_OLD_SHA: %s", f.read("log"))
	}
	if f.read("changed") != "value" {
		t.Fatalf("PULLGATE_CHANGED_FILES: %q", f.read("changed"))
	}
	// An older SHA is refused: push a back.
	f.outbox()
	f.r.Push(a)
	if r := f.run(); r != Refused {
		t.Fatalf("an older commit must be refused, got %s", r)
	}
	ms := f.outbox()
	if !contains(ms, "Refused <code>"+a) || contains(ms, "Apply this commit?") {
		t.Fatal("a refusal sends a notice and no question")
	}
	// It is not offered again.
	if r := f.run(); r != Nothing || len(f.outbox()) != 0 {
		t.Fatalf("an old commit is not offered again: %s", r)
	}
}

func TestForcePushRefusedUnlessAllowOlder(t *testing.T) {
	f := newFixture(t, ``, "")
	f.applyNew("one")
	f.r.Git("checkout", "--quiet", "--orphan", "other")
	f.commit("rewritten", "evil", nil)
	if r := f.run(); r != Refused {
		t.Fatalf("a rewritten branch must be refused, got %s", r)
	}
	f.outbox()

	g := newFixture(t, ``, "[limits]\nallow_older = true\n")
	g.applyNew("one")
	g.r.Git("checkout", "--quiet", "--orphan", "other")
	other := g.commit("rewritten", "two", nil)
	if r := g.run(); r != Asked {
		t.Fatalf("with allow_older it is asked, got %s", r)
	}
	q := g.ask()
	if !strings.Contains(q.Text, "⚠ This commit is not a descendant of the applied commit.") {
		t.Fatalf("the warning is missing:\n%s", q.Text)
	}
	g.tap(q.Code, "apply")
	if r := g.apply(); r != Applied || g.state().Applied != other {
		t.Fatalf("apply: %s", r)
	}
}

func TestRootTakesSHAFromItsOwnState(t *testing.T) {
	f := newFixture(t, ``, "")
	a := f.commit("first", "one", nil)
	f.run()
	q := f.ask()
	// A newer commit after the question does not change what is applied.
	f.commit("second", "two", nil)
	f.tap(q.Code, "apply")
	if r := f.apply(); r != Applied {
		t.Fatalf("apply: %s", r)
	}
	if f.state().Applied != a || f.read("live") != "one" {
		t.Fatalf("applied %s (live %q), want the asked SHA %s", f.state().Applied, f.read("live"), a)
	}
}

func TestTapOnSupersededQuestionAppliesOldSHA(t *testing.T) {
	f := newFixture(t, ``, "")
	a := f.commit("first", "one", nil)
	f.run()
	q1 := f.ask()
	b := f.commit("second", "two", nil)
	if r := f.run(); r != Asked {
		t.Fatalf("run: %s", r)
	}
	ms := f.outbox()
	if !contains(ms, "a newer commit replaced this question") {
		t.Fatal("the old question must be closed")
	}
	// The listener took the tap on q1 before it handled the close.
	f.tap(q1.Code, "apply")
	if r := f.apply(); r != Applied || f.state().Applied != a {
		t.Fatalf("the old SHA must be applied (rule 2): %s %s", r, f.state().Applied)
	}
	// The question for b had base "" and is closed, so the next run asks
	// again with a right summary.
	if !contains(f.outbox(), "the applied commit changed") {
		t.Fatal("the stale question must be closed")
	}
	if r := f.run(); r != Asked {
		t.Fatalf("run: %s", r)
	}
	q := f.ask()
	if !strings.Contains(q.Text, "<code>"+b+"</code>") || !strings.Contains(q.Text, "Since <code>"+a) {
		t.Fatalf("the new question is wrong:\n%s", q.Text)
	}
}

func TestUnknownCodeAndReplayAreIgnored(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	f.run()
	q := f.ask()
	f.tap("AAAAAAAAAAAAAAAAAAAAAA", "apply")
	if r := f.apply(); r != Ignored || f.state().Applied != "" {
		t.Fatalf("an unknown code must be ignored: %s", r)
	}
	f.tap(q.Code, "apply")
	f.apply()
	f.commit("second", "two", nil)
	// A replay of the used code does nothing.
	f.tap(q.Code, "apply")
	if r := f.apply(); r != Ignored || f.read("live") != "one" {
		t.Fatalf("a replayed code must be ignored: %s", r)
	}
}

func TestBadApprovalFilesAreRemoved(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	f.run()
	q := f.ask()
	in := f.env.Spool.Inbox()
	sha := f.state().Open().SHA
	// An approval that names a SHA has an unknown field: refused.
	bad := `{"code":"` + q.Code + `","action":"apply","sha":"` + sha + `"}`
	if err := os.WriteFile(filepath.Join(in, "1.json"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "2.json"), []byte(strings.Repeat("x", 5000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.base, "elsewhere"), filepath.Join(in, "3.json")); err != nil {
		t.Fatal(err)
	}
	if r := f.apply(); r != Nothing || f.state().Applied != "" {
		t.Fatalf("bad approvals must apply nothing: %s", r)
	}
	if left, _ := os.ReadDir(in); len(left) != 0 {
		t.Fatalf("bad files must be removed: %v", left)
	}
	if f.state().Open() == nil {
		t.Fatal("the question must stay open")
	}
}

func TestSkip(t *testing.T) {
	f := newFixture(t, ``, "")
	a := f.commit("first", "one", nil)
	f.run()
	f.tap(f.ask().Code, "skip")
	if r := f.apply(); r != Skipped || f.state().Applied != "" {
		t.Fatalf("skip: %s", r)
	}
	if r := f.run(); r != Nothing {
		t.Fatalf("a skipped SHA is not asked again: %s", r)
	}
	f.outbox()
	if err := f.env.Status(new(strings.Builder), StatusFlags{AskAgain: true}); err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r != Asked || f.ask().Code == "" {
		t.Fatalf("--ask-again asks again: %s", r)
	}
	_ = a
}

func TestExpiredApprovalIsIgnored(t *testing.T) {
	f := newFixture(t, ``, "[limits]\napproval_ttl = \"1h\"\n")
	f.commit("first", "one", nil)
	f.run()
	q := f.ask()
	f.tap(q.Code, "apply")
	f.now = f.now.Add(61 * time.Minute)
	if r := f.apply(); r != Ignored || f.state().Applied != "" {
		t.Fatalf("an expired approval must be ignored: %s", r)
	}
	if !contains(f.outbox(), "outside the time limit") {
		t.Error("no report")
	}
	// An unanswered question that expired is not asked again.
	if r := f.run(); r != Nothing {
		t.Fatalf("run: %s", r)
	}
}

func TestClockBeforeAskedIsIgnored(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	f.run()
	f.tap(f.ask().Code, "apply")
	f.now = f.now.Add(-time.Hour) // the clock went back
	if r := f.apply(); r != Ignored || f.state().Applied != "" {
		t.Fatalf("a clock before the question must deny: %s", r)
	}
}

func TestExpiredQuestionIsClosedByRun(t *testing.T) {
	f := newFixture(t, ``, "[limits]\napproval_ttl = \"1h\"\n")
	f.commit("first", "one", nil)
	f.run()
	f.outbox()
	f.now = f.now.Add(2 * time.Hour)
	if r := f.run(); r != Nothing {
		t.Fatalf("run: %s", r)
	}
	if !contains(f.outbox(), "expired") || f.state().Open() != nil {
		t.Fatal("the expired question must be closed")
	}
	f.commit("second", "two", nil)
	if r := f.run(); r != Asked {
		t.Fatalf("a new commit asks again: %s", r)
	}
}

func TestFailingCheckSendsNoQuestionAndIsNotCheckedAgain(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", map[string]string{"fail-check": "1"})
	if r := f.run(); r != CheckFailed {
		t.Fatalf("run: %s", r)
	}
	ms := f.outbox()
	if contains(ms, "Apply this commit?") || !contains(ms, "The check of") || !contains(ms, "bad config &lt;b&gt;") {
		t.Fatalf("a failed check sends a notice with its output, and no question: %v", ms)
	}
	if r := f.run(); r != Nothing {
		t.Fatalf("second run: %s", r)
	}
	if got := strings.Count(f.read("check-count"), "x"); got != 1 {
		t.Fatalf("the check ran %d times, want 1", got)
	}
}

func TestCheckAndSummaryRunAsCheckUser(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	f.run()
	want := strconv.Itoa(f.env.Sandbox.UID)
	if f.read("check-uid") != want || f.read("summary-uid") != want {
		t.Fatalf("check uid %q, summary uid %q, want %s", f.read("check-uid"), f.read("summary-uid"), want)
	}
	if f.read("check-uid") == "0" {
		t.Fatal("check ran as root")
	}
}

func TestResolveSandboxNeverRoot(t *testing.T) {
	if _, err := ResolveSandbox(0, "root"); err == nil {
		t.Fatal("check_user root must be refused")
	}
	if _, err := ResolveSandbox(1000, "root"); err == nil {
		t.Fatal("check_user root must be refused, also when not root")
	}
	if _, err := ResolveSandbox(0, "no-such-user-pullgate"); err == nil {
		t.Fatal("a missing user must be refused")
	}
	f := newFixture(t, ``, "")
	me := f.env.Sandbox.UID
	sb, err := ResolveSandbox(0, f.env.Cfg.Commands.CheckUser)
	if me != 0 && (err != nil || !sb.Switch || sb.UID != me) {
		t.Fatalf("as root, check switches to check_user: %+v %v", sb, err)
	}
	if _, err := ResolveSandbox(me+1, f.env.Cfg.Commands.CheckUser); err == nil {
		t.Fatal("not as root, check_user must be the running user")
	}
	// The command of check and summary carries the credential.
	f.env.Sandbox = Sandbox{UID: 4242, GID: 4343, Switch: true}
	cmd := f.env.command(context.Background(), []string{"/bin/true"}, cmdEnv{Tree: "/"}, true)
	c := cmd.SysProcAttr.Credential
	if c == nil || c.Uid != 4242 || c.Gid != 4343 || len(c.Groups) != 0 {
		t.Fatalf("credential %+v", c)
	}
	if cmd := f.env.command(context.Background(), []string{"/bin/true"}, cmdEnv{Tree: "/"}, false); cmd.SysProcAttr.Credential != nil {
		t.Fatal("apply runs as root")
	}
	for _, kv := range cmd.Env {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "LANG=") && !strings.HasPrefix(kv, "PULLGATE_") {
			t.Fatalf("unexpected env %q", kv)
		}
	}
}

func TestRootRefusesGitInWritableDir(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	f.run()
	if err := os.Chmod(filepath.Join(f.env.Dir.Repo(), "config"), 0o666); err != nil {
		t.Fatal(err)
	}
	f.commit("second", "two", nil)
	if _, err := f.env.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "can be written by group or others") {
		t.Fatalf("root must refuse git there: %v", err)
	}
}

func TestFailingApplyRollsBack(t *testing.T) {
	for _, fail := range []string{"fail-apply", "fail-health"} {
		t.Run(fail, func(t *testing.T) {
			f := newFixture(t, `on_failure = "rollback"`, "")
			a := f.applyNew("one")
			b := f.commit("bad", "two", map[string]string{fail: "1"})
			f.run()
			f.tap(f.ask().Code, "apply")
			if r := f.apply(); r != RolledBack {
				t.Fatalf("apply: %s", r)
			}
			st := f.state()
			if st.Applied != a || st.InProgress != nil || f.env.Dir.Paused() {
				t.Fatalf("state after rollback: %+v", st)
			}
			if f.read("live") != "one" {
				t.Fatalf("live %q, want the old value", f.read("live"))
			}
			if !strings.Contains(f.read("log"), "apply "+a+" old="+b+" rb=1 failed="+b) {
				t.Fatalf("the rollback env is wrong:\n%s", f.read("log"))
			}
			if !contains(f.outbox(), "Rolled back to <code>"+a) {
				t.Fatal("no report")
			}
			// max_failed_tries = 1: not asked again.
			if r := f.run(); r != Nothing {
				t.Fatalf("run: %s", r)
			}
		})
	}
}

func TestMaxFailedTries(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "[limits]\nmax_failed_tries = 2\n")
	f.applyNew("one")
	f.commit("bad", "two", map[string]string{"fail-apply": "1"})
	for try := 1; try <= 2; try++ {
		if r := f.run(); r != Asked {
			t.Fatalf("try %d: run %s", try, r)
		}
		f.tap(f.ask().Code, "apply")
		if r := f.apply(); r != RolledBack {
			t.Fatalf("try %d: apply %s", try, r)
		}
		f.outbox()
	}
	if r := f.run(); r != Nothing {
		t.Fatalf("after max_failed_tries the SHA waits: %s", r)
	}
}

func TestFailingRollbackPauses(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "")
	f.applyNew("one")
	f.commit("bad", "two", map[string]string{"fail-health": "1"})
	f.run()
	f.tap(f.ask().Code, "apply")
	f.touch("fail-rollback")
	if r := f.apply(); r != RollbackError {
		t.Fatalf("apply: %s", r)
	}
	if !f.env.Dir.Paused() || f.state().InProgress != nil {
		t.Fatal("a failed rollback must write the pause file")
	}
	if !contains(f.outbox(), "The instance is paused") {
		t.Fatal("no report")
	}
	f.commit("fix", "three", nil)
	if r := f.run(); r != Paused {
		t.Fatalf("a paused instance does nothing: %s", r)
	}
}

func TestOnFailurePauseHoldsAndAsksNewer(t *testing.T) {
	f := newFixture(t, `on_failure = "pause"`, "")
	a := f.applyNew("one")
	b := f.commit("bad", "two", map[string]string{"fail-apply": "1"})
	f.run()
	f.tap(f.ask().Code, "apply")
	if r := f.apply(); r != Held {
		t.Fatalf("apply: %s", r)
	}
	if strings.Contains(f.read("log"), "rb=1") {
		t.Fatal("pause must not roll back")
	}
	if f.env.Dir.Paused() || f.state().Held != b || f.state().Applied != a {
		t.Fatalf("pause holds the SHA and writes no pause file: %+v", f.state())
	}
	if !contains(f.outbox(), "It is held and not tried again") {
		t.Fatal("no report")
	}
	if r := f.run(); r != Nothing {
		t.Fatalf("the held SHA is not tried again: %s", r)
	}
	var out strings.Builder
	if err := f.env.Status(&out, StatusFlags{}); err != nil || !strings.Contains(out.String(), "held:") {
		t.Fatalf("status must show the held SHA: %s", out.String())
	}
	c := f.commit("fix", "three", map[string]string{"fail-apply": ""})
	if r := f.run(); r != Asked {
		t.Fatalf("a newer commit is asked: %s", r)
	}
	q := f.ask()
	if !strings.Contains(q.Text, "⚠ The last apply (<code>"+b+"</code>) failed.") {
		t.Fatalf("the warning is missing:\n%s", q.Text)
	}
	f.tap(q.Code, "apply")
	if r := f.apply(); r != Applied || f.state().Applied != c || f.state().Held != "" {
		t.Fatalf("the fix must apply: %s %+v", r, f.state())
	}
	if !strings.Contains(f.read("log"), "apply "+c+" old="+a+" rb= failed="+b) {
		t.Fatalf("PULLGATE_FAILED_SHA: %s", f.read("log"))
	}
}

func TestCodeDefaultIsPause(t *testing.T) {
	f := newFixture(t, ``, "")
	if f.env.Cfg.OnFailure != "pause" {
		t.Fatalf("the code default must be pause, got %q", f.env.Cfg.OnFailure)
	}
	f.applyNew("one")
	f.commit("bad", "two", map[string]string{"fail-apply": "1"})
	f.run()
	f.tap(f.ask().Code, "apply")
	if r := f.apply(); r != Held {
		t.Fatalf("apply: %s", r)
	}
}

func TestFirstApplyFailsWithRollbackHolds(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "")
	f.commit("bad", "one", map[string]string{"fail-apply": "1"})
	f.run()
	f.tap(f.ask().Code, "apply")
	if r := f.apply(); r != Held || f.env.Dir.Paused() {
		t.Fatalf("with nothing to roll back to, the SHA is held: %s", r)
	}
}

func TestPauseFile(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	f.run()
	q := f.ask()
	if err := f.env.Status(new(strings.Builder), StatusFlags{Pause: true}); err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r != Paused {
		t.Fatalf("run: %s", r)
	}
	f.tap(q.Code, "apply")
	if r := f.apply(); r != Paused || f.state().Applied != "" {
		t.Fatalf("a paused instance must not apply: %s", r)
	}
	if !contains(f.outbox(), "ignored: the instance is paused") {
		t.Fatal("no report")
	}
	if err := f.env.Status(new(strings.Builder), StatusFlags{Resume: true}); err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r != Asked {
		t.Fatalf("after resume the tip is asked again: %s", r)
	}
}

func TestOffMode(t *testing.T) {
	f := newFixture(t, `mode = "off"`, "")
	f.commit("first", "one", nil)
	if r := f.run(); r != Off {
		t.Fatalf("run: %s", r)
	}
	if r := f.apply(); r != Off {
		t.Fatalf("apply: %s", r)
	}
}

func TestLock(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	if err := f.env.Dir.Ensure(); err != nil {
		t.Fatal(err)
	}
	l, err := f.env.Dir.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r != Busy {
		t.Fatalf("a second run must stop: %s", r)
	}
	_ = l.Close()
	if r := f.run(); r != Asked {
		t.Fatalf("run: %s", r)
	}
}

func TestQuestionsPerDay(t *testing.T) {
	f := newFixture(t, ``, "[limits]\nquestions_per_day = 1\n")
	f.commit("first", "one", nil)
	if r := f.run(); r != Asked {
		t.Fatalf("run: %s", r)
	}
	f.outbox()
	f.commit("second", "two", nil)
	if r := f.run(); r != LimitReached {
		t.Fatalf("the limit must hold: %s", r)
	}
	if !contains(f.outbox(), "questions_per_day") {
		t.Fatal("one notice")
	}
	f.run()
	if len(f.outbox()) != 0 {
		t.Fatal("only one notice per day")
	}
	f.now = f.now.Add(25 * time.Hour)
	if r := f.run(); r != Asked {
		t.Fatalf("a day later it asks: %s", r)
	}
}

func TestAutoMode(t *testing.T) {
	f := newFixture(t, `mode = "auto"`, "")
	sha := f.commit("first", "one", nil)
	if r := f.run(); r != AutoQueued {
		t.Fatalf("run: %s", r)
	}
	for _, m := range f.outbox() {
		if m.Kind == spool.KindAsk {
			t.Fatal("auto mode asks no question")
		}
	}
	if r := f.apply(); r != Applied || f.state().Applied != sha {
		t.Fatalf("apply: %s", r)
	}
	if _, err := os.Lstat(f.env.Dir.AutoFile()); err == nil {
		t.Fatal("the auto approval must be removed")
	}
	// An auto approval that root did not write is refused.
	f.commit("second", "two", nil)
	f.run()
	if err := os.Chmod(f.env.Dir.AutoFile(), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := f.apply(); r == Applied {
		t.Fatal("an auto approval that others can read or write must be refused")
	}
}

func TestCrashBetweenApplyAndStateWrite(t *testing.T) {
	f := newFixture(t, ``, "")
	f.applyNew("one")
	b := f.commit("second", "two", nil)
	st := f.state()
	st.InProgress = &state.InProgress{Phase: "apply", SHA: b, OldSHA: st.Applied, Started: f.now}
	if err := f.env.Dir.Save(st); err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r != CutOff {
		t.Fatalf("run: %s", r)
	}
	if !f.env.Dir.Paused() || f.state().InProgress != nil || f.state().Marks[b].Mark != state.MarkFailed {
		t.Fatalf("a cut-off apply must pause and mark the SHA failed: %+v", f.state())
	}
	if !contains(f.outbox(), "was cut off") {
		t.Fatal("no report")
	}
}

func TestSymlinkWarningAndRefuse(t *testing.T) {
	f := newFixture(t, ``, "")
	if err := os.Symlink("/etc/shadow", filepath.Join(f.r.Work, "conf")); err != nil {
		t.Fatal(err)
	}
	f.commit("link", "one", nil)
	f.run()
	if q := f.ask(); !strings.Contains(q.Text, "⚠ Symlink out of the tree: <code>conf</code> → <code>/etc/shadow</code>") {
		t.Fatalf("the symlink warning is missing:\n%s", q.Text)
	}
	g := newFixture(t, ``, "[limits]\nrefuse_symlinks = true\n")
	if err := os.Symlink("../../x", filepath.Join(g.r.Work, "conf")); err != nil {
		t.Fatal(err)
	}
	g.commit("link", "one", nil)
	if r := g.run(); r != CheckFailed {
		t.Fatalf("refuse_symlinks must stop the commit: %s", r)
	}
	if contains(g.outbox(), "Apply this commit?") {
		t.Fatal("no question for a refused commit")
	}
}

func TestBranchBackToApplied(t *testing.T) {
	f := newFixture(t, ``, "")
	a := f.applyNew("one")
	f.commit("second", "two", nil)
	f.run()
	f.outbox()
	f.r.Push(a)
	if r := f.run(); r != Nothing || f.state().Open() != nil {
		t.Fatalf("back to the applied SHA closes the question: %s", r)
	}
	if !contains(f.outbox(), "back to the applied commit") {
		t.Fatal("no close")
	}
}

func TestCheckTimeout(t *testing.T) {
	f := newFixture(t, ``, "")
	f.script("check.sh", "sleep 30\n")
	f.env.Cfg.Commands.CheckTimeout.Duration = time.Second
	f.commit("first", "one", nil)
	start := time.Now()
	if r := f.run(); r != CheckFailed {
		t.Fatalf("run: %s", r)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("the timeout did not stop the check")
	}
	if !contains(f.outbox(), "timed out") {
		t.Fatal("no notice")
	}
}

func TestClosedQuestionAsksAgainWhenTheSHAComesBack(t *testing.T) {
	f := newFixture(t, ``, "")
	a := f.applyNew("one")
	b := f.commit("second", "two", nil)
	f.run()
	f.outbox()
	f.r.Push(a) // the branch goes back: the question for b is closed
	f.run()
	f.outbox()
	f.r.Push(b) // and b comes back
	if r := f.run(); r != Asked {
		t.Fatalf("b must be asked again: %s", r)
	}
}

func TestFailedOutboxWriteIsTriedAgain(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	// The check runs after the spool is set up: it breaks the outbox.
	f.script("check.sh", "chmod 0500 "+f.env.Spool.Outbox()+"\n")
	if _, err := f.env.Run(context.Background()); err == nil {
		t.Fatal("a failed outbox write must be an error")
	}
	if f.state().Open() != nil {
		t.Fatal("an undelivered question must not stay open")
	}
	f.script("check.sh", "true\n")
	if r := f.run(); r != Asked { // Ensure fixes the mode again
		t.Fatalf("the next run must ask: %s", r)
	}
}

func TestStopDuringApplyLeavesInProgress(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "")
	f.applyNew("one")
	f.commit("second", "two", nil)
	f.run()
	f.tap(f.ask().Code, "apply")
	f.script("apply.sh", "sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	if _, err := f.env.Apply(ctx); err == nil {
		t.Fatal("a stop during apply must be an error")
	}
	if f.state().InProgress == nil || f.env.Dir.Paused() {
		t.Fatal("a stop must leave in_progress, and must not roll back or pause yet")
	}
	if r := f.run(); r != CutOff || !f.env.Dir.Paused() {
		t.Fatalf("the next start must pause: %s", r)
	}
}
