package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"

	"github.com/code6x/pullgate/internal/doctor"
	"github.com/code6x/pullgate/internal/gate"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

// Setup is what steps 1 and 5 of bootstrap need (DESIGN.md section 17):
// the users, the directories and the units, then pullgate doctor and the
// enable. cmd/pullgate fills it; tests give fakes for root and systemd.
type Setup struct {
	StateDir, SpoolDir, UnitDir string
	UID                         uint32 // the owner of the directories: root (the test user in tests)

	Units  map[string]string // the units this binary carries
	Enable []string          // the units to enable; NAME is the instance

	ListenerUser string // the listener user (and its group), "pullgate"
	Useradd      string // the useradd program
	// Systemctl runs systemctl with args.
	Systemctl func(ctx context.Context, args ...string) (string, error)
	// UserExists and AddUser look up and make a user; nil is user.Lookup
	// and Useradd. Tests give fakes.
	UserExists func(name string) bool
	AddUser    func(ctx context.Context, name string) error
	// Doctor runs pullgate doctor on the instances of listener.toml and
	// returns the number of problems. beforeEnable leaves out the checks
	// that the units are enabled and run.
	Doctor func(ctx context.Context, beforeEnable bool) int
}

var userRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// checkUser is the check_user of the instance: of its config when it
// exists, else the one that minimalConfig writes.
func (o *Options) checkUser(name string) (string, error) {
	b, err := os.ReadFile(o.instanceConfig(name))
	if errors.Is(err, fs.ErrNotExist) {
		return "pullgate-check-" + name, nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", o.instanceConfig(name), err)
	}
	var c struct {
		Commands struct {
			CheckUser string `toml:"check_user"`
		} `toml:"commands"`
	}
	if err := toml.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("%s: %w. Fix: correct the TOML, or move the file away so bootstrap writes a new one", o.instanceConfig(name), err)
	}
	if c.Commands.CheckUser == "" {
		return "pullgate-check", nil // the default of the config
	}
	return c.Commands.CheckUser, nil
}

// users makes the listener user and the check user of each instance, as
// system users with their own group, no home and no shell, when they do
// not exist yet.
func (o *Options) users(ctx context.Context) error {
	s := o.Setup
	names := []string{s.ListenerUser}
	for _, in := range o.Instances {
		u, err := o.checkUser(in.Name)
		if err != nil {
			return err
		}
		if u == s.ListenerUser {
			return fmt.Errorf("--instance %s: check_user is the listener user %s; a check that is taken over could write approvals (THREAT-MODEL.md T2). Fix: set [commands] check_user in %s to its own user", in.Name, u, o.instanceConfig(in.Name))
		}
		if !slices.Contains(names, u) {
			names = append(names, u)
		}
	}
	exists, add := s.UserExists, s.AddUser
	if exists == nil {
		exists = func(n string) bool { _, err := user.Lookup(n); return err == nil }
	}
	if add == nil {
		add = s.useradd
	}
	for _, n := range names {
		if !userRE.MatchString(n) {
			return fmt.Errorf("%q is not a user name. Fix: use [a-z_][a-z0-9_-]{0,31}", n)
		}
		if exists(n) {
			o.say("User %s: exists.", n)
			continue
		}
		if err := add(ctx, n); err != nil {
			return fmt.Errorf("make the user %s: %w. Fix: make it by hand (useradd --system --user-group %s), then run bootstrap again", n, err, n)
		}
		if !exists(n) {
			return fmt.Errorf("useradd %s ran, but the user is not there. Fix: check /etc/passwd and nsswitch", n)
		}
		o.say("User %s: made.", n)
	}
	return nil
}

// useradd makes a system user with its own group, no home and no shell.
func (s *Setup) useradd(ctx context.Context, n string) error {
	cmd := exec.CommandContext(ctx, s.Useradd, "--system", "--no-create-home", "--user-group", "--shell", "/usr/sbin/nologin", n) //nolint:gosec // a checked user name
	cmd.Env = childEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dirs makes the base directories with the owner and the mode that doctor
// checks, and reads them back.
func (o *Options) dirs() error {
	for _, w := range doctor.BaseDirs(o.EtcDir, o.Setup.StateDir, o.Setup.SpoolDir, int(o.Setup.UID)) {
		if err := os.Mkdir(w.Path, w.Mode); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("make %s: %w. Fix: make the directories above it", w.Path, err)
		}
		// A directory that another user owns is not taken over: what that
		// user put in it would stay.
		if fi, err := os.Lstat(w.Path); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != w.UID {
				return fmt.Errorf("%s exists and is not owned by uid %d. Fix: check what is in it, then chown root %s, or remove it", w.Path, w.UID, w.Path)
			}
		}
		f, err := os.OpenFile(w.Path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w. Fix: it must be a directory, not a link", w.Path, err)
		}
		err = f.Chown(w.UID, -1)
		if err == nil {
			err = f.Chmod(w.Mode)
		}
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("fix %s: %w", w.Path, err)
		}
		if err := doctor.ReadBack(w); err != nil {
			return err
		}
	}
	o.say("Directories: %s, %s and %s are in place.", o.EtcDir, o.Setup.StateDir, o.Setup.SpoolDir)
	return nil
}

