package bootstrap

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/code6x/pullgate/internal/testgit"
)

// setupFx is a bootstrap with steps 1 and 5, with fakes for root,
// useradd, systemd and doctor. The test user plays root and the listener.
type setupFx struct {
	*fx
	mu      sync.Mutex
	users   map[string]bool
	added   []string
	calls   [][]string
	doctors []bool
	problem map[bool]int // doctor problems, by beforeEnable
}

func newSetupFx(t *testing.T) *setupFx {
	t.Helper()
	f := &setupFx{fx: newFx(t), problem: map[bool]int{}}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	f.users = map[string]bool{u.Username: true}
	base := testgit.PrivateDir(t)
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "units"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.o.Setup = &Setup{
		StateDir: filepath.Join(base, "state"), SpoolDir: filepath.Join(base, "spool"), UnitDir: filepath.Join(base, "units"),
		UID:          uint32(os.Geteuid()),
		Units:        map[string]string{"pullgate-listen.service": "listen\n", "pullgate-run@.timer": "timer\n"},
		Enable:       []string{"pullgate-listen.service", "pullgate-run@NAME.timer"},
		ListenerUser: u.Username,
		UserExists: func(n string) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.users[n]
		},
		AddUser: func(_ context.Context, n string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.added = append(f.added, n)
			f.users[n] = true
			return nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, args)
			return "", nil
		},
		Doctor: func(_ context.Context, before bool) int {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.doctors = append(f.doctors, before)
			return f.problem[before]
		},
	}
	return f
}

func (f *setupFx) pair() error {
	f.t.Helper()
	code, done := f.start()
	f.tg.Say(owner, owner, code, false)
	return wait(f.t, done)
}

func (f *setupFx) enabled() bool {
	for _, c := range f.calls {
		if c[0] == "enable" {
			return true
		}
	}
	return false
}

// Step 1 and step 5: the users, the directories, the units, doctor, and
// the enable.
func TestSetupMakesUsersDirectoriesUnitsAndEnables(t *testing.T) {
	f := newSetupFx(t)
	if err := f.pair(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	if !slices.Equal(f.added, []string{"pullgate-check-config-repo"}) {
		t.Fatalf("users made: %v", f.added)
	}
	s := f.o.Setup
	for p, m := range map[string]os.FileMode{f.etc: 0o755, s.StateDir: 0o711, s.SpoolDir: 0o755, filepath.Join(s.StateDir, "config-repo"): 0o711, filepath.Join(s.SpoolDir, "config-repo", "outbox"): 0o770} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != m {
			t.Fatalf("%s: %v %v, want %o", p, fi, err, m)
		}
	}
	for n, body := range s.Units {
		if b, err := os.ReadFile(filepath.Join(s.UnitDir, n)); err != nil || string(b) != body {
			t.Fatalf("unit %s: %q %v", n, b, err)
		}
	}
	want := [][]string{{"daemon-reload"}, {"enable", "--now", "pullgate-listen.service", "pullgate-run@config-repo.timer"}, {"restart", "pullgate-listen.service"}}
	if len(f.calls) != 3 || !slices.Equal(f.calls[0], want[0]) || !slices.Equal(f.calls[1], want[1]) || !slices.Equal(f.calls[2], want[2]) {
		t.Fatalf("systemctl calls: %v", f.calls)
	}
	if !slices.Equal(f.doctors, []bool{true, false}) {
		t.Fatalf("doctor runs: %v", f.doctors)
	}
	if strings.Contains(f.out.String(), token) {
		t.Fatal("the token was printed")
	}
}

// A failed doctor enables nothing.
func TestFailedDoctorEnablesNothing(t *testing.T) {
	f := newSetupFx(t)
	f.problem[true] = 2
	err := f.pair()
	if err == nil || !strings.Contains(err.Error(), "no unit was enabled") {
		t.Fatalf("bootstrap: %v", err)
	}
	if f.enabled() {
		t.Fatalf("units were enabled: %v", f.calls)
	}
	if !slices.Equal(f.doctors, []bool{true}) {
		t.Fatalf("doctor runs: %v", f.doctors)
	}
}

// Doctor after the enable finds a problem: bootstrap fails and says so.
func TestDoctorAfterEnable(t *testing.T) {
	f := newSetupFx(t)
	f.problem[false] = 1
	if err := f.pair(); err == nil || !strings.Contains(err.Error(), "the units are enabled, but") {
		t.Fatalf("bootstrap: %v", err)
	}
}

// A check user that is the listener user is refused before anything is
// asked or written.
func TestSetupRefusesCheckUserListener(t *testing.T) {
	f := newSetupFx(t)
	if err := os.WriteFile(filepath.Join(f.etc, "config-repo.toml"), []byte("[commands]\ncheck_user = \""+f.o.Setup.ListenerUser+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), f.o)
	if err == nil || !strings.Contains(err.Error(), "check_user is the listener user") {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(f.added) != 0 || len(f.calls) != 0 || len(f.tg.Messages()) != 0 {
		t.Fatalf("something was done: %v %v", f.added, f.calls)
	}
}

// useradd fails: bootstrap stops with the fix, before the token.
func TestSetupUseraddFails(t *testing.T) {
	f := newSetupFx(t)
	f.o.Setup.AddUser = func(context.Context, string) error { return errors.New("exit status 9") }
	err := Run(context.Background(), f.o)
	if err == nil || !strings.Contains(err.Error(), "make the user pullgate-check-config-repo") || !strings.Contains(err.Error(), "Fix") {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.etc, "listener.toml")); err == nil {
		t.Fatal("listener.toml was written")
	}
}

// A unit that is already the same is not written again; a changed one is
// replaced with the unit of this version.
func TestSetupUnitsReplaced(t *testing.T) {
	f := newSetupFx(t)
	p := filepath.Join(f.o.Setup.UnitDir, "pullgate-run@.timer")
	if err := os.WriteFile(p, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.pair(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	if b, _ := os.ReadFile(p); string(b) != "timer\n" {
		t.Fatalf("the unit was not replaced: %q", b)
	}
	if !strings.Contains(f.out.String(), "Units: 2 of 2 written") {
		t.Fatalf("output:\n%s", f.out.String())
	}
}

// A new host: the config directory does not exist yet. Bootstrap checks
// the directories above it, and makes it.
func TestSetupMakesTheConfigDir(t *testing.T) {
	f := newSetupFx(t)
	f.etc = filepath.Join(f.etc, "pullgate")
	f.o.EtcDir = f.etc
	if err := f.pair(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	if fi, err := os.Stat(f.etc); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("%s: %v %v", f.etc, fi, err)
	}
}

// A base directory of another user is refused, not taken over.
func TestSetupRefusesADirOfAnotherUser(t *testing.T) {
	f := newSetupFx(t)
	f.o.Setup.UID++ // the test user plays "another user"
	err := Run(context.Background(), f.o)
	if err == nil || !strings.Contains(err.Error(), "is not owned by uid") {
		t.Fatalf("bootstrap: %v", err)
	}
}
