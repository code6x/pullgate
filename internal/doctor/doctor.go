// Package doctor is "pullgate doctor": it checks the setup of a host and
// prints one line per check (DESIGN.md section 18). It changes nothing,
// but for a small test file in each outbox, which it removes.
package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/gate"
	"github.com/code6x/pullgate/internal/gitx"
	"github.com/code6x/pullgate/internal/safedir"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
	"github.com/code6x/pullgate/internal/telegram"
)

// Options are the inputs of a doctor run.
type Options struct {
	Version   string
	ConfigDir string // /etc/pullgate
	StateDir  string // /var/lib/pullgate
	SpoolDir  string // /var/spool/pullgate
	UnitDir   string // /etc/systemd/system
	Listener  string // the listener config, ConfigDir/listener.toml
	Name      string // one instance; "" is every instance of the listener config
	Telegram  bool   // also check the bot token and the chat (it calls Telegram)
	UID       uint32 // the owner of the gate files: root (the test user in tests)

	Units  map[string]string // the units this binary carries
	Enable []string          // the units the README enables; NAME is the instance

	// Systemctl runs systemctl with args and returns its output; tests
	// give a fake.
	Systemctl func(ctx context.Context, args ...string) (string, error)
	// ReadAs reads the file path as the user uid, with the groups gids
	// (the first is the primary group); tests give a fake.
	ReadAs func(ctx context.Context, uid int, gids []int, path string) error
	// Remote checks that git can read the branch of an instance; nil is
	// git ls-remote with the deploy key and the pinned host key.
	Remote func(ctx context.Context, cfg *config.Instance, uid uint32) error

	Out io.Writer
}

// The units that must be enabled. The others of Enable are optional.
var required = map[string]bool{
	"pullgate-listen.service":  true,
	"pullgate-apply@NAME.path": true,
	"pullgate-run@NAME.timer":  true,
}

type doc struct {
	o        *Options
	problems int
}

