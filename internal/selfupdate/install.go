package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/code6x/pullgate/internal/message"
	"github.com/code6x/pullgate/internal/state"
)

// UnitList is what "pullgate units" prints: the units of contrib/systemd/
// that the binary carries, and the units that README.md enables (NAME for
// each instance).
type UnitList struct {
	Units  map[string]string `json:"units"`
	Enable []string          `json:"enable"`
}

var (
	unitNameRE   = regexp.MustCompile(`^pullgate-[a-z0-9-]{1,40}@?\.(service|timer|path)$`)
	enableNameRE = regexp.MustCompile(`^pullgate-[a-z0-9-]{1,40}(@NAME)?\.(service|timer|path)$`)
)

// check checks the list that a new binary printed. The binary passed the
// attestation, but root still writes only files named pullgate-*.
func (l *UnitList) check() error {
	if len(l.Units) == 0 || len(l.Units) > maxUnits {
		return fmt.Errorf("pullgate units: %d units, want 1 to %d", len(l.Units), maxUnits)
	}
	for n, c := range l.Units {
		if !unitNameRE.MatchString(n) {
			return fmt.Errorf("pullgate units: %q is not a name of a pullgate unit", n)
		}
		if len(c) > maxUnitSize || strings.ContainsRune(c, 0) {
			return fmt.Errorf("pullgate units: %s is too large or holds a NUL", n)
		}
	}
	for _, e := range l.Enable {
		if !enableNameRE.MatchString(e) {
			return fmt.Errorf("pullgate units: %q is not a unit to enable", e)
		}
		if _, ok := l.Units[template(e)]; !ok {
			return fmt.Errorf("pullgate units: %s is enabled, but there is no unit file %s", e, template(e))
		}
	}
	return nil
}

// template is the file name of a unit: pullgate-run@NAME.timer is
// pullgate-run@.timer.
func template(unit string) string { return strings.Replace(unit, "@NAME.", "@.", 1) }

// fileOf is the unit file of a unit: pullgate-run@config-repo.timer is
// pullgate-run@.timer.
func fileOf(unit string) string {
	at := strings.Index(unit, "@")
	dot := strings.LastIndex(unit, ".")
	if at < 0 || dot < at {
		return unit
	}
	return unit[:at+1] + unit[dot:]
}

// unitChange is one unit file that the update wrote: what was there
// before, for a rollback.
type unitChange struct {
	Name    string
	Old     []byte
	Existed bool
}

// unitReport is what the update did to the units.
type unitReport struct {
	changes []unitChange
	links   []string // unit files that are links (for example masked), left alone
	dropins []string // every drop-in of a pullgate unit
	enabled []string // the units that were re-enabled
}

// install installs the release of q (DESIGN.md section 15).
func (u *Updater) install(ctx context.Context, st *State, q *Question) (Result, error) {
	from := u.Version
	st.InProgress = &InProgress{Version: q.Version, From: from, Started: u.now()}
	if err := u.save(st); err != nil {
		return "", err
	}
	u.Log.Info("update started", "version", q.Version, "sha256", q.SHA256, "from", from)
	dir := filepath.Dir(u.Bin)
	prev := filepath.Join(dir, ".pullgate.prev")
	// Keep the old binary: a second name for the same file. The rename of
	// install.sh puts the new file under the name pullgate; the old one
	// stays as .pullgate.prev until the new one works.
	if err := os.Remove(prev); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return u.failed(st, q, "cannot remove the old "+prev+": "+err.Error())
	}
	if err := os.Link(u.Bin, prev); err != nil {
		return u.failed(st, q, "cannot keep the old binary as "+prev+": "+err.Error())
	}
	syncDir(dir)
	if u.crash("kept") {
		return "", errCrash
	}
	args := []string{"--version", q.Version, "--sha256", q.SHA256, "--forward-only", "--bin-dir", dir}
	if u.Cfg.Updates.GH != "" {
		args = append(args, "--gh", u.Cfg.Updates.GH)
	}
	tell, reason, err := u.script(ctx, args...)
	if err != nil || !slices.Equal(tell["installed"], []string{q.Version, q.SHA256}) {
		if err == nil {
			reason = "install.sh did not install it. This is a bug in pullgate"
		}
		// install.sh renames the new file in its last step. When it says
		// it did not get there, the old binary is still in place; check.
		if out, verr := u.binVersion(ctx, u.Bin); verr != nil || out != "pullgate "+from {
			return u.rollback(ctx, st, q, prev, nil, reason)
		}
		_ = os.Remove(prev)
		return u.failed(st, q, reason)
	}
	if u.crash("installed") {
		return "", errCrash
	}
	if out, err := u.binVersion(ctx, u.Bin); err != nil || out != "pullgate "+q.Version {
		return u.rollback(ctx, st, q, prev, nil, "the new pullgate does not run, or does not say that it is "+q.Version)
	}
	rep, err := u.updateUnits(ctx)
	if err != nil {
		return u.rollback(ctx, st, q, prev, rep, err.Error())
	}
	if u.crash("units") {
		return "", errCrash
	}
	if err := u.restartListener(ctx); err != nil {
		return u.rollback(ctx, st, q, prev, rep, err.Error())
	}
	_ = os.Remove(prev)
	syncDir(dir)
	st.InProgress = nil
	if err := u.save(st); err != nil {
		return "", err
	}
	u.Log.Info("updated", "version", q.Version, "from", from, "units_changed", len(rep.changes))
	// The new listener sends this: so the report also shows that it runs.
	u.reply(q.ID, "✅ pullgate is updated: "+message.Code(from)+" → "+message.Code(q.Version)+".\n"+rep.text())
	return ResInstalled, nil
}

