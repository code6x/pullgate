package gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/ghdeploy"
	"github.com/code6x/pullgate/internal/ghdeploy/fakegh"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
	"github.com/code6x/pullgate/internal/testgit"
)

type ghFix struct {
	*fixture
	srv   *fakegh.Server
	token string
	log   *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newGH is a fixture with a [github] block, a fake GitHub server and a
// token made by the test.
func newGH(t *testing.T, top, sections string) *ghFix {
	t.Helper()
	srv := fakegh.New(t)
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := "github_pat_TEST" + hex.EncodeToString(b)
	tf := filepath.Join(testgit.PrivateDir(t), "config-repo.github-token")
	if err := os.WriteFile(tf, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	block := fmt.Sprintf("\n[github]\nrepository = \"owner/config-repo\"\nenvironment = \"example-host\"\ntoken_file = %q\napi_url = %q\n", tf, srv.URL)
	f := &ghFix{fixture: newFixture(t, top, sections+block), srv: srv, token: tok, log: &syncBuf{}}
	f.setup = func(e *Env) {
		e.GitHubHTTP = ghdeploy.NewHTTPClient(nil)
		e.Log = slog.New(slog.NewTextHandler(f.log, nil))
	}
	f.load()
	return f
}

func create(sha string) string { return "create " + sha }

func post(st, sha, desc string) string { return st + " " + sha + " " + desc }

func started(sha string) string { return post(ghdeploy.InProgress, sha, ghdeploy.DescApplyStarted) }

func (f *ghFix) want(want ...string) {
	f.t.Helper()
	if got := f.srv.Steps(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		f.t.Fatalf("GitHub got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func (f *ghFix) queue() *ghdeploy.Queue {
	f.t.Helper()
	q, err := ghdeploy.Open(ghdeploy.Dir(f.env.Dir.Path), uint32(os.Geteuid()))
	if err != nil {
		f.t.Fatal(err)
	}
	return q
}

// down makes the fake GitHub answer 503 until up.
func (f *ghFix) down() { f.srv.Answer = func(fakegh.Call) (int, string) { return 503, "" } }
func (f *ghFix) up()   { f.srv.Answer = nil }

func (f *fixture) stateDir() string { return f.env.Dir.Path }

func TestGitHubOffRunsNoGitHubCode(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "")
	f.applyNew("one")
	if _, err := os.Lstat(ghdeploy.Dir(f.stateDir())); err == nil {
		t.Fatal("without [github] no queue folder is made")
	}
	if f.env.gh != nil || f.env.ghToken != "" {
		t.Fatal("without [github] no GitHub code runs")
	}
	var out bytes.Buffer
	if err := f.env.Status(&out, StatusFlags{}); err != nil || strings.Contains(out.String(), "github") {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
}

func TestGitHubApplyPostsBeforeTheCommand(t *testing.T) {
	f := newGH(t, "", "")
	var live []bool
	f.srv.Answer = func(fakegh.Call) (int, string) {
		_, err := os.Stat(filepath.Join(f.ctrl, "live"))
		live = append(live, err == nil)
		return 0, ""
	}
	a := f.applyNew("one")
	f.want(create(a), started(a), post("success", a, ghdeploy.DescApplied))
	if fmt.Sprint(live) != "[false false true]" {
		t.Fatalf("the create and in_progress must come before the apply command: %v", live)
	}
	for _, c := range f.srv.Calls() {
		if c.Auth != "Bearer "+f.token || c.Method != "POST" || !strings.HasPrefix(c.Path, "/repos/owner/config-repo/deployments") {
			t.Fatalf("%+v", c)
		}
	}
	if c := f.srv.Calls()[0]; c.Body["ref"] != a || len(a) != 40 || c.Body["environment"] != "example-host" || c.Body["auto_merge"] != false ||
		c.Body["production_environment"] != false || c.Body["transient_environment"] != false || c.Body["payload"] != nil || fmt.Sprint(c.Body["required_contexts"]) != "[]" {
		t.Fatalf("create: %v", c.Body)
	}
	// Nothing more on the next runs: no false cut-off.
	f.run()
	f.apply()
	f.want(create(a), started(a), post("success", a, ghdeploy.DescApplied))
}

func TestGitHubAutoModePostsTheSame(t *testing.T) {
	f := newGH(t, `mode = "auto"`, "")
	a := f.commit("first", "one", nil)
	if r := f.run(); r != AutoQueued {
		t.Fatal(r)
	}
	f.want()
	if r := f.apply(); r != Applied {
		t.Fatal(r)
	}
	f.want(create(a), started(a), post("success", a, ghdeploy.DescApplied))
}

func TestGitHubRollbackPosts(t *testing.T) {
	f := newGH(t, `on_failure = "rollback"`, "")
	a := f.applyNew("one")
	f.srv.Reset()
	var rolled []bool
	f.srv.Answer = func(fakegh.Call) (int, string) {
		rolled = append(rolled, strings.Contains(f.read("log"), "rb=1"))
		return 0, ""
	}
	b := f.commit("bad", "two", map[string]string{"fail-health": "1"})
	f.run()
	f.tap(f.ask().Code, "apply")
	if r := f.apply(); r != RolledBack {
		t.Fatal(r)
	}
	// The failure and the rollback's create and in_progress go out before
	// the rollback runs.
	if fmt.Sprint(rolled) != "[false false false false false true]" {
		t.Fatalf("%v", rolled)
	}
	all := []string{create(b), started(b), post("failure", b, ghdeploy.DescRollbackStarts),
		create(a), post("in_progress", a, ghdeploy.DescRollbackStarted), post("success", a, ghdeploy.DescRolledBack)}
	f.want(all...)
	f.run()
	f.apply()
	f.want(all...)
}

func TestGitHubBrokenAndFixForwardPosts(t *testing.T) {
	f := newGH(t, `on_failure = "rollback"`, "")
	a, b := breakIt(t, f.fixture)
	broken := []string{create(a), started(a), post("success", a, ghdeploy.DescApplied),
		create(b), started(b), post("failure", b, ghdeploy.DescRollbackStarts),
		create(a), post("in_progress", a, ghdeploy.DescRollbackStarted), post("error", a, ghdeploy.DescBroken)}
	f.want(broken...)
	f.outbox()
	// A failed fix forward: error, no rollback.
	c := f.commit("fix", "three", map[string]string{"fail-health": "1"})
	f.run()
	f.tap(f.ask().Code, "apply")
	if r := f.apply(); r != StillBroken {
		t.Fatal(r)
	}
	broken = append(broken, create(c), started(c), post("error", c, ghdeploy.DescFixForwardFailed))
	f.want(broken...)
	d := f.commit("fix 2", "four", map[string]string{"fail-health": ""})
	f.run()
	f.tap(f.ask().Code, "apply")
	if r := f.apply(); r != Applied {
		t.Fatal(r)
	}
	broken = append(broken, create(d), started(d), post("success", d, ghdeploy.DescFixedForward))
	f.want(broken...)
	f.run()
	f.want(broken...)
}

func TestGitHubHeldPosts(t *testing.T) {
	for _, top := range []string{``, `on_failure = "rollback"`} {
		f := newGH(t, top, "")
		var a string
		if top == "" {
			a = f.applyNew("one")
		}
		f.srv.Reset()
		b := f.commit("bad", "two", map[string]string{"fail-health": "1"})
		f.run()
		f.tap(f.ask().Code, "apply")
		if r := f.apply(); r != Held {
			t.Fatalf("%s: %s (applied %s)", top, r, a)
		}
		f.want(create(b), started(b), post("failure", b, ghdeploy.DescHeld))
	}
}

// Steps that post nothing: a skip, an expired tap, a refused tap, a tap
// of the applied commit, a failed check, a check that found nothing, and
// a tip that still settles.
func TestGitHubStepsThatPostNothing(t *testing.T) {
	f := newGH(t, "", "")
	a := f.applyNew("one")
	b := f.applyNew("two")
	f.srv.Reset()
	f.commit("two", "two again", nil)
	f.run()
	f.tap(f.ask().Code, "skip")
	if r := f.apply(); r != Skipped {
		t.Fatal(r)
	}
	f.commit("three", "three", nil)
	f.run()
	c := f.ask().Code
	f.now = f.now.Add(13 * time.Hour)
	f.tap(c, "apply")
	if r := f.apply(); r != Ignored {
		t.Fatal(r)
	}
	f.commit("docs", "four", map[string]string{"nothing-here": "1"})
	if r := f.run(); r != NothingToDo {
		t.Fatal(r)
	}
	f.commit("bad check", "five", map[string]string{"fail-check": "1"})
	if r := f.run(); r != CheckFailed {
		t.Fatal(r)
	}
	// A tap of the applied commit, and a tap of a commit that does not
	// descend from it.
	st := f.state()
	st.Questions = append(st.Questions,
		state.Question{ID: 90, Code: "appliedcodexxxxxxxxxxx", SHA: b, Base: b, Asked: f.now, Expires: f.now.Add(time.Hour)},
		state.Question{ID: 91, Code: "refusedcodexxxxxxxxxxx", SHA: a, Base: b, Asked: f.now, Expires: f.now.Add(time.Hour)})
	if err := f.env.Dir.Save(st); err != nil {
		t.Fatal(err)
	}
	f.tap("appliedcodexxxxxxxxxxx", "apply")
	if r := f.apply(); r != Ignored {
		t.Fatal(r)
	}
	f.tap("refusedcodexxxxxxxxxxx", "apply")
	if r := f.apply(); r != Refused {
		t.Fatal(r)
	}
	if len(f.srv.All()) != 0 {
		t.Fatalf("nothing must be posted: %v", f.srv.Steps())
	}
	g := newGH(t, "", "\n[limits]\nsettle = \"1h\"\n")
	g.commit("first", "one", nil)
	if r := g.run(); r != Settling || len(g.srv.All()) != 0 {
		t.Fatalf("%s %v", r, g.srv.Steps())
	}
}

// A crash between the queue write and the state save: the step never
// started, and its deployment gets its own error. Nothing pauses.
func TestGitHubCrashBeforeTheStateSave(t *testing.T) {
	f := newGH(t, "", "")
	a := f.applyNew("one")
	b := f.commit("second", "two", nil)
	q := f.queue()
	q.Start(ghdeploy.RoleApply, b, ghdeploy.Target{API: f.srv.URL, Repository: "owner/config-repo", Environment: "example-host"}, ghdeploy.DescApplyStarted, f.now)
	if err := q.Save(); err != nil {
		t.Fatal(err)
	}
	f.srv.Reset()
	if r := f.run(); r != Sent || f.env.Dir.Paused() {
		t.Fatalf("run: %s", r)
	}
	f.want(create(b), started(b), post("error", b, ghdeploy.DescStopped))
	_ = a
}

// A crash after the rollback was queued, before its state save: the
// rollback never ran (stopped before), the apply was cut off.
func TestGitHubCrashBeforeTheRollbackSave(t *testing.T) {
	f := newGH(t, `on_failure = "rollback"`, "")
	a := f.applyNew("one")
	b := f.commit("second", "two", nil)
	q := f.queue()
	k := q.Start(ghdeploy.RoleApply, b, ghdeploy.Target{API: f.srv.URL, Repository: "owner/config-repo", Environment: "example-host"}, ghdeploy.DescApplyStarted, f.now)
	q.Status(k, ghdeploy.Failure, ghdeploy.DescRollbackStarts, f.now)
	q.Start(ghdeploy.RoleRollback, a, ghdeploy.Target{API: f.srv.URL, Repository: "owner/config-repo", Environment: "example-host"}, ghdeploy.DescRollbackStarted, f.now)
	if err := q.Save(); err != nil {
		t.Fatal(err)
	}
	st := f.state()
	st.InProgress = &state.InProgress{Phase: "apply", SHA: b, OldSHA: a, Started: f.now}
	if err := f.env.Dir.Save(st); err != nil {
		t.Fatal(err)
	}
	f.srv.Reset()
	if r := f.run(); r != CutOff {
		t.Fatal(r)
	}
	f.want(create(b), started(b), post("failure", b, ghdeploy.DescRollbackStarts),
		create(a), post("in_progress", a, ghdeploy.DescRollbackStarted),
		post("error", a, ghdeploy.DescStopped), post("error", b, ghdeploy.DescCutOff))
}

func TestGitHubStopDuringTheApplyIsCutOff(t *testing.T) {
	f := newGH(t, `on_failure = "rollback"`, "")
	f.applyNew("one")
	f.srv.Reset()
	b := f.commit("second", "two", nil)
	f.run()
	f.tap(f.ask().Code, "apply")
	f.script("apply.sh", "sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	if _, err := f.env.Apply(ctx); err == nil {
		t.Fatal("a stop must be an error")
	}
	f.want(create(b), started(b))
	if r := f.run(); r != CutOff {
		t.Fatal(r)
	}
	f.want(create(b), started(b), post("error", b, ghdeploy.DescCutOff))
	_ = f.env.Dir.Resume()
	f.run()
	f.want(create(b), started(b), post("error", b, ghdeploy.DescCutOff))
}

// status --resume clears an in_progress that no run found: the same error.
func TestGitHubResumePostsCutOff(t *testing.T) {
	f := newGH(t, "", "")
	f.applyNew("one")
	b := f.commit("second", "two", nil)
	f.run()
	f.tap(f.ask().Code, "apply")
	f.script("apply.sh", "sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	_, _ = f.env.Apply(ctx)
	f.srv.Reset()
	var out bytes.Buffer
	if err := f.env.Status(&out, StatusFlags{Resume: true}); err != nil {
		t.Fatal(err)
	}
	f.want(post("error", b, ghdeploy.DescCutOff))
}

// prepend puts a line at the top of a test script.
func (f *fixture) prepend(n, line string) {
	f.t.Helper()
	p := filepath.Join(f.base, "bin", n)
	b, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	f.script(n, line+"\n"+strings.TrimPrefix(string(b), "#!/bin/sh\n"))
}

// A success queued before a failed state save: the next run's cut-off is
// the last status.
func TestGitHubFailedSaveAfterSuccessIsCutOff(t *testing.T) {
	f := newGH(t, "", "")
	f.applyNew("one")
	f.srv.Reset()
	b := f.commit("second", "two", nil)
	f.run()
	f.tap(f.ask().Code, "apply")
	f.prepend("health.sh", "chmod 511 "+f.stateDir())
	if _, err := f.env.Apply(context.Background()); err == nil {
		t.Fatal("the state save must fail")
	}
	f.prepend("health.sh", "chmod 711 "+f.stateDir())
	if r := f.run(); r != CutOff {
		t.Fatal(r)
	}
	f.want(create(b), started(b), post("success", b, ghdeploy.DescApplied), post("error", b, ghdeploy.DescCutOff))
}

// A failed save of the broken state: exactly one error on the rollback.
func TestGitHubFailedBrokenSaveGivesOneError(t *testing.T) {
	f := newGH(t, `on_failure = "rollback"`, "")
	a := f.applyNew("one")
	f.srv.Reset()
	b := f.commit("bad", "two", map[string]string{"fail-health": "1"})
	f.run()
	f.tap(f.ask().Code, "apply")
	f.prepend("apply.sh", `if [ "$PULLGATE_ROLLBACK" = 1 ]; then chmod 511 `+f.stateDir()+`; exit 3; fi`)
	if r, err := f.env.Apply(context.Background()); r != RollbackError || err == nil {
		t.Fatalf("%s %v", r, err)
	}
	if r := f.run(); r != CutOff {
		t.Fatal(r)
	}
	f.want(create(b), started(b), post("failure", b, ghdeploy.DescRollbackStarts),
		create(a), post("in_progress", a, ghdeploy.DescRollbackStarted), post("error", a, ghdeploy.DescBroken))
}

// A run with a check request waits for the lock of an apply; it must not
// see that apply as cut off.
func TestGitHubCheckRunWaitingForTheLockIsNoCutOff(t *testing.T) {
	f := newGH(t, "", "")
	a := f.applyNew("one")
	b := f.commit("second", "two", nil)
	lock, err := f.env.Dir.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	// What an apply does before its command runs.
	q := f.queue()
	k := q.Start(ghdeploy.RoleApply, b, ghdeploy.Target{API: f.srv.URL, Repository: "owner/config-repo", Environment: "example-host"}, ghdeploy.DescApplyStarted, f.now)
	if err := q.Save(); err != nil {
		t.Fatal(err)
	}
	st := f.state()
	st.InProgress = &state.InProgress{Phase: "apply", SHA: b, OldSHA: a, Started: f.now}
	if err := f.env.Dir.Save(st); err != nil {
		t.Fatal(err)
	}
	if err := f.env.Spool.WriteCheck(); err != nil {
		t.Fatal(err)
	}
	done := make(chan Result)
	go func() { r, _ := f.env.Run(context.Background()); done <- r }()
	time.Sleep(500 * time.Millisecond)
	// The apply ends.
	q.Status(k, ghdeploy.Success, ghdeploy.DescApplied, f.now)
	if err := q.Save(); err != nil {
		t.Fatal(err)
	}
	st.InProgress, st.Applied = nil, b
	if err := f.env.Dir.Save(st); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	if r := <-done; r == CutOff {
		t.Fatal("a false cut-off")
	}
	for _, s := range f.srv.Steps() {
		if strings.Contains(s, "error") {
			t.Fatalf("a false error: %v", f.srv.Steps())
		}
	}
}

func TestGitHubBusyTimerRunChecksNothing(t *testing.T) {
	f := newGH(t, "", "")
	f.applyNew("one")
	b := f.commit("second", "two", nil)
	lock, err := f.env.Dir.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	q := f.queue()
	q.Start(ghdeploy.RoleApply, b, ghdeploy.Target{API: f.srv.URL, Repository: "owner/config-repo", Environment: "example-host"}, ghdeploy.DescApplyStarted, f.now)
	if err := q.Save(); err != nil {
		t.Fatal(err)
	}
	f.srv.Reset()
	if r := f.run(); r != Busy {
		t.Fatal(r)
	}
	if len(f.srv.All()) != 0 || len(f.queue().Posts) != 2 {
		t.Fatalf("a busy run sends and checks nothing: %v", f.srv.Steps())
	}
	_ = lock.Close()
}

// The queue goes out on every exit: paused, off, held, broken, cut off.
func TestGitHubSendsWhenPausedOffHeldBrokenOrCutOff(t *testing.T) {
	t.Run("paused", func(t *testing.T) {
		f := newGH(t, "", "")
		f.down()
		a := f.applyNew("one")
		if err := f.env.Dir.Pause("test"); err != nil {
			t.Fatal(err)
		}
		f.up()
		f.srv.Reset()
		if r := f.run(); r != Paused {
			t.Fatal(r)
		}
		f.want(create(a), started(a), post("success", a, ghdeploy.DescApplied))
		f.srv.Reset()
		f.queue() // nothing waits
		f.apply()
		f.want()
	})
	t.Run("off", func(t *testing.T) {
		f := newGH(t, `mode = "off"`, "")
		b := f.commit("first", "one", nil)
		if err := f.env.Dir.Ensure(); err != nil {
			t.Fatal(err)
		}
		q := f.queue()
		q.Start(ghdeploy.RoleApply, b, ghdeploy.Target{API: f.srv.URL, Repository: "owner/config-repo", Environment: "example-host"}, ghdeploy.DescApplyStarted, f.now)
		if err := q.Save(); err != nil {
			t.Fatal(err)
		}
		f.down()
		if r := f.apply(); r != Off {
			t.Fatal(r)
		}
		f.up()
		if r := f.run(); r != Off {
			t.Fatal(r)
		}
		f.want(create(b), started(b), post("error", b, ghdeploy.DescStopped))
	})
	t.Run("held", func(t *testing.T) {
		f := newGH(t, "", "")
		f.applyNew("one")
		f.down()
		b := f.commit("bad", "two", map[string]string{"fail-health": "1"})
		f.run()
		f.tap(f.ask().Code, "apply")
		if r := f.apply(); r != Held {
			t.Fatal(r)
		}
		f.up()
		f.srv.Reset()
		f.run()
		f.want(create(b), started(b), post("failure", b, ghdeploy.DescHeld))
	})
	t.Run("broken", func(t *testing.T) {
		f := newGH(t, `on_failure = "rollback"`, "")
		f.down()
		a, b := breakIt(t, f.fixture)
		f.up()
		f.run()
		f.want(create(a), started(a), post("success", a, ghdeploy.DescApplied),
			create(b), started(b), post("failure", b, ghdeploy.DescRollbackStarts),
			create(a), post("in_progress", a, ghdeploy.DescRollbackStarted), post("error", a, ghdeploy.DescBroken))
	})
	t.Run("cut off", func(t *testing.T) {
		f := newGH(t, "", "")
		f.applyNew("one")
		b := f.commit("second", "two", nil)
		f.run()
		f.tap(f.ask().Code, "apply")
		f.script("apply.sh", "sleep 30\n")
		f.down()
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(500*time.Millisecond, cancel)
		_, _ = f.env.Apply(ctx)
		f.up()
		f.srv.Reset()
		if r := f.run(); r != CutOff {
			t.Fatal(r)
		}
		f.want(create(b), started(b), post("error", b, ghdeploy.DescCutOff))
	})
}

func TestGitHubThatNeverAnswersAddsAtMostTheSendLimit(t *testing.T) {
	if ghdeploy.SendLimit != 10*time.Second {
		t.Fatal("the send limit is 10 seconds")
	}
	f := newGH(t, "", "")
	setup := f.setup
	f.setup = func(e *Env) { setup(e); e.GitHubSendLimit = 700 * time.Millisecond }
	f.load()
	f.srv.Hang(t)
	a := f.commit("first", "one", nil)
	f.run()
	f.tap(f.ask().Code, "apply")
	start := time.Now()
	if r := f.apply(); r != Applied || f.state().Applied != a {
		t.Fatal(r)
	}
	if d := time.Since(start); d > 700*time.Millisecond+3*time.Second {
		t.Fatalf("apply took %s", d)
	}
	start = time.Now()
	f.run()
	if d := time.Since(start); d > 700*time.Millisecond+3*time.Second {
		t.Fatalf("run took %s", d)
	}
	if len(f.queue().Posts) != 3 {
		t.Fatal("the posts wait")
	}
}

// The send limit counts the time spent sending: an apply command that
// runs longer than it still gets its final status sent by the same
// process.
func TestGitHubLongApplyStillSendsItsResult(t *testing.T) {
	f := newGH(t, "", "")
	setup := f.setup
	f.setup = func(e *Env) { setup(e); e.GitHubSendLimit = 300 * time.Millisecond }
	f.load()
	a := f.commit("first", "one", nil)
	f.run()
	f.tap(f.ask().Code, "apply")
	f.prepend("apply.sh", "sleep 1")
	if r := f.apply(); r != Applied {
		t.Fatal(r)
	}
	f.want(create(a), started(a), post("success", a, ghdeploy.DescApplied))
}

var shaRE = regexp.MustCompile(`[0-9a-f]{7,40}`)

// scenario applies one, fails two with a rollback, then applies three.
// It returns the results, the state and the messages, without SHAs.
func scenario(t *testing.T, f *fixture) (string, string, []string) {
	t.Helper()
	var res []string
	var msgs []spool.Message
	for _, c := range []struct{ subject, value, file string }{{"one", "one", ""}, {"bad", "two", "fail-health"}, {"good", "three", "fail-health"}} {
		more := map[string]string{}
		if c.file != "" {
			more[c.file] = "1"
			if c.subject == "good" {
				more[c.file] = ""
			}
		}
		f.commit(c.subject, c.value, more)
		res = append(res, string(f.run()))
		out := f.outbox()
		msgs = append(msgs, out...)
		for _, m := range out {
			if m.Kind == spool.KindAsk {
				f.tap(m.Code, "apply")
			}
		}
		r, err := f.env.Apply(context.Background())
		res = append(res, string(r), fmt.Sprint(err))
		msgs = append(msgs, f.outbox()...)
	}
	var texts []string
	for _, m := range msgs {
		texts = append(texts, m.Kind+" "+shaRE.ReplaceAllString(m.Text, "SHA"))
	}
	b, _ := json.Marshal(f.state())
	return strings.Join(res, " "), shaRE.ReplaceAllString(string(b), "SHA"), texts
}

func TestGitHubProblemsNeverChangeTheDeploy(t *testing.T) {
	base := newFixture(t, `on_failure = "rollback"`, "")
	wantRes, wantState, wantMsgs := scenario(t, base.fixtureRef())
	if len(wantMsgs) < 4 || !strings.Contains(wantRes, "applied") {
		t.Fatalf("the scenario did too little: %s %q", wantRes, wantMsgs)
	}
	for _, c := range []string{"down", "timeout", "401", "403", "404", "409", "422", "429", "500"} {
		t.Run(c, func(t *testing.T) {
			f := newGH(t, `on_failure = "rollback"`, "")
			switch c {
			case "down":
				f.srv.Close()
			case "timeout":
				setup := f.setup
				f.setup = func(e *Env) { setup(e); e.GitHubSendLimit = 300 * time.Millisecond }
				f.load()
				f.srv.Hang(t)
			default:
				var code int
				_, _ = fmt.Sscan(c, &code)
				f.srv.Answer = func(fakegh.Call) (int, string) { return code, `{"message": "GITHUB-SAYS"}` }
			}
			res, st, msgs := scenario(t, f.fixture)
			var notices []string
			kept := msgs[:0]
			for _, m := range msgs {
				if strings.Contains(m, "GitHub token") {
					notices = append(notices, m)
					continue
				}
				kept = append(kept, m)
			}
			if res != wantRes || st != wantState || strings.Join(kept, "\n") != strings.Join(wantMsgs, "\n") {
				t.Fatalf("not the same as without [github]:\n%s\n%s\n%s\nwant:\n%s\n%s\n%s", res, st, kept, wantRes, wantState, wantMsgs)
			}
			refused := c == "401" || c == "403" || c == "404"
			if refused != (len(notices) == 1) || len(notices) > 1 {
				t.Fatalf("token-refused notices: %q", notices)
			}
			if refused && !strings.Contains(notices[0], ghdeploy.RefusedText(name, "owner/config-repo")) {
				t.Fatal(notices[0])
			}
		})
	}
}

func (f *fixture) fixtureRef() *fixture { return f }

// The fake token never shows in the spool, the state, the queue, the
// log, pullgate status, or the environment of the commands.
func TestGitHubTokenNeverShows(t *testing.T) {
	f := newGH(t, `on_failure = "rollback"`, "")
	for _, s := range []string{"check.sh", "summary.sh", "apply.sh", "health.sh"} {
		f.prepend(s, "env >> "+f.ctrl+"/envdump")
	}
	f.srv.Answer = func(c fakegh.Call) (int, string) {
		if strings.HasSuffix(c.Path, "/statuses") {
			return 422, ""
		}
		return 401, ""
	}
	scenario(t, f.fixture)
	var out bytes.Buffer
	if err := f.env.Status(&out, StatusFlags{}); err != nil {
		t.Fatal(err)
	}
	f.srv.Answer = nil
	f.run()
	if err := f.env.Status(&out, StatusFlags{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.ctrl, "envdump")); len(b) == 0 || strings.Contains(string(b), f.token) {
		t.Fatal("the environment of the commands shows the token, or was not dumped")
	}
	if strings.Contains(out.String(), f.token) || strings.Contains(f.log.String(), f.token) {
		t.Fatal("status or the log shows the token")
	}
	for _, d := range []string{filepath.Join(f.base, "spool"), f.stateDir()} {
		_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || strings.HasPrefix(p, f.env.Dir.Repo()) {
				return nil
			}
			if b, _ := os.ReadFile(p); strings.Contains(string(b), f.token) {
				t.Errorf("%s shows the token", p)
			}
			return nil
		})
	}
}

func TestGitHubStatusLine(t *testing.T) {
	f := newGH(t, "", "")
	f.srv.Answer = func(fakegh.Call) (int, string) { return 422, `{"message": "GITHUB-SAYS"}` }
	f.applyNew("one")
	var out bytes.Buffer
	if err := f.env.Status(&out, StatusFlags{}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "github:") || !strings.Contains(s, "HTTP 422: GitHub did not take the post") || !strings.Contains(s, "0 posts wait") || strings.Contains(s, "GITHUB-SAYS") {
		t.Fatal(s)
	}
	f.down()
	f.applyNew("two")
	out.Reset()
	_ = f.env.Status(&out, StatusFlags{})
	if !strings.Contains(out.String(), "create, HTTP 503") || !strings.Contains(out.String(), "3 posts wait") {
		t.Fatal(out.String())
	}
	f.up()
	f.run()
	out.Reset()
	_ = f.env.Status(&out, StatusFlags{})
	if !strings.Contains(out.String(), "success, OK; 0 posts wait (environment example-host)") {
		t.Fatal(out.String())
	}
}

// A cut-off never goes to an older deployment of the same commit (a step
// that was not queued, for example while the queue could not be used).
func TestGitHubCutOffSkipsAnOlderDeployment(t *testing.T) {
	f := newGH(t, "", "")
	a := f.applyNew("one")
	b := f.commit("second", "two", nil)
	q := f.queue()
	k := q.Start(ghdeploy.RoleApply, b, ghdeploy.Target{API: f.srv.URL, Repository: "owner/config-repo", Environment: "example-host"}, ghdeploy.DescApplyStarted, f.now)
	q.Status(k, ghdeploy.Failure, ghdeploy.DescHeld, f.now)
	if err := q.Save(); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	st := f.state()
	st.InProgress = &state.InProgress{Phase: "apply", SHA: b, OldSHA: a, Started: f.now}
	if err := f.env.Dir.Save(st); err != nil {
		t.Fatal(err)
	}
	f.srv.Reset()
	if r := f.run(); r != CutOff {
		t.Fatal(r)
	}
	f.want(create(b), started(b), post("failure", b, ghdeploy.DescHeld))
}