func (d *doc) ok(what, text string) { _, _ = fmt.Fprintf(d.o.Out, "ok       %s: %s\n", what, text) }
func (d *doc) note(what, text string) {
	_, _ = fmt.Fprintf(d.o.Out, "note     %s: %s\n", what, text)
}
func (d *doc) problem(what, text string) {
	d.problems++
	_, _ = fmt.Fprintf(d.o.Out, "PROBLEM  %s: %s\n", what, oneLine(text))
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Run runs every check and returns the number of problems.
func Run(ctx context.Context, o *Options) int {
	d := &doc{o: o}
	if o.Version == "dev" {
		d.note("version", "pullgate dev: a build without a version, so the units cannot be matched to a release")
	} else {
		d.ok("version", "pullgate "+o.Version)
	}
	// token_file may be empty: the example unit gives the token as a
	// systemd credential, which a root shell does not have. token checks it.
	lc, err := config.LoadListenerForUpdate(o.Listener, o.UID)
	if err != nil {
		d.problem("listener config", err.Error())
	} else {
		d.ok("listener config", o.Listener+" is valid, with the instances "+strings.Join(lc.Instances, ", "))
	}
	names := []string{}
	switch {
	case o.Name != "":
		names = append(names, o.Name)
		if lc != nil && !slices.Contains(lc.Instances, o.Name) {
			d.problem("listener config", o.Listener+" does not list the instance "+o.Name+", so the listener sends none of its questions. Fix: add it to instances, or run pullgate bootstrap with every instance of the host")
		}
	case lc != nil:
		names = lc.Instances
	}
	d.units(ctx, names)
	var owners *spool.Owners
	if lc != nil {
		if ow, err := gate.ResolveOwners(lc.Updates.ListenerUser); err != nil {
			d.problem("listener user", err.Error())
		} else {
			owners = &ow
			d.listenerReads(lc, ow)
			d.token(ctx, lc, ow)
		}
	}
	for _, n := range names {
		d.instance(ctx, n, owners)
	}
	if d.problems == 0 {
		_, _ = fmt.Fprintln(o.Out, "All checks passed.")
	} else {
		_, _ = fmt.Fprintf(o.Out, "%d problem(s). Fix them as each line says, then run pullgate doctor again.\n", d.problems)
	}
	return d.problems
}

// units checks that the units of this version are installed, enabled
// where the README says so, and that the listener runs.
func (d *doc) units(ctx context.Context, names []string) {
	o := d.o
	fix := "copy it from contrib/systemd/ of version " + o.Version + " to " + o.UnitDir + ", then run systemctl daemon-reload"
	for _, n := range slices.Sorted(maps.Keys(o.Units)) {
		b, err := os.ReadFile(filepath.Join(o.UnitDir, n)) //nolint:gosec // a unit file, read only to compare
		switch {
		case errors.Is(err, fs.ErrNotExist):
			d.problem("unit "+n, "it is not installed in "+o.UnitDir+". Fix: "+fix)
		case err != nil:
			d.problem("unit "+n, err.Error()+". Fix: check "+o.UnitDir)
		case string(b) != o.Units[n]:
			d.note("unit "+n, "it differs from the unit of this version. If you did not change it on purpose: "+fix)
		default:
			d.ok("unit "+n, "installed, the same as in this version")
		}
	}
	for _, e := range o.Enable {
		list := []string{e}
		if strings.Contains(e, "NAME") {
			list = nil
			for _, n := range names {
				list = append(list, strings.ReplaceAll(e, "NAME", n))
			}
		}
		for _, u := range list {
			out, _ := o.Systemctl(ctx, "is-enabled", u)
			switch {
			case out == "enabled":
				d.ok("unit "+u, "enabled")
			case required[e]:
				d.problem("unit "+u, "systemctl is-enabled says "+strconv.Quote(out)+". Fix: systemctl enable --now "+u)
			default:
				d.note("unit "+u, "not enabled (optional, see the README). To use it: systemctl enable --now "+u)
			}
		}
	}
	out, _ := o.Systemctl(ctx, "is-active", "pullgate-listen.service")
	if out == "active" {
		d.ok("unit pullgate-listen.service", "active")
	} else {
		d.problem("unit pullgate-listen.service", "systemctl is-active says "+strconv.Quote(out)+": no question reaches the owner. Fix: see journalctl -u pullgate-listen.service, then systemctl restart pullgate-listen.service")
	}
}

// listenerReads checks that the listener user can read listener.toml
// (root, 0640 with its group, or 0644), and that both use the same spool.
func (d *doc) listenerReads(lc *config.Listener, ow spool.Owners) {
	if fi, err := os.Stat(lc.Path); err == nil {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || !canRead(fi.Mode().Perm(), int(st.Uid), int(st.Gid), ow.ListenerUID, listenerGroups(ow)) {
			d.problem("listener config", lc.Path+" cannot be read by the listener user, so the listener does not start. Fix: chown root:"+strconv.Itoa(ow.ListenerGID)+" "+lc.Path+" and chmod 0640 "+lc.Path)
		}
	}
	if filepath.Clean(lc.SpoolDir) != filepath.Clean(d.o.SpoolDir) {
		d.problem("listener config", "spool_dir of "+lc.Path+" is "+lc.SpoolDir+", but root uses "+d.o.SpoolDir+", so no question reaches the listener. Fix: remove spool_dir from "+lc.Path+", or give doctor the same --spool-dir")
	}
}

// canRead says whether the user luid with the groups gids can read a file
// with the mode m, the owner uid and the group gid.
func canRead(m fs.FileMode, uid, gid, luid int, gids []int) bool {
	switch {
	case uid == luid:
		return m&0o400 != 0
	case slices.Contains(gids, gid):
		return m&0o040 != 0
	}
	return m&0o004 != 0
}

// listenerGroups is the primary group of the listener user, then its
// other groups.
func listenerGroups(l spool.Owners) []int {
	gids := []int{l.ListenerGID}
	if lu, err := user.LookupId(strconv.Itoa(l.ListenerUID)); err == nil {
		if ids, err := lu.GroupIds(); err == nil {
			for _, s := range ids {
				if g, err := strconv.Atoi(s); err == nil && g != l.ListenerGID {
					gids = append(gids, g)
				}
			}
		}
	}
	return gids
}

// tokenFile is the bot token file, and whether it is the systemd
// credential of the example unit (owned by root) rather than a token_file
// that the listener reads itself (owned by the listener user).
func (d *doc) tokenFile(lc *config.Listener) (string, bool) {
	if cd := os.Getenv("CREDENTIALS_DIRECTORY"); lc.TokenFile != "" && (cd == "" || !strings.HasPrefix(lc.TokenFile, cd+"/")) {
		return lc.TokenFile, false
	}
	return filepath.Join(d.o.ConfigDir, "bot-token"), true
}

// token checks the owner and the mode of the token file; with --telegram
// also the token and the chat, with Telegram. The token is never printed.
func (d *doc) token(ctx context.Context, lc *config.Listener, ow spool.Owners) {
	p, credential := d.tokenFile(lc)
	c := *lc
	c.TokenFile = p
	owner, fix := uint32(ow.ListenerUID), "token_file is read by the listener: it must be owned by the listener user, with mode 0600" //nolint:gosec // a uid fits in uint32
	if credential {
		owner, fix = d.o.UID, "the example unit gives "+p+" to the listener as a systemd credential: it must be owned by root, with mode 0600 (pullgate bootstrap writes it)"
	}
	tok, err := c.ReadToken(owner)
	if err != nil {
		d.problem("bot token", err.Error()+". Fix: "+fix)
		return
	}
	d.ok("bot token", p+": the owner and the mode are right")
	if !d.o.Telegram {
		d.note("telegram", "not checked; run pullgate doctor --telegram to check the token and the chat with Telegram")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tg := telegram.New(lc.APIURL, tok)
	me, err := tg.GetMe(ctx)
	if err != nil {
		d.problem("telegram", "getMe failed: "+err.Error()+". Fix: check the token with BotFather and the network of the host")
		return
	}
	d.ok("telegram", "the token works (bot @"+me.Username+")")
	ch, err := tg.GetChat(ctx, lc.ChatID)
	if err != nil {
		d.problem("telegram", "getChat failed for chat_id of "+lc.Path+": "+err.Error()+". Fix: send /start to the bot in your private chat, or run pullgate bootstrap to pair again")
		return
	}
	d.ok("telegram", "the bot can reach the chat ("+ch.Type+")")
}

// want is one path with the owner, group and mode that DESIGN.md section 4
// gives it. gid -1: any group.
type want struct {
	path     string
	uid, gid int
	mode     fs.FileMode
}

// instance checks one instance: its config and gate files, its users, the
// owners and modes of its directories as read back, that the listener can
// read its outbox, and that git can read its repo.
func (d *doc) instance(ctx context.Context, name string, ow *spool.Owners) {
	o := d.o
	what := "instance " + name
	cfg, err := config.Load(o.ConfigDir, name, o.UID)
	if err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "Fix") {
			msg += ". Fix: make " + filepath.Join(o.ConfigDir, name+".toml") + " as the README says (section Config), owned by root, mode 0600"
		}
		d.problem(what, msg)
		return
	}
	env, err := gate.New(cfg, o.StateDir, o.SpoolDir, o.UID, slog.New(slog.DiscardHandler), io.Discard)
	if err != nil {
		d.problem(what, err.Error())
		return
	}
	d.ok(what, cfg.Path+" is valid; its files, its programs and check_user "+cfg.Commands.CheckUser+" pass the checks")
	if ow != nil && (ow.ListenerUID != env.Owners.ListenerUID || ow.ListenerGID != env.Owners.ListenerGID) {
		d.problem(what, "[listener] user of "+cfg.Path+" is not the listener user of "+o.Listener+" (updates.listener_user). Fix: give both the same user")
	}
	u, l := int(o.UID), env.Owners
	sd := state.Dir{Path: filepath.Join(o.StateDir, name)}
	sp := env.Spool
	list := []want{
		{o.StateDir, u, -1, 0o711},
		{sd.Path, u, -1, 0o711},
		{sd.Repo(), u, -1, 0o700},
		{sd.Trees(), u, -1, 0o711},
		{sd.StateFile(), u, -1, 0o600},
		{o.SpoolDir, u, -1, 0o755},
		{filepath.Dir(sp.Outbox()), u, -1, 0o755},
		{sp.Outbox(), u, l.ListenerGID, 0o770},
		{sp.Inbox(), l.ListenerUID, l.ListenerGID, 0o700},
		{sp.CheckDir(), l.ListenerUID, l.ListenerGID, 0o700},
		{sp.Receipts(), l.ListenerUID, l.ListenerGID, 0o700},
	}
	missing, bad := []string{}, false
	for _, w := range list {
		err := readBack(w)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, w.path)
		case err != nil:
			bad = true
			d.problem(what, strings.ReplaceAll(err.Error(), "@NAME.", "@"+name+"."))
		}
	}
	if len(missing) > 0 {
		d.note(what, "not made yet: "+strings.Join(missing, ", ")+". The first run makes them: systemctl start pullgate-run@"+name+".service")
	}
	if !bad {
		d.ok(what, "the owners and the modes of the state and the spool, as read back, are right")
	}
	d.outbox(ctx, name, sp, l)
	remote := o.Remote
	if remote == nil {
		remote = lsRemote
	}
	if err := remote(ctx, cfg, o.UID); err != nil {
		fix := "Fix: check url and branch in [repo] of " + cfg.Path
		if local, _ := gitx.LocalPath(cfg.Repo.URL); local == "" {
			fix += ", that the deploy key " + cfg.Repo.DeployKey + " is added to the repo (read-only), and that " + cfg.Repo.KnownHosts + " holds the host key of the git server"
		}
		d.problem(what, "git cannot read the branch "+cfg.Repo.Branch+" of "+cfg.Repo.URL+": "+err.Error()+". "+fix)
	} else {
		d.ok(what, "git can read the branch "+cfg.Repo.Branch+" with the deploy key and the pinned host key")
	}
}