// failed reports an update that installed nothing.
func (u *Updater) failed(st *State, q *Question, reason string) (Result, error) {
	st.InProgress = nil
	if err := u.save(st); err != nil {
		return "", err
	}
	u.Log.Error("update failed, nothing was installed", "version", q.Version, "reason", reason)
	u.reply(q.ID, "❌ The update to "+message.Code(q.Version)+" failed: "+message.Code(reason)+". Nothing was installed; pullgate "+message.Code(u.Version)+" still runs. Send <code>/update</code> to try again.")
	return ResFailed, nil
}

// rollback puts the old binary back, and the old units, and restarts the
// listener. A rename in one directory: the binary is always the old file
// or the new one.
func (u *Updater) rollback(parent context.Context, st *State, q *Question, prev string, rep *unitReport, reason string) (Result, error) {
	u.Log.Error("the update failed, rolling back", "version", q.Version, "reason", reason)
	// A stop of the service must not stop the way back.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Minute)
	defer cancel()
	var problems []string
	if a, errA := os.Stat(prev); errA == nil {
		if b, errB := os.Stat(u.Bin); errB == nil && os.SameFile(a, b) {
			_ = os.Remove(prev) // the binary was never replaced
		}
	}
	if _, err := os.Lstat(prev); errors.Is(err, fs.ErrNotExist) {
		// nothing to put back
	} else if err := os.Rename(prev, u.Bin); err != nil {
		problems = append(problems, "the old binary was not put back: "+err.Error())
	}
	syncDir(filepath.Dir(u.Bin))
	if rep != nil {
		problems = append(problems, u.restoreUnits(ctx, rep)...)
	}
	if out, err := u.binVersion(ctx, u.Bin); err != nil || out != "pullgate "+u.Version {
		problems = append(problems, "the binary does not say that it is "+u.Version)
	}
	if err := u.restartListener(ctx); err != nil {
		problems = append(problems, err.Error())
	}
	st.InProgress = nil
	if err := u.save(st); err != nil {
		return "", err
	}
	text := "❌ The update to " + message.Code(q.Version) + " failed: " + message.Code(reason) + ". pullgate " + message.Code(u.Version) + " is back, with its units."
	if len(problems) > 0 {
		u.Log.Error("the rollback had problems", "problems", strings.Join(problems, "; "))
		text = "❌ The update to " + message.Code(q.Version) + " failed: " + message.Code(reason) + ". The rollback to " + message.Code(u.Version) + " had problems: " + message.Code(strings.Join(problems, "; ")) +
			". Fix: as root, run " + message.Code("install.sh --version "+u.Version+" --reinstall") + ", copy the units of that version to /etc/systemd/system, and run " + message.Code("systemctl daemon-reload") + "."
	}
	u.reply(q.ID, text)
	return ResFailed, nil
}

