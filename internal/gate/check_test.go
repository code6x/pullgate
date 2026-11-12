package gate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/spool"
)

// request writes a check request, as the listener does on /check.
func (f *fixture) request() {
	f.t.Helper()
	if err := f.env.Spool.Ensure(f.env.UID, f.env.Owners); err != nil {
		f.t.Fatal(err)
	}
	if err := f.env.Spool.WriteCheck(); err != nil {
		f.t.Fatal(err)
	}
}

func TestCheckRequestStartsANormalRun(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	f.request()
	if r := f.run(); r != Asked {
		t.Fatalf("run: %s, want asked", r)
	}
	q := f.ask()
	if f.env.Spool.HasCheck() {
		t.Fatal("root must remove the request")
	}
	// No new commit: a reply says so.
	f.now = f.now.Add(2 * time.Minute)
	f.request()
	if r := f.run(); r != Nothing {
		t.Fatalf("run: %s", r)
	}
	if !contains(f.outbox(), "is still open") {
		t.Fatal("the reply must say that the question is still open")
	}
	f.tap(q.Code, "apply")
	if r := f.apply(); r != Applied {
		t.Fatalf("apply: %s", r)
	}
	f.outbox()
	f.now = f.now.Add(2 * time.Minute)
	f.request()
	if r := f.run(); r != Nothing {
		t.Fatalf("run: %s", r)
	}
	if !contains(f.outbox(), "No new commit. The applied commit is <code>"+f.state().Applied+"</code>") {
		t.Fatal("the reply must name the applied SHA")
	}
}