// readBack reads the owner and the mode of w.path, without following a
// symlink, and compares them with w.
func readBack(w want) error {
	fi, err := os.Lstat(w.path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner", w.path)
	}
	got := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	if fi.Mode()&fs.ModeSymlink != 0 || int(st.Uid) != w.uid || (w.gid >= 0 && int(st.Gid) != w.gid) || got != w.mode {
		g := "any group"
		if w.gid >= 0 {
			g = "group " + strconv.Itoa(w.gid)
		}
		return fmt.Errorf("%s has owner %d, group %d, mode %s%s; DESIGN.md section 4 wants owner %d, %s, mode %s. "+
			"Fix: run systemctl start pullgate-run@NAME.service, which sets them; if they stay wrong, the file system or the unit changes them (check RestrictSUIDSGID, CAP_CHOWN and the mount options)",
			w.path, st.Uid, st.Gid, got, symlinkNote(fi), w.uid, g, w.mode)
	}
	return nil
}

func symlinkNote(fi fs.FileInfo) string {
	if fi.Mode()&fs.ModeSymlink != 0 {
		return " (a symlink)"
	}
	return ""
}

// outbox writes a test file to the outbox as root does, and reads it as
// the listener user: the check that issue #19 was missing.
func (d *doc) outbox(ctx context.Context, name string, sp spool.Dirs, l spool.Owners) {
	what := "instance " + name
	if _, err := os.Lstat(sp.Outbox()); err != nil {
		return // not made yet: said above
	}
	p, err := sp.WriteDoctorTest(l.ListenerGID)
	if err != nil {
		d.problem(what, "root cannot write to the outbox: "+err.Error())
		return
	}
	defer func() { _ = sp.RemoveDoctorTest(p) }()
	if err := d.o.ReadAs(ctx, l.ListenerUID, listenerGroups(l), p); err != nil {
		d.problem(what, "the listener user cannot read a file in "+sp.Outbox()+" that root wrote, so no question would reach the owner: "+err.Error()+". Fix: run systemctl start pullgate-run@"+name+".service to fix the modes, and check that the outbox has the group of the listener user")
		return
	}
	d.ok(what, "the listener user can read the outbox")
}

