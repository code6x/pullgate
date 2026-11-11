package gate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
	"github.com/code6x/pullgate/internal/testgit"
)

func TestMain(m *testing.M) {
	syscall.Umask(0o077)
	os.Exit(m.Run())
}

type fixture struct {
	t    *testing.T
	r    *testgit.Remote
	base string
	ctrl string // files that the test scripts read and write
	cfg  string // the config dir
	env  *Env
	now  time.Time
}

const name = "config-repo"

// newFixture makes a remote, scripts, a config and an Env. extra is TOML
// added to the config: top-level keys first, then sections.
func newFixture(t *testing.T, top, sections string) *fixture {
	t.Helper()
	testgit.NeedGit(t)
	f := &fixture{t: t, r: testgit.New(t), base: testgit.PrivateDir(t), now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	f.ctrl = filepath.Join(f.base, "ctrl")
	f.cfg = filepath.Join(f.base, "etc")
	for _, d := range []string{f.ctrl, f.cfg, filepath.Join(f.base, "bin"), filepath.Join(f.base, "state"), filepath.Join(f.base, "spool")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c := f.ctrl
	f.script("apply.sh", `echo "apply $PULLGATE_SHA old=$PULLGATE_OLD_SHA rb=$PULLGATE_ROLLBACK failed=$PULLGATE_FAILED_SHA" >> `+c+`/log
if [ "$PULLGATE_ROLLBACK" = 1 ] && [ -e `+c+`/fail-rollback ]; then exit 3; fi
if [ -e "$PULLGATE_TREE/fail-apply" ]; then exit 2; fi
tr '\0' '\n' < "$PULLGATE_CHANGED_FILES" > `+c+`/changed
cat "$PULLGATE_TREE/value" > `+c+`/live
`)
	f.script("health.sh", `if [ -e "$PULLGATE_TREE/fail-health" ]; then exit 1; fi
`)
	f.script("check.sh", `id -u > `+c+`/check-uid
echo x >> `+c+`/check-count
if [ -e "$PULLGATE_TREE/fail-check" ]; then echo "bad config <b>"; exit 1; fi
`)
	f.script("summary.sh", `id -u > `+c+`/summary-uid
echo "value $(cat "$PULLGATE_TREE/value")"
if [ -n "$PULLGATE_OLD_TREE" ]; then echo "old value $(cat "$PULLGATE_OLD_TREE/value")"; fi
`)
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(f.base, "bin")
	toml := fmt.Sprintf(`host = "example-host"
%s
[repo]
url = %q
branch = "main"

[commands]
check = [%q]
summary = [%q]
apply = [%q]
health = [%q]
check_user = %q

[listener]
user = %q

[watch]
paths = ["policy/**"]
%s
`, top, f.r.Bare, b+"/check.sh", b+"/summary.sh", b+"/apply.sh", b+"/health.sh", u.Username, u.Username, sections)
	if err := os.WriteFile(filepath.Join(f.cfg, name+".toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	f.load()
	return f
}

func (f *fixture) load() {
	f.t.Helper()
	uid := uint32(os.Geteuid())
	cfg, err := config.Load(f.cfg, name, uid)
	if err != nil {
		f.t.Fatal(err)
	}
	env, err := New(cfg, filepath.Join(f.base, "state"), filepath.Join(f.base, "spool"), uid, slog.New(slog.NewTextHandler(io.Discard, nil)), io.Discard)
	if err != nil {
		f.t.Fatal(err)
	}
	env.Now = func() time.Time { return f.now }
	f.env = env
}

func (f *fixture) script(n, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.base, "bin", n), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) commit(subject, value string, more map[string]string) string {
	f.t.Helper()
	files := map[string]string{"value": value + "\n"}
	for k, v := range more {
		files[k] = v
	}
	return f.r.Commit(subject, files)
}

func (f *fixture) run() Result {
	f.t.Helper()
	res, err := f.env.Run(context.Background())
	if err != nil {
		f.t.Fatalf("run: %v", err)
	}
	return res
}

func (f *fixture) apply() Result {
	f.t.Helper()
	res, err := f.env.Apply(context.Background())
	if err != nil {
		f.t.Fatalf("apply: %v", err)
	}
	return res
}

// outbox reads and removes the outbox, as the listener would.
func (f *fixture) outbox() []spool.Message {
	f.t.Helper()
	files, err := f.env.Spool.ReadOutbox()
	if err != nil {
		f.t.Fatal(err)
	}
	var out []spool.Message
	for _, of := range files {
		if of.Err != nil {
			f.t.Fatal(of.Err)
		}
		out = append(out, of.Message)
		if err := f.env.Spool.Done(of.Name); err != nil {
			f.t.Fatal(err)
		}
	}
	return out
}

// ask returns the one ask message in the outbox (and drops the rest).
func (f *fixture) ask() spool.Message {
	f.t.Helper()
	var asks []spool.Message
	for _, m := range f.outbox() {
		if m.Kind == spool.KindAsk {
			asks = append(asks, m)
		}
	}
	if len(asks) != 1 {
		f.t.Fatalf("want one question, got %d", len(asks))
	}
	return asks[0]
}

func (f *fixture) tap(code, action string) {
	f.t.Helper()
	if err := f.env.Spool.WriteApproval(spool.Approval{Code: code, Action: action}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) state() *state.State {
	f.t.Helper()
	st, err := f.env.Dir.Load()
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

func (f *fixture) read(n string) string {
	b, _ := os.ReadFile(filepath.Join(f.ctrl, n))
	return strings.TrimSpace(string(b))
}

func (f *fixture) touch(n string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.ctrl, n), nil, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// applyNew commits a value, asks, taps Apply and applies it.
func (f *fixture) applyNew(value string) string {
	f.t.Helper()
	sha := f.commit("set "+value, value, nil)
	if r := f.run(); r != Asked {
		f.t.Fatalf("run: %s, want asked", r)
	}
	f.tap(f.ask().Code, "apply")
	if r := f.apply(); r != Applied {
		f.t.Fatalf("apply: %s, want applied", r)
	}
	f.outbox()
	return sha
}

func contains(ms []spool.Message, s string) bool {
	for _, m := range ms {
		if strings.Contains(m.Text, s) {
			return true
		}
	}
	return false
}