func TestCheckRequestNeverAppliesWithoutTap(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	for i := range 3 {
		f.request()
		f.run()
		f.apply()
		f.now = f.now.Add(time.Duration(i+2) * time.Minute)
	}
	if st := f.state(); st.Applied != "" || st.Open() == nil {
		t.Fatalf("mode ask: a check request must only ask, never apply: %+v", st)
	}
	if f.read("log") != "" {
		t.Fatal("the apply command must not run without a tap")
	}
	// A check request put in the inbox is a bad approval, not a tap.
	if err := os.WriteFile(filepath.Join(f.env.Spool.Inbox(), "1.json"), []byte(spool.CheckContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := f.apply(); r == Applied || f.state().Applied != "" {
		t.Fatalf("apply: %s", r)
	}
}

func TestCheckRateLimitByRoot(t *testing.T) {
	f := newFixture(t, ``, "")
	a := f.commit("first", "one", nil)
	f.request()
	if r := f.run(); r != Asked {
		t.Fatalf("run: %s", r)
	}
	f.outbox()
	b := f.commit("second", "two", nil)
	f.now = f.now.Add(30 * time.Second)
	f.request()
	if r := f.run(); r != CheckLimited {
		t.Fatalf("a second request in a minute must be dropped: %s", r)
	}
	if f.state().LastTip != a {
		t.Fatal("a dropped request must not fetch")
	}
	if ms := f.outbox(); len(ms) != 1 || !contains(ms, "was dropped") {
		t.Fatalf("one reply for the drop: %+v", ms)
	}
	f.now = f.now.Add(10 * time.Second)
	f.request()
	if r := f.run(); r != CheckLimited {
		t.Fatalf("run: %s", r)
	}
	if len(f.outbox()) != 0 {
		t.Fatal("only one reply for drops in a minute")
	}
	// A clock that goes back does not pass the limit.
	f.now = f.now.Add(-50 * time.Second)
	f.request()
	if r := f.run(); r != CheckLimited {
		t.Fatalf("run: %s", r)
	}
	f.now = f.now.Add(2 * time.Minute)
	f.request()
	if r := f.run(); r != Asked || f.state().LastTip != b {
		t.Fatalf("a minute later the request runs: %s", r)
	}
}

// TestCheckBadRequestFiles: a file of another owner needs root to make;
// spool.TestBadCheckRequests covers it ("wrong owner").
func TestCheckBadRequestFiles(t *testing.T) {
	cases := map[string]func(f *fixture){
		"symlink": func(f *fixture) {
			if err := os.Symlink(f.env.Dir.StateFile(), filepath.Join(f.env.Spool.CheckDir(), spool.CheckFile)); err != nil {
				f.t.Fatal(err)
			}
		},
		"too big": func(f *fixture) {
			if err := os.WriteFile(filepath.Join(f.env.Spool.CheckDir(), spool.CheckFile), make([]byte, 100), 0o600); err != nil {
				f.t.Fatal(err)
			}
		},
	}
	for n, c := range cases {
		t.Run(n, func(t *testing.T) {
			f := newFixture(t, ``, "")
			f.commit("first", "one", nil)
			if err := f.env.Spool.Ensure(f.env.UID, f.env.Owners); err != nil {
				t.Fatal(err)
			}
			c(f)
			// A timer run that finds only bad files removes them and
			// goes on as a normal run, with no reply.
			if r := f.run(); r != Asked {
				t.Fatalf("the run must go on as a normal run: %s", r)
			}
			if left, _ := os.ReadDir(f.env.Spool.CheckDir()); len(left) != 0 {
				t.Fatal("a bad request must be removed")
			}
			if st := f.state(); !st.CheckAt.IsZero() {
				t.Fatal("a bad request must not count as a request")
			}
			for _, m := range f.outbox() {
				if m.Kind != spool.KindAsk {
					t.Fatalf("a bad request gets no reply: %+v", m)
				}
			}
		})
	}
}

func TestCheckPausedAndOff(t *testing.T) {
	for _, mode := range []string{"paused", "off"} {
		t.Run(mode, func(t *testing.T) {
			top := ``
			if mode == "off" {
				top = `mode = "off"`
			}
			f := newFixture(t, top, "")
			f.commit("first", "one", nil)
			if mode == "paused" {
				if err := f.env.Dir.Ensure(); err != nil {
					t.Fatal(err)
				}
				if err := f.env.Dir.Pause("test"); err != nil {
					t.Fatal(err)
				}
			}
			f.request()
			r := f.run()
			if string(r) != mode {
				t.Fatalf("run: %s", r)
			}
			if !contains(f.outbox(), "nothing was fetched") {
				t.Fatal("the reply must say so")
			}
			if st := f.state(); st.LastTip != "" || len(st.Questions) != 0 {
				t.Fatalf("a paused or off instance must not fetch: %+v", st)
			}
			if f.env.Spool.HasCheck() {
				t.Fatal("the request must be removed")
			}
		})
	}
}

func TestCheckWaitsForTheLock(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	if err := f.env.Dir.Ensure(); err != nil {
		t.Fatal(err)
	}
	l, err := f.env.Dir.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	f.request()
	done := make(chan Result, 1)
	go func() {
		r, err := f.env.Run(context.Background())
		if err != nil {
			r = Result("error: " + err.Error())
		}
		done <- r
	}()
	select {
	case r := <-done:
		t.Fatalf("the run must wait while the lock is held: %s", r)
	case <-time.After(300 * time.Millisecond):
	}
	if st := f.state(); st.LastTip != "" {
		t.Fatal("nothing may run while the lock is held")
	}
	_ = l.Close()
	if r := <-done; r != Asked {
		t.Fatalf("after the lock is free the request runs once: %s", r)
	}
}

func TestCheckAutoModeApplies(t *testing.T) {
	// Decision (DESIGN.md section 13): /check on an auto instance runs as
	// the timer does, so a new commit is applied without a tap.
	f := newFixture(t, `mode = "auto"`, "")
	sha := f.commit("first", "one", nil)
	f.request()
	if r := f.run(); r != AutoQueued {
		t.Fatalf("run: %s", r)
	}
	if !contains(f.outbox(), "applied now, without a tap") {
		t.Fatal("the reply must say that auto mode applies it")
	}
	if r := f.apply(); r != Applied || f.state().Applied != sha {
		t.Fatalf("apply: %s", r)
	}
}

func TestCheckReplyForRefusedTip(t *testing.T) {
	f := newFixture(t, ``, "")
	f.applyNew("one")
	f.r.Git("checkout", "--quiet", "--orphan", "other")
	f.commit("rewritten", "evil", nil) // not a descendant
	if r := f.run(); r != Refused {
		t.Fatalf("run: %s", r)
	}
	f.outbox()
	f.now = f.now.Add(2 * time.Minute)
	f.request()
	if r := f.run(); r != Nothing {
		t.Fatalf("run: %s", r)
	}
	if ms := f.outbox(); !contains(ms, "allow_older = true") || contains(ms, "--ask-again") {
		t.Fatalf("the reply must point to allow_older: %+v", ms)
	}
}

func TestCheckLimitReplySentOnce(t *testing.T) {
	f := newFixture(t, ``, "[limits]\nquestions_per_day = 1\n")
	f.commit("first", "one", nil)
	f.run()
	f.outbox()
	f.commit("second", "two", nil)
	f.request()
	if r := f.run(); r != LimitReached {
		t.Fatalf("run: %s", r)
	}
	n := 0
	for _, m := range f.outbox() {
		if strings.Contains(m.Text, "questions_per_day") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("one message about the limit, got %d", n)
	}
}

// TestCheckWaiterStopsWhenRequestIsGone: a run that waited for the lock and
// then finds no request (another run took it) stops: no fetch, no reply.
func TestCheckWaiterStopsWhenRequestIsGone(t *testing.T) {
	f := newFixture(t, ``, "")
	f.commit("first", "one", nil)
	if err := f.env.Dir.Ensure(); err != nil {
		t.Fatal(err)
	}
	l, err := f.env.Dir.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	f.request()
	done := make(chan Result, 1)
	go func() {
		r, err := f.env.Run(context.Background())
		if err != nil {
			r = Result("error: " + err.Error())
		}
		done <- r
	}()
	select {
	case r := <-done:
		t.Fatalf("the run must wait while the lock is held: %s", r)
	case <-time.After(300 * time.Millisecond):
	}
	// The run that holds the lock takes the request.
	if n, _, err := f.env.Spool.TakeChecks(os.Getuid()); err != nil || n != 1 {
		t.Fatalf("TakeChecks: %d, %v", n, err)
	}
	_ = l.Close()
	if r := <-done; r != Busy {
		t.Fatalf("a waiter without a request must stop as busy: %s", r)
	}
	if st := f.state(); st.LastTip != "" {
		t.Fatal("the waiter must not fetch")
	}
	if out := f.outbox(); len(out) != 0 {
		t.Fatalf("the waiter must not reply: %d messages", len(out))
	}
}