// lsRemote runs git ls-remote for the branch of cfg, as a run fetches:
// with the deploy key and the pinned host key, in a new temporary home.
func lsRemote(ctx context.Context, cfg *config.Instance, uid uint32) error {
	local, err := gitx.LocalPath(cfg.Repo.URL)
	if err != nil {
		return err
	}
	if local != "" {
		if err := safedir.CheckTree(local, uid); err != nil {
			return fmt.Errorf("the local repo %s is not safe for root: %w", local, err)
		}
	}
	tmp, err := os.MkdirTemp("", "pullgate-doctor-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	g := &gitx.Git{Dir: filepath.Join(tmp, "probe.git"), Home: tmp, UID: uid, Key: cfg.Repo.DeployKey, KnownHosts: cfg.Repo.KnownHosts, Timeout: 30 * time.Second}
	if err := g.Init(ctx); err != nil {
		return err
	}
	out, err := g.Run(ctx, "ls-remote", "--heads", "--", cfg.Repo.URL, "refs/heads/"+cfg.Repo.Branch)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return errors.New("the branch does not exist")
	}
	return nil
}

// Systemctl runs systemctl with a fixed path and returns its first line.
func Systemctl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", args...) //nolint:gosec // fixed program
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0"}
	out, err := cmd.Output()
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" && err != nil {
		s = "no answer"
	}
	return s, err
}

// ReadAsCommand is the hidden command that ReadAs starts: it reads one
// byte of the file in its argument.
const ReadAsCommand = "_doctor-read"

// ReadAs starts this binary as the user uid with the groups gids, and
// lets it read the file path (ReadAsCommand). It needs root.
func ReadAs(ctx context.Context, uid int, gids []int, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/proc/self/exe", ReadAsCommand, path) //nolint:gosec // this binary; the path is the test file of WriteDoctorTest
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	cmd.Dir = "/"
	groups := make([]uint32, 0, len(gids))
	for _, g := range gids {
		groups = append(groups, uint32(g)) //nolint:gosec // a gid fits in uint32
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: groups[0], Groups: groups}} //nolint:gosec // a uid fits in uint32
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		if s := strings.TrimSpace(errOut.String()); s != "" {
			return errors.New(oneLine(s))
		}
		return err
	}
	return nil
}

// ReadOne is the body of ReadAsCommand. It reads only a test file of
// WriteDoctorTest, and prints nothing of it.
func ReadOne(path string) error {
	if !filepath.IsAbs(path) || !strings.HasPrefix(filepath.Base(path), spool.DoctorPrefix) {
		return errors.New("not a test file of pullgate doctor")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // a test file of WriteDoctorTest
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Read(make([]byte, 1))
	return err
}
