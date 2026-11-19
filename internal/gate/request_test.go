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
	f.commit("second", "two", nil) // not fetched: /status does not fetch
	f.requestKind(spool.RequestStatus)
	if r := f.run(); r != Nothing {
		t.Fatalf("run: %s", r)
	}
	ms := f.outbox()
	if len(ms) != 1 || ms[0].Kind != spool.KindNotice || !strings.Contains(ms[0].Text, "applied:") ||
		!strings.Contains(ms[0].Text, a) || !strings.Contains(ms[0].Text, "paused:      no") {
		t.Fatalf("status notice: %+v", ms)
	}
	if f.state().LastTip != a {
		t.Fatal("/status must not fetch")
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