// units writes the units of this version to the unit directory, and runs
// systemctl daemon-reload.
func (o *Options) units(ctx context.Context) error {
	s := o.Setup
	changed := 0
	for _, n := range slices.Sorted(maps.Keys(s.Units)) {
		p := filepath.Join(s.UnitDir, n)
		if b, err := os.ReadFile(p); err == nil && string(b) == s.Units[n] { //nolint:gosec // a unit file of pullgate
			continue
		}
		if err := writeOwned(p, []byte(s.Units[n]), 0o644, -1); err != nil {
			return fmt.Errorf("%w. Fix: check that %s exists and that root can write it", err, s.UnitDir)
		}
		changed++
	}
	o.say("Units: %d of %d written to %s (the others were the same).", changed, len(s.Units), s.UnitDir)
	if _, err := s.Systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w. Fix: run it by hand and see its error", err)
	}
	return nil
}

// instanceDirs makes the state and the spool of an instance, as a run
// does, so that doctor can check them before the first run.
func (o *Options) instanceDirs(name string) error {
	ow, err := gate.ResolveOwners(o.Setup.ListenerUser)
	if err != nil {
		return err
	}
	if err := (state.Dir{Path: filepath.Join(o.Setup.StateDir, name)}).Ensure(); err != nil {
		return err
	}
	return spool.Dirs{Base: o.Setup.SpoolDir, Name: name}.Ensure(o.Setup.UID, ow)
}

// enable runs doctor, and enables and starts the units only when it finds
// no problem. Then doctor runs again, with the units.
func (o *Options) enable(ctx context.Context) error {
	s := o.Setup
	o.say("Checking the setup with pullgate doctor:")
	if n := s.Doctor(ctx, true); n > 0 {
		return fmt.Errorf("pullgate doctor found %d problem(s) (above), so no unit was enabled. Fix them, then run pullgate bootstrap again", n)
	}
	var units []string
	for _, e := range s.Enable {
		if !strings.Contains(e, "NAME") {
			units = append(units, e)
			continue
		}
		for _, in := range o.Instances {
			units = append(units, strings.ReplaceAll(e, "NAME", in.Name))
		}
	}
	// The listener is enabled, not started here: the restart below starts
	// it once. A start and then a restart would kill the first listener in
	// its first request, and it would log an error.
	var now, later []string
	for _, u := range units {
		if u == "pullgate-listen.service" {
			later = append(later, u)
		} else {
			now = append(now, u)
		}
	}
	if len(later) > 0 {
		if _, err := s.Systemctl(ctx, append([]string{"enable"}, later...)...); err != nil {
			return fmt.Errorf("systemctl enable %s: %w. Fix: see journalctl -xe, fix the unit, then run pullgate bootstrap again", strings.Join(later, " "), err)
		}
	}
	if _, err := s.Systemctl(ctx, append([]string{"enable", "--now"}, now...)...); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %w. Fix: see journalctl -xe, fix the unit, then run pullgate bootstrap again", strings.Join(now, " "), err)
	}
	// A listener that ran before has the old listener.toml: restart it.
	if _, err := s.Systemctl(ctx, "restart", "pullgate-listen.service"); err != nil {
		return fmt.Errorf("systemctl restart pullgate-listen.service: %w. Fix: see journalctl -u pullgate-listen.service", err)
	}
	o.say("Enabled and started: %s.", strings.Join(units, ", "))
	o.say("Checking again, with the units running:")
	if n := s.Doctor(ctx, false); n > 0 {
		return fmt.Errorf("the units are enabled, but pullgate doctor found %d problem(s) (above). Fix them, then run pullgate doctor", n)
	}
	return nil
}
