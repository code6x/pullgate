package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
	"github.com/code6x/pullgate/internal/telegram/faketg"
	"github.com/code6x/pullgate/internal/testgit"
)

const (
	name  = "config-repo"
	token = "123:test-token"
)

// fix is a host where every check passes. The test user plays root, the
// listener user and the check user.
type fix struct {
	t       *testing.T
	o       *Options
	base    string
	out     strings.Builder
	tg      *faketg.Server
	enabled map[string]string // systemctl is-enabled answers
	active  string
	readErr error
}

func TestMain(m *testing.M) {
	syscall.Umask(0o022)
	os.Exit(m.Run())
}

func newFix(t *testing.T) *fix {
	t.Helper()
	testgit.NeedGit(t)
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	f := &fix{t: t, base: testgit.PrivateDir(t), tg: faketg.New(t, token), active: "active"}
	r := testgit.New(t)
	r.Commit("first", map[string]string{"value": "1\n"})
	b := f.base
	for _, d := range []string{"etc", "units", "bin"} {
		if err := os.Mkdir(filepath.Join(b, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Chmod(filepath.Join(b, "etc"), 0o755)
	for _, d := range []string{"state", "spool"} {
		if err := os.Mkdir(filepath.Join(b, d), map[string]os.FileMode{"state": 0o711, "spool": 0o755}[d]); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Chmod(filepath.Join(b, "state"), 0o711)
	f.write("bin/apply.sh", "#!/bin/sh\nexit 0\n", 0o700)
	f.write("etc/bot-token", token+"\n", 0o600)
	f.write("etc/listener.toml", fmt.Sprintf(`token_file = %q
owner_id = 42
chat_id = 42
instances = [%q]
spool_dir = %q
state_file = %q
api_url = %q

[updates]
listener_user = %q
`, filepath.Join(b, "etc/bot-token"), name, filepath.Join(b, "spool"), filepath.Join(b, "listener.json"), f.tg.URL, u.Username), 0o600)
	f.write("etc/"+name+".toml", fmt.Sprintf(`[repo]
url = %q
branch = "main"

[commands]
apply = [%q]
check_user = %q

[listener]
user = %q
`, r.Bare, filepath.Join(b, "bin/apply.sh"), u.Username, u.Username), 0o600)
	f.write("units/pullgate-listen.service", "listen\n", 0o644)
	f.write("units/pullgate-run@.timer", "timer\n", 0o644)
	f.enabled = map[string]string{"pullgate-listen.service": "enabled", "pullgate-run@" + name + ".timer": "enabled", "pullgate-update.timer": "enabled"}
	uid := uint32(os.Geteuid())
	// What the first run makes.
	sp := spool.Dirs{Base: filepath.Join(b, "spool"), Name: name}
	if err := sp.Ensure(uid, spool.Owners{ListenerUID: os.Geteuid(), ListenerGID: os.Getegid()}); err != nil {
		t.Fatal(err)
	}
	sd := state.Dir{Path: filepath.Join(b, "state", name)}
	if err := sd.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct {
		p string
		m os.FileMode
	}{{sd.Repo(), 0o700}, {sd.Trees(), 0o711}} {
		if err := os.MkdirAll(d.p, d.m); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(d.p, d.m)
	}
	if err := sd.Save(&state.State{}); err != nil {
		t.Fatal(err)
	}
	f.o = &Options{
		Version: "1.2.3", ConfigDir: filepath.Join(b, "etc"), StateDir: filepath.Join(b, "state"), SpoolDir: filepath.Join(b, "spool"),
		UnitDir: filepath.Join(b, "units"), Listener: filepath.Join(b, "etc/listener.toml"), UID: uid,
		Units:  map[string]string{"pullgate-listen.service": "listen\n", "pullgate-run@.timer": "timer\n"},
		Enable: []string{"pullgate-listen.service", "pullgate-run@NAME.timer", "pullgate-update.timer"},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			switch args[0] {
			case "is-enabled":
				if s, ok := f.enabled[args[1]]; ok {
					return s, nil
				}
				return "disabled", errors.New("exit status 1")
			case "is-active":
				return f.active, nil
			}
			return "", errors.New("unknown")
		},
		ReadAs: func(_ context.Context, uid int, gids []int, path string) error {
			if f.readErr != nil {
				return f.readErr
			}
			if uid != os.Geteuid() || len(gids) == 0 || gids[0] != os.Getegid() {
				return fmt.Errorf("wrong user %d %v", uid, gids)
			}
			return ReadOne(path)
		},
		Out: &f.out,
	}
	return f
}

func (f *fix) write(p, body string, mode os.FileMode) {
	f.t.Helper()
	p = filepath.Join(f.base, p)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fix) run() int {
	f.t.Helper()
	f.out.Reset()
	return Run(context.Background(), f.o)
}

func TestAllOK(t *testing.T) {
	f := newFix(t)
	if n := f.run(); n != 0 {
		t.Fatalf("%d problems:\n%s", n, f.out.String())
	}
	out := f.out.String()
	for _, w := range []string{"All checks passed", "the listener user can read the outbox", "git can read the branch main", "as read back, are right"} {
		if !strings.Contains(out, w) {
			t.Fatalf("no %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, token) {
		t.Fatal("the output shows the token")
	}
	// The test file is gone again.
	if left, _ := os.ReadDir(filepath.Join(f.o.SpoolDir, name, "outbox")); len(left) != 0 {
		t.Fatalf("left in the outbox: %v", left)
	}
}

func TestTelegramOK(t *testing.T) {
	f := newFix(t)
	f.o.Telegram = true
	if n := f.run(); n != 0 || !strings.Contains(f.out.String(), "the token works") || !strings.Contains(f.out.String(), "the bot can reach the chat") {
		t.Fatalf("%d problems:\n%s", n, f.out.String())
	}
}

// Each failing check: a problem with the fix in its line, and exit != 0.
func TestEachProblem(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(f *fix)
		want string
	}{
		{"unit missing", func(f *fix) { _ = os.Remove(filepath.Join(f.o.UnitDir, "pullgate-run@.timer")) }, "pullgate-run@.timer: it is not installed"},
		{"required unit not enabled", func(f *fix) { delete(f.enabled, "pullgate-run@"+name+".timer") }, "systemctl enable --now pullgate-run@" + name + ".timer"},
		{"listener not active", func(f *fix) { f.active = "failed" }, "is-active says \"failed\""},
		{"listener config writable by others", func(f *fix) { _ = os.Chmod(f.o.Listener, 0o666) }, "PROBLEM  listener config"},
		{"listener config not valid", func(f *fix) { f.write("etc/listener.toml", "owner_id = 1\n", 0o600) }, "PROBLEM  listener config"},
		{"listener config without the instance", func(f *fix) { f.o.Name = "other" }, "does not list the instance other"},
		{"listener config with another spool", func(f *fix) {
			b, _ := os.ReadFile(f.o.Listener)
			f.write("etc/listener.toml", strings.Replace(string(b), filepath.Join(f.base, "spool"), filepath.Join(f.base, "other"), 1), 0o600)
		}, "spool_dir of"},
		{"token readable by others", func(f *fix) { _ = os.Chmod(filepath.Join(f.base, "etc/bot-token"), 0o644) }, "PROBLEM  bot token"},
		{"telegram token refused", func(f *fix) {
			f.o.Telegram = true
			f.write("etc/bot-token", "999:wrong\n", 0o600)
		}, "getMe failed"},
		{"telegram chat not found", func(f *fix) { f.o.Telegram = true; f.tg.NoChat = true }, "getChat failed"},
		{"instance config writable by others", func(f *fix) { _ = os.Chmod(filepath.Join(f.o.ConfigDir, name+".toml"), 0o666) }, "PROBLEM  instance " + name},
		{"command writable by others", func(f *fix) { _ = os.Chmod(filepath.Join(f.base, "bin/apply.sh"), 0o777) }, "PROBLEM  instance " + name},
		{"command missing", func(f *fix) { _ = os.Remove(filepath.Join(f.base, "bin/apply.sh")) }, "PROBLEM  instance " + name},
		{"check user missing", func(f *fix) {
			p := filepath.Join(f.o.ConfigDir, name+".toml")
			b, _ := os.ReadFile(p)
			u, _ := user.Current()
			f.write("etc/"+name+".toml", strings.Replace(string(b), "check_user = \""+u.Username+"\"", "check_user = \"no-such-user-x\"", 1), 0o600)
		}, "no-such-user-x"},
		{"outbox mode changed", func(f *fix) { _ = os.Chmod(filepath.Join(f.o.SpoolDir, name, "outbox"), os.ModeSetgid|0o770) }, "outbox has owner"},
		{"inbox mode changed", func(f *fix) { _ = os.Chmod(filepath.Join(f.o.SpoolDir, name, "inbox"), 0o755) }, "inbox has owner"},
		{"state dir mode changed", func(f *fix) { _ = os.Chmod(filepath.Join(f.o.StateDir, name), 0o755) }, "systemctl start pullgate-run@" + name + ".service"},
		{"listener cannot read the outbox", func(f *fix) { f.readErr = errors.New("permission denied") }, "the listener user cannot read a file in"},
		{"config dir writable by others", func(f *fix) { _ = os.Chmod(f.o.ConfigDir, 0o775) }, "PROBLEM  listener config"},
		{"before enable: no unit is checked", func(f *fix) {
			f.o.BeforeEnable = true
			_ = os.Remove(filepath.Join(f.o.UnitDir, "pullgate-run@.timer"))
		}, "pullgate-run@.timer: it is not installed"},
		{"check user is the listener user", func(f *fix) { f.o.asRoot = true }, "is the listener user or has its group"},
		{"listener cannot read its config", func(f *fix) { f.readErr = errors.New("permission denied") }, "cannot be read by the listener user"},
		{"state file is not a regular file", func(f *fix) {
			p := filepath.Join(f.o.StateDir, name, "state.json")
			_ = os.Remove(p)
			_ = syscall.Mkfifo(p, 0o600)
		}, "is a p"},
		{"git cannot read the repo", func(f *fix) {
			p := filepath.Join(f.o.ConfigDir, name+".toml")
			b, _ := os.ReadFile(p)
			f.write("etc/"+name+".toml", strings.Replace(string(b), "branch = \"main\"", "branch = \"nope\"", 1), 0o600)
		}, "git cannot read the branch nope"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFix(t)
			c.set(f)
			n := f.run()
			out := f.out.String()
			if n == 0 || !strings.Contains(out, c.want) || !strings.Contains(out, "problem(s)") {
				t.Fatalf("problems %d, want %q:\n%s", n, c.want, out)
			}
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, "PROBLEM") && !strings.Contains(l, "Fix") {
					t.Fatalf("a problem without a fix: %s", l)
				}
			}
			if strings.Contains(out, "wrong\n") || strings.Contains(out, token) {
				t.Fatal("the output shows a token")
			}
		})
	}
}

// A note is not a problem: a changed unit, an optional unit that is not
// enabled, a directory that the first run has not made yet.
func TestNotesAreNotProblems(t *testing.T) {
	f := newFix(t)
	f.write("units/pullgate-listen.service", "changed\n", 0o644)
	delete(f.enabled, "pullgate-update.timer")
	_ = os.RemoveAll(filepath.Join(f.o.StateDir, name, "trees"))
	if n := f.run(); n != 0 {
		t.Fatalf("%d problems:\n%s", n, f.out.String())
	}
	out := f.out.String()
	for _, w := range []string{"pullgate-listen.service: it differs", "pullgate-update.timer: not enabled (optional", "not made yet: " + filepath.Join(f.o.StateDir, name, "trees")} {
		if !strings.Contains(out, w) {
			t.Fatalf("no %q:\n%s", w, out)
		}
	}
}

// token_file outside the config dir: doctor checks that file, not
// ConfigDir/bot-token (no CREDENTIALS_DIRECTORY under sudo).
func TestTokenFileOfTheConfig(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	f := newFix(t)
	f.o.Telegram = true
	other := filepath.Join(f.base, "token")
	f.write("token", token+"\n", 0o600)
	b, _ := os.ReadFile(f.o.Listener)
	f.write("etc/listener.toml", strings.Replace(string(b), filepath.Join(f.base, "etc/bot-token"), other, 1), 0o600)
	f.write("etc/bot-token", "999:old\n", 0o600) // an old file that the listener does not use
	if n := f.run(); n != 0 || !strings.Contains(f.out.String(), "bot token: "+other) {
		t.Fatalf("%d problems:\n%s", n, f.out.String())
	}
}

// Before enable: the enabled and active checks are left out, with a note.
func TestBeforeEnable(t *testing.T) {
	f := newFix(t)
	f.o.BeforeEnable = true
	f.enabled = map[string]string{}
	f.active = "inactive"
	if n := f.run(); n != 0 || !strings.Contains(f.out.String(), "bootstrap enables them") {
		t.Fatalf("%d problems:\n%s", n, f.out.String())
	}
}
