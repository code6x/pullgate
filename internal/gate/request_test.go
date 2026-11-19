package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

func (f *fixture) requestKind(kind string) {
	f.t.Helper()
	if err := f.env.Spool.Ensure(f.env.UID, f.env.Owners); err != nil {
		f.t.Fatal(err)
	}
	if err := f.env.Spool.WriteRequest(kind); err != nil {
		f.t.Fatal(err)
	}
}

func TestStatusRequest(t *testing.T) {
	f := newFixture(t, ``, "")
	a := f.applyNew("one")
	f.commit("second", "two", nil)
	f.requestKind(spool.RequestStatus)
	// /status never skips the normal run: a stream of them cannot stop
	// fetches.
	if r := f.run(); r != Asked {
		t.Fatalf("run: %s", r)
	}
	ms := f.outbox()
	if len(ms) != 2 || ms[0].Kind != spool.KindNotice || !strings.Contains(ms[0].Text, "applied:") ||
		!strings.Contains(ms[0].Text, a) || !strings.Contains(ms[0].Text, "paused:      no") || ms[1].Kind != spool.KindAsk {
		t.Fatalf("status notice, then the question: %+v", ms)
	}
	// A second one within a minute is dropped.
	f.requestKind(spool.RequestStatus)
	f.run()
	if len(f.outbox()) != 0 {
		t.Fatal("a second /status within a minute must get no answer")
	}
	// It works while paused, and shows why.
	f.now = f.now.Add(2 * time.Minute)
	if err := f.env.Dir.Pause("paused by the owner"); err != nil {
		t.Fatal(err)
	}
	f.requestKind(spool.RequestStatus)
	f.run()
	if !contains(f.outbox(), "paused:      yes: paused by the owner") {
		t.Fatal("the status must show the pause and its reason")
	}
}

func TestPauseRequest(t *testing.T) {
	f := newFixture(t, ``, "")
	f.applyNew("one")
	f.requestKind(spool.RequestPause)
	f.run()
	if !f.env.Dir.Paused() || !strings.HasPrefix(f.env.Dir.PauseReason(), state.TelegramPause+"2026-11-11 12:00") {
		t.Fatalf("not paused from Telegram: %q", f.env.Dir.PauseReason())
	}
	if !contains(f.outbox(), "is paused. Nothing is fetched") {
		t.Fatal("no reply")
	}
	f.commit("second", "two", nil)
	if r := f.run(); r != Paused {
		t.Fatalf("a paused instance does nothing: %s", r)
	}
}

func TestPauseRequestKeepsAnOtherPause(t *testing.T) {
	f := newFixture(t, ``, "")
	if err := f.env.Dir.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := f.env.Dir.Pause("the apply of x was cut off"); err != nil {
		t.Fatal(err)
	}
	f.requestKind(spool.RequestPause)
	f.run()
	if f.env.Dir.PauseReason() != "the apply of x was cut off" {
		t.Fatalf("a pause that is there must not change: %q", f.env.Dir.PauseReason())
	}
	if !contains(f.outbox(), "was paused already") {
		t.Fatal("no reply")
	}
}

// The listener can only write request files; a bad one pauses nothing.
func TestListenerCannotPauseByOtherFiles(t *testing.T) {
	for _, c := range []struct{ name, content string }{
		{"pause", "pause\nby a thief\n"},
		{"paused", "x\n"},
		{"state.json", `{"applied":""}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, ``, "")
			f.commit("first", "one", nil)
			f.request()
			if err := os.WriteFile(filepath.Join(f.env.Spool.CheckDir(), c.name), []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			f.run()
			if f.env.Dir.Paused() {
				t.Fatal("a bad request file must not pause")
			}
			if _, err := os.Lstat(filepath.Join(f.env.Spool.CheckDir(), c.name)); err == nil {
				t.Fatal("root must remove the bad file")
			}
		})
	}
}

func TestResumeLiftsOnlyATelegramPause(t *testing.T) {
	cases := map[string]func(f *fixture){
		"root --pause":   func(f *fixture) { _ = f.env.Status(new(strings.Builder), StatusFlags{Pause: true}) },
		"cut off":        func(f *fixture) { _ = f.env.Dir.Pause("the apply of x was cut off") },
		"planted prefix": func(f *fixture) { _ = f.env.Dir.Pause(state.TelegramPause + "2026-11-11 12:00 UTC and more") },
		"two lines":      func(f *fixture) { _ = f.env.Dir.Pause(state.TelegramPause + "2026-11-11 12:00 UTC\nx") },
		"telegram":       nil,
	}
	for name, pause := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, ``, "")
			f.applyNew("one")
			if pause == nil {
				f.requestKind(spool.RequestPause)
				f.run()
			} else {
				pause(f)
			}
			f.outbox()
			f.requestKind(spool.RequestResume)
			f.run()
			out := f.outbox()
			if pause == nil {
				if f.env.Dir.Paused() || !contains(out, "goes on") {
					t.Fatal("/resume must lift a /pause")
				}
				return
			}
			if !f.env.Dir.Paused() || !contains(out, "this pause was not set from Telegram") {
				t.Fatalf("/resume must not lift this pause: %+v", out)
			}
		})
	}
}

func TestResumeDoesNotEndBroken(t *testing.T) {
	f := newFixture(t, `on_failure = "rollback"`, "")
	breakIt(t, f)
	f.requestKind(spool.RequestPause)
	f.run()
	f.requestKind(spool.RequestResume)
	f.run()
	if f.env.Dir.Paused() || f.state().Broken == nil || !contains(f.outbox(), "still broken") {
		t.Fatal("/resume lifts the pause, but the broken state stays")
	}
}

func TestAskAgainOnlyExpiredOrLost(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		f := newFixture(t, ``, "")
		f.applyNew("one")
		f.commit("second", "two", nil)
		f.run()
		f.ask()
		f.now = f.now.Add(13 * time.Hour) // approval_ttl is 12h
		if r := f.run(); r != Nothing {
			t.Fatalf("after expiry: %s", r)
		}
		f.outbox()
		f.requestKind(spool.RequestAskAgain)
		if r := f.run(); r != Asked {
			t.Fatalf("/ask-again on an expired question: %s", r)
		}
	})
	t.Run("lost", func(t *testing.T) {
		f := newFixture(t, ``, "")
		f.applyNew("one")
		f.commit("second", "two", nil)
		f.run()
		old := f.ask() // as if it never reached the chat
		f.requestKind(spool.RequestAskAgain)
		if r := f.run(); r != Asked {
			t.Fatalf("/ask-again on an open question: %s", r)
		}
		if f.ask().Code == old.Code {
			t.Fatal("the new question needs a new code")
		}
	})
	for _, mark := range []string{"skipped", "failed", "held", "refused", "check-failed"} {
		t.Run(mark, func(t *testing.T) {
			f := newFixture(t, ``, "")
			f.applyNew("one")
			sha := f.commit("second", "two", nil)
			f.run()
			f.outbox()
			st := f.state()
			st.Questions = nil
			if mark == "held" {
				st.Held = sha
				st.SetMark(sha, state.MarkFailed, f.now)
			} else {
				st.SetMark(sha, mark, f.now)
			}
			if err := f.env.Dir.Save(st); err != nil {
				t.Fatal(err)
			}
			f.requestKind(spool.RequestAskAgain)
			if r := f.run(); r != Nothing {
				t.Fatalf("/ask-again on %s: %s", mark, r)
			}
			if !contains(f.outbox(), "Not asked again") {
				t.Fatal("no reply")
			}
		})
	}
}