// updateUnits takes the units of the new binary and installs the ones
// that differ, then reloads systemd, re-enables and restarts them (not
// the listener: restartListener does it last).
func (u *Updater) updateUnits(ctx context.Context) (*unitReport, error) {
	rep := &unitReport{}
	out, err := u.run(ctx, maxUnitsJSON, u.Bin, "units")
	if err != nil {
		return rep, fmt.Errorf("the new pullgate units failed: %w", err)
	}
	var l UnitList
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return rep, fmt.Errorf("the new pullgate units printed no unit list: %w", err)
	}
	if err := l.check(); err != nil {
		return rep, err
	}
	names := make([]string, 0, len(l.Units))
	for n := range l.Units {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		p := filepath.Join(u.UnitDir, n)
		c := unitChange{Name: n}
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return rep, fmt.Errorf("read %s: %w", p, err)
		case fi.Mode()&fs.ModeSymlink != 0 || !fi.Mode().IsRegular():
			// Masked (a link to /dev/null) or linked by the owner.
			rep.links = append(rep.links, n)
			continue
		default:
			c.Existed = true
			if c.Old, err = os.ReadFile(p); err != nil { //nolint:gosec // a unit file of pullgate
				return rep, fmt.Errorf("read %s: %w", p, err)
			}
			if bytes.Equal(c.Old, []byte(l.Units[n])) {
				continue
			}
		}
		if err := state.WriteFileAtomic(p, []byte(l.Units[n]), 0o644); err != nil {
			return rep, err
		}
		rep.changes = append(rep.changes, c)
	}
	if m, err := filepath.Glob(filepath.Join(u.UnitDir, "pullgate-*.d", "*")); err == nil {
		for _, p := range m {
			rel, _ := filepath.Rel(u.UnitDir, p)
			rep.dropins = append(rep.dropins, rel)
		}
	}
	if err := u.Systemctl(ctx, "daemon-reload"); err != nil {
		return rep, fmt.Errorf("systemctl daemon-reload failed: %w", err)
	}
	isNew := map[string]bool{}
	for _, c := range rep.changes {
		isNew[c.Name] = !c.Existed
	}
	for _, e := range l.Enable {
		for _, unit := range u.expand(e) {
			if unit == ListenerUnit {
				continue
			}
			// A unit that the owner turned off stays off; a unit that is
			// new in this version is enabled, as the README says.
			if !isNew[template(e)] && u.Systemctl(ctx, "is-enabled", "--quiet", unit) != nil {
				continue
			}
			if err := u.Systemctl(ctx, "reenable", unit); err != nil {
				return rep, fmt.Errorf("systemctl reenable %s failed: %w", unit, err)
			}
			rep.enabled = append(rep.enabled, unit)
			if err := u.Systemctl(ctx, "restart", unit); err != nil {
				return rep, fmt.Errorf("systemctl restart %s failed: %w", unit, err)
			}
		}
	}
	if slices.Contains(l.Enable, ListenerUnit) {
		if err := u.Systemctl(ctx, "reenable", ListenerUnit); err != nil {
			return rep, fmt.Errorf("systemctl reenable %s failed: %w", ListenerUnit, err)
		}
		rep.enabled = append(rep.enabled, ListenerUnit)
	}
	return rep, nil
}

// expand gives the units of one entry of the enable list: one for each
// instance when it has NAME.
func (u *Updater) expand(e string) []string {
	if !strings.Contains(e, "@NAME.") {
		return []string{e}
	}
	var out []string
	for _, inst := range u.Cfg.Instances {
		out = append(out, strings.Replace(e, "@NAME.", "@"+inst+".", 1))
	}
	return out
}

// restoreUnits writes back the unit files of before the update. It
// returns what did not work.
func (u *Updater) restoreUnits(ctx context.Context, rep *unitReport) []string {
	var problems []string
	for _, c := range rep.changes {
		p := filepath.Join(u.UnitDir, c.Name)
		if c.Existed {
			if err := state.WriteFileAtomic(p, c.Old, 0o644); err != nil {
				problems = append(problems, "unit "+c.Name+" was not put back: "+err.Error())
			}
			continue
		}
		for _, unit := range rep.enabled {
			if fileOf(unit) == c.Name {
				_ = u.Systemctl(ctx, "disable", "--now", unit)
			}
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			problems = append(problems, "the new unit "+c.Name+" was not removed: "+err.Error())
		}
	}
	if len(rep.changes) > 0 {
		if err := u.Systemctl(ctx, "daemon-reload"); err != nil {
			problems = append(problems, "systemctl daemon-reload failed: "+err.Error())
		}
		for _, unit := range rep.enabled {
			if unit == ListenerUnit {
				continue
			}
			if _, err := os.Lstat(filepath.Join(u.UnitDir, fileOf(unit))); err != nil {
				continue // removed above
			}
			if err := u.Systemctl(ctx, "restart", unit); err != nil {
				problems = append(problems, "systemctl restart "+unit+" failed")
			}
		}
	}
	return problems
}

// restartListener restarts the listener, and checks that it is still
// active after ListenerWait.
func (u *Updater) restartListener(ctx context.Context) error {
	if err := u.Systemctl(ctx, "restart", ListenerUnit); err != nil {
		return fmt.Errorf("systemctl restart %s failed: %w", ListenerUnit, err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(u.ListenerWait):
	}
	if err := u.Systemctl(ctx, "is-active", "--quiet", ListenerUnit); err != nil {
		return fmt.Errorf("%s is not active %s after its restart", ListenerUnit, u.ListenerWait)
	}
	return nil
}

// text is the part of the report about the units.
func (r *unitReport) text() string {
	var lines []string
	if len(r.changes) == 0 {
		lines = append(lines, "Units: no change.")
	} else {
		var n []string
		for _, c := range r.changes {
			n = append(n, message.Code(c.Name))
		}
		lines = append(lines, "Units installed from the new version: "+strings.Join(n, ", ")+".")
	}
	if len(r.links) > 0 {
		var n []string
		for _, c := range r.links {
			n = append(n, message.Code(c))
		}
		lines = append(lines, "Units that are links (masked?), left alone: "+strings.Join(n, ", ")+".")
	}
	if len(r.dropins) > 0 {
		var n []string
		for _, c := range r.dropins {
			n = append(n, message.Code(c))
		}
		lines = append(lines, "Drop-ins, not changed. Check that you still need them: "+strings.Join(n, ", ")+".")
	}
	return strings.Join(lines, "\n")
}

func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil { //nolint:gosec // the bin directory, checked by install.sh
		_ = f.Sync()
		_ = f.Close()
	}
}
