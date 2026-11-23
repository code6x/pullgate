// Package selfupdate is "pullgate update" (DESIGN.md section 15). Root
// looks for a new release of pullgate with the copy of install.sh that is
// built into the binary, sends a notice or asks the owner, and installs
// only the release that the owner approved, with every check of
// install.sh. Then it installs the units of the new version and restarts
// them. When the new binary does not start, it puts the old one back.
package selfupdate

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/message"
	"github.com/code6x/pullgate/internal/safedir"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

// Repo is where the releases come from. install.sh has the same name.
const Repo = "code6x/pullgate"

// RootPath is the PATH of every program root runs here, as in install.sh.
const RootPath = "/usr/sbin:/usr/bin:/sbin:/bin"

// QuestionTTL is how long an update question can be answered.
const QuestionTTL = 12 * time.Hour

// CheckInterval is the least time between two /update requests that root
// acts on.
const CheckInterval = time.Minute

// RepeatSecurity is how often the notice of a security release comes
// again, until it is installed.
const RepeatSecurity = 23 * time.Hour

// Limits of the programs that root runs here.
const (
	scriptTimeout = 20 * time.Minute // install.sh: two downloads and gh attestation verify
	runTimeout    = time.Minute      // pullgate version, pullgate units
	maxOutput     = 256 << 10        // the output of install.sh that is kept
	maxUnitsJSON  = 1 << 20          // the output of pullgate units
	maxUnits      = 32
	maxUnitSize   = 64 << 10
)

// ListenerUnit is the unit of the listener. It is restarted last.
const ListenerUnit = "pullgate-listen.service"

var versionRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

// ValidVersion reports whether v is a release version X.Y.Z.
func ValidVersion(v string) bool { return versionRE.MatchString(v) }

func parts(v string) [3]int {
	var p [3]int
	for i, s := range strings.SplitN(v, ".", 3) {
		p[i], _ = strconv.Atoi(s)
	}
	return p
}

// newer reports whether version a is newer than version b. Both must be
// X.Y.Z; else it is false.
func newer(a, b string) bool {
	if !ValidVersion(a) || !ValidVersion(b) {
		return false
	}
	pa, pb := parts(a), parts(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Systemctl runs systemctl with its arguments. The error is nil only when
// systemctl exits 0.
type Systemctl func(ctx context.Context, args ...string) error

// Updater holds what root needs for one start of pullgate update.
type Updater struct {
	Cfg      *config.Listener // the listener config: instances and [updates]
	StateDir string           // /var/lib/pullgate/_update
	Spool    spool.Dirs       // /var/spool/pullgate/_update
	Owners   spool.Owners
	UID      uint32 // root: owns the state and the spool
	Bin      string // the installed binary, DIR/pullgate
	Version  string // the version of this process
	Script   string // install.sh, built into the binary
	UnitDir  string // /etc/systemd/system
	Host     string // shown in each message
	Path     string // the PATH of install.sh; RootPath
	Arch     string // runtime.GOARCH
	Systemctl
	Log          *slog.Logger
	Out          io.Writer     // the journal: the output of install.sh
	ListenerWait time.Duration // how long the new listener must stay active
	Now          func() time.Time

	// Crash is for tests: when it returns true at a step, the update stops
	// there at once, as if the host lost power.
	Crash func(step string) bool
}

// errCrash is what a step returns after Crash said so.
var errCrash = errors.New("crash (test)")

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) crash(step string) bool { return u.Crash != nil && u.Crash(step) }

func (u *Updater) head() string {
	return "pullgate on <b>" + html.EscapeString(u.Host) + "</b>, update of pullgate"
}

func (u *Updater) send(m spool.Message) {
	m.Text = u.head() + "\n" + m.Text
	if _, err := u.Spool.Send(u.Owners.ListenerGID, m); err != nil {
		u.Log.Error("write to the outbox", "kind", m.Kind, "err", err)
	}
}

func (u *Updater) notice(text string) { u.send(spool.Message{Kind: spool.KindNotice, Text: text}) }

func (u *Updater) reply(q int64, text string) {
	u.send(spool.Message{Kind: spool.KindReply, Question: q, Text: text})
}

func (u *Updater) edit(q int64, text string) {
	u.send(spool.Message{Kind: spool.KindEdit, Question: q, Text: text})
}

// releaseURL is the page of the release notes. The version passed
// ValidVersion.
func releaseURL(v string) string { return "https://github.com/" + Repo + "/releases/tag/v" + v }

func (u *Updater) tarball(v string) string { return "pullgate_" + v + "_linux_" + u.Arch + ".tar.gz" }

// ensure makes the state directory (root, 0700), its tmp directory, and
// the spool (DESIGN.md section 4).
func (u *Updater) ensure() error {
	if err := safedir.CheckParents(filepath.Dir(u.StateDir), u.UID); err != nil {
		return fmt.Errorf("the state directory is not safe: %w", err)
	}
	for _, p := range []string{u.StateDir, u.tmpDir()} {
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("make %s: %w", p, err)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory. Fix: remove it", p)
		}
		if err := os.Chmod(p, 0o700); err != nil {
			return err
		}
	}
	if err := safedir.CheckParents(u.tmpDir(), u.UID); err != nil {
		return fmt.Errorf("the state directory is not safe: %w", err)
	}
	return u.Spool.Ensure(u.UID, u.Owners)
}

func (u *Updater) tmpDir() string { return filepath.Join(u.StateDir, "tmp") }

// Result is what one start did, for the log.
type Result string

// The results.
const (
	ResDev       Result = "dev build, no update"
	ResNothing   Result = "nothing to do"
	ResChecked   Result = "checked"
	ResAsked     Result = "asked"
	ResNoticed   Result = "notice sent"
	ResInstalled Result = "installed"
	ResFailed    Result = "update failed"
	ResSkipped   Result = "skipped"
)

// Run is one start of pullgate update: the approvals first, then a
// request for /update, else the daily look for a new release.
func (u *Updater) Run(ctx context.Context) (Result, error) {
	if err := u.ensure(); err != nil {
		return "", err
	}
	lock, err := state.Dir{Path: u.StateDir}.Lock(true)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Close() }()
	st, err := u.load()
	if err != nil {
		// Remove the request, so the path unit does not start this again
		// and again, and tell the owner.
		if n, _, _ := u.Spool.TakeChecks(u.Owners.ListenerUID); n > 0 {
			u.notice("Error: " + message.Code(err.Error()))
		}
		return "", err
	}
	if st.InProgress != nil {
		u.cutOff(ctx, st)
		if err := u.save(st); err != nil {
			return "", err
		}
	}
	approvals, err := u.Spool.TakeApprovals(u.Owners.ListenerUID, 0)
	if err != nil {
		return "", err
	}
	requests, bad, err := u.Spool.TakeChecks(u.Owners.ListenerUID)
	if err != nil {
		return "", err
	}
	for _, b := range bad {
		u.Log.Warn("a bad file in the update requests, removed", "err", b)
	}
	if !ValidVersion(u.Version) {
		// A build from the source cannot know which release is newer.
		u.Log.Info("this pullgate has no release version, so it does not update itself", "version", u.Version)
		if requests > 0 || len(approvals) > 0 {
			u.notice("This pullgate is a build from the source (version " + message.Code(u.Version) + "), so it does not update itself. Install a release with <code>install.sh</code> (README, section Install).")
		}
		return ResDev, nil
	}
	if u.expire(st) {
		if err := u.save(st); err != nil {
			return "", err
		}
	}
	for _, a := range approvals {
		if a.Err != nil {
			u.Log.Warn("a bad approval file, removed", "file", a.Name, "err", a.Err)
			continue
		}
		res, err := u.approval(ctx, st, a.Approval)
		if err != nil || res == ResInstalled {
			// At most one install per start; the new binary handles the
			// next request. Other approvals were removed already: their
			// question is gone with this one.
			if requests > 0 && err == nil {
				u.notice("The update ran, so the /update sent with it was not handled. Send <code>/update</code> again.")
			}
			return res, err
		}
		if requests == 0 && res != ResNothing {
			return res, nil
		}
	}
	if requests > 0 {
		now := u.now()
		if d := now.Sub(st.CheckAt); d < CheckInterval && d > -CheckInterval {
			u.Log.Info("an /update request less than a minute after the last one, dropped")
			if d := now.Sub(st.CheckNote); d >= CheckInterval || d <= -CheckInterval {
				st.CheckNote = now
				u.notice("A look for a new release ran less than a minute ago, so this /update was dropped.")
			}
			return ResNothing, u.save(st)
		}
		st.CheckAt = now
		if err := u.save(st); err != nil {
			return "", err
		}
		return u.look(ctx, st, true)
	}
	if len(approvals) > 0 || u.Spool.HasCheck() {
		// Started for a tap, or a request came in the meantime (the path
		// unit starts again for it): not the daily look.
		return ResNothing, nil
	}
	return u.look(ctx, st, false)
}

// expire closes the open question when its time is up.
func (u *Updater) expire(st *State) bool {
	q := st.Question
	if q == nil || u.now().Before(q.Expires) {
		return false
	}
	u.edit(q.ID, "Expired: the update to "+message.Code(q.Version)+" was not answered in time. Send <code>/update</code> to ask again.")
	st.Question = nil
	return true
}

// cutOff reports an update that a crash or a reboot cut off. The binary
// is the old file or the new one, never a part of one (the rename). The
// owner checks which one runs.
func (u *Updater) cutOff(ctx context.Context, st *State) {
	ip := st.InProgress
	st.InProgress = nil
	says := "it does not run"
	if out, err := u.binVersion(ctx, u.Bin); err == nil {
		says = out
	}
	u.Log.Error("an update was cut off", "version", ip.Version, "from", ip.From, "binary", says)
	u.notice("⚠ The update of pullgate from " + message.Code(ip.From) + " to " + message.Code(ip.Version) +
		" was cut off (a crash or a reboot). " + message.Code(u.Bin+" version") + " says: " + message.Code(says) +
		". The units may be the old ones or the new ones. Check the host: " + message.Code("systemctl status 'pullgate-*'") +
		". Fix: run " + message.Code("install.sh --version "+ip.Version) + " as root, or send <code>/update</code>.")
}

// approval handles one approval from the inbox.
func (u *Updater) approval(ctx context.Context, st *State, a spool.Approval) (Result, error) {
	q := st.Question
	if q == nil || subtle.ConstantTimeCompare([]byte(q.Code), []byte(a.Code)) != 1 {
		u.Log.Info("an approval for no open update question, ignored")
		u.notice("A tap came for an update question that is closed. Nothing was installed. Send <code>/update</code> to ask again.")
		return ResNothing, nil
	}
	now := u.now()
	if now.Before(q.Asked) || !now.Before(q.Expires) {
		st.Question = nil
		u.reply(q.ID, "Expired: nothing was installed. Send <code>/update</code> to ask again.")
		return ResNothing, u.save(st)
	}
	// The code works once.
	st.Question = nil
	if err := u.save(st); err != nil {
		return "", err
	}
	if a.Action == "skip" {
		u.Log.Info("update skipped", "version", q.Version)
		u.reply(q.ID, "Skipped: nothing was installed. Send <code>/update</code> to ask again.")
		return ResSkipped, nil
	}
	// Forward only (rule 5): the version comes from root's own state.
	if !newer(q.Version, u.Version) {
		u.Log.Warn("refused: the release is not newer", "version", q.Version, "installed", u.Version)
		u.reply(q.ID, "Refused: "+message.Code(q.Version)+" is not newer than "+message.Code(u.Version)+". pullgate never installs an older version from Telegram.")
		return ResFailed, nil
	}
	return u.install(ctx, st, q)
}

// found is what install.sh --check-only found.
type found struct {
	Version  string // the newest release; "" when it is not known
	Security bool
	Fetched  bool // the files were downloaded: a failure after this is a failed check
	Checked  bool // every check passed
	SHA256   string
	Upto     bool // the installed version is the newest
	Reason   string
	Err      error
}

func (u *Updater) checkRelease(ctx context.Context) found {
	args := []string{"--check-only", "--bin-dir", filepath.Dir(u.Bin)}
	if u.Cfg.Updates.GH != "" {
		args = append(args, "--gh", u.Cfg.Updates.GH)
	}
	tell, reason, err := u.script(ctx, args...)
	var f found
	if v := tell["version"]; len(v) == 2 && ValidVersion(v[0]) {
		f.Version, f.Security = v[0], v[1] == "security"
	}
	_, f.Fetched = tell["fetched"]
	if err != nil {
		f.Err, f.Reason = err, reason
		return f
	}
	if _, ok := tell["up-to-date"]; ok {
		f.Upto = true
		return f
	}
	c := tell["checked"]
	if len(c) != 3 || c[0] != f.Version || !sha256RE.MatchString(c[1]) {
		f.Err, f.Reason = errors.New("install.sh --check-only gave no result"), "install.sh gave no result. This is a bug in pullgate"
		return f
	}
	f.Checked, f.SHA256 = true, c[1]
	if !newer(f.Version, u.Version) {
		f.Checked, f.Upto = false, true
	}
	return f
}

// look finds the newest release. requested: the owner sent /update; else
// the timer started it.
func (u *Updater) look(ctx context.Context, st *State, requested bool) (Result, error) {
	if !requested && !u.Cfg.Updates.Notices() {
		u.Log.Info("the daily look for a new release is off ([updates] check = false)")
		return ResNothing, nil
	}
	f := u.checkRelease(ctx)
	now := u.now()
	switch {
	case f.Err != nil && !f.Fetched:
		// The network, GitHub, or this host: no release to name.
		u.Log.Warn("could not look for a new release", "err", f.Err, "reason", f.Reason)
		if requested {
			u.notice("Could not look for a new release: " + message.Code(f.Reason) + ". See the journal: <code>journalctl -u pullgate-update.service</code>")
		}
		return ResChecked, nil
	case f.Err != nil:
		u.Log.Error("a release failed a check", "version", f.Version, "reason", f.Reason)
		if st.Warned == nil {
			st.Warned = map[string]time.Time{}
		}
		_, warned := st.Warned[f.Version]
		if requested || !warned {
			st.Warned[f.Version] = now
			name := "The newest release"
			if f.Version != "" {
				name = "pullgate " + message.Code(f.Version)
			}
			u.notice("⚠ " + name + " failed a check, so it is not offered: " + message.Code(f.Reason) +
				". Do not install it by hand. See the journal: <code>journalctl -u pullgate-update.service</code>")
		}
		return ResNoticed, u.save(st)
	case f.Upto:
		u.Log.Info("pullgate is up to date", "version", u.Version)
		if q := st.Question; q != nil {
			u.edit(q.ID, "Closed: "+message.Code(q.Version)+" is not newer than the installed "+message.Code(u.Version)+" any more.")
			st.Question = nil
		}
		if requested {
			u.notice("pullgate " + message.Code(u.Version) + " is installed. It is the newest release.")
		}
		return ResChecked, u.save(st)
	}
	// A newer release that passed every check.
	if q := st.Question; q != nil {
		if q.Version == f.Version && q.SHA256 == f.SHA256 {
			if requested {
				// Send it again: the listener sends it only when it does
				// not know it yet (for example it lost the message).
				u.send(spool.Message{Kind: spool.KindAsk, Question: q.ID, Code: q.Code, Expires: q.Expires, Text: u.questionText(f)})
				u.reply(q.ID, "The question for "+message.Code(q.Version)+" is still open.")
			}
			return ResChecked, nil
		}
		u.edit(q.ID, "Closed: a newer question replaced this one ("+message.Code(f.Version)+").")
		st.Question = nil
		if err := u.save(st); err != nil {
			return "", err
		}
		requested = true // the owner asked for an update before: ask again
	}
	if requested {
		return u.ask(st, f)
	}
	if st.Notified == nil {
		st.Notified = map[string]time.Time{}
	}
	last, done := st.Notified[f.Version]
	if done && (!f.Security || now.Sub(last) < RepeatSecurity) {
		return ResChecked, nil
	}
	st.Notified[f.Version] = now
	lines := []string{}
	if f.Security {
		lines = append(lines, "⚠ <b>Security release.</b> It fixes a security problem. Install it soon.")
	}
	lines = append(lines,
		"pullgate "+message.Code(f.Version)+" is out. Installed: "+message.Code(u.Version)+".",
		"Release notes: "+releaseURL(f.Version),
		"Send <code>/update</code> to install it.")
	u.notice(strings.Join(lines, "\n"))
	u.Log.Info("notice of a new release sent", "version", f.Version, "security", f.Security)
	return ResNoticed, u.save(st)
}

// ask records the question in the state, then writes it to the outbox.
func (u *Updater) ask(st *State, f found) (Result, error) {
	c, err := state.NewCode()
	if err != nil {
		return "", err
	}
	now := u.now()
	q := &Question{ID: st.NextID, Code: c, Version: f.Version, SHA256: f.SHA256, Asked: now, Expires: now.Add(QuestionTTL)}
	st.NextID++
	st.Question = q
	if err := u.save(st); err != nil {
		return "", err
	}
	m := spool.Message{Kind: spool.KindAsk, Question: q.ID, Code: c, Expires: q.Expires, Text: u.head() + "\n" + u.questionText(f)}
	if _, err := u.Spool.Send(u.Owners.ListenerGID, m); err != nil {
		st.Question = nil
		_ = u.save(st)
		return "", fmt.Errorf("write the update question to the outbox: %w", err)
	}
	u.Log.Info("asked", "version", f.Version, "sha256", f.SHA256, "question", q.ID)
	return ResAsked, nil
}

// questionText is the question, without the head.
func (u *Updater) questionText(f found) string {
	var lines []string
	if f.Security {
		lines = append(lines, "⚠ <b>Security release.</b> It fixes a security problem.")
	}
	lines = append(lines,
		"Update pullgate to "+message.Code(f.Version)+"?",
		"Installed now: "+message.Code(u.Version),
		"Release notes: "+releaseURL(f.Version),
		"File: "+message.Code(u.tarball(f.Version)),
		"SHA-256: "+message.Code(f.SHA256),
		"",
		"The SHA-256 and the build attestation passed. Apply installs only this file, after the same checks again, then the units of the new version, and restarts them.")
	return strings.Join(lines, "\n")
}

// capBuf keeps at most max bytes, and copies everything to w.
type capBuf struct {
	b   []byte
	max int
	w   io.Writer
	cut bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	if c.w != nil {
		_, _ = c.w.Write(p)
	}
	room := c.max - len(c.b)
	if len(p) > room {
		c.cut = true
	}
	if room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// command makes a command with an empty environment (PATH and LC_ALL
// only, plus the proxy variables), in its own process group, which is
// killed when ctx ends.
func command(ctx context.Context, path string, extra []string, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // /bin/sh with the built-in install.sh, or the binary that install.sh checked
	cmd.Env = append([]string{"PATH=" + path, "LC_ALL=C"}, extra...)
	for _, k := range []string{"https_proxy", "HTTPS_PROXY", "no_proxy", "NO_PROXY"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Dir = "/"
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// script runs the built-in install.sh with args. It returns the
// "@pullgate WORD VALUES" lines by WORD, and the reason of a failure: the
// last line of install.sh, without "pullgate: " and "(exit 1)".
func (u *Updater) script(parent context.Context, args ...string) (map[string][]string, string, error) {
	ctx, cancel := context.WithTimeout(parent, scriptTimeout)
	defer cancel()
	out := u.Out
	if out == nil {
		out = io.Discard
	}
	stdout := &capBuf{max: maxOutput, w: out}
	stderr := &capBuf{max: maxOutput, w: out}
	tmp := u.tmpDir()
	cmd := command(ctx, u.Path, []string{"TMPDIR=" + tmp, "HOME=" + tmp}, "/bin/sh", append([]string{"-c", u.Script, "install.sh"}, args...)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	tell := map[string][]string{}
	for line := range strings.SplitSeq(string(stdout.b), "\n") {
		if rest, ok := strings.CutPrefix(line, "@pullgate "); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				tell[f[0]] = f[1:]
			}
		}
	}
	if err == nil {
		return tell, "", nil
	}
	reason := "install.sh failed: " + err.Error()
	if ctx.Err() != nil {
		reason = "install.sh ran longer than " + scriptTimeout.String()
	}
	lines := strings.Split(strings.TrimSpace(string(stderr.b)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); strings.HasPrefix(last, "pullgate: ") {
		last = strings.TrimPrefix(last, "pullgate: ")
		last = strings.TrimSuffix(last, " (exit 1)")
		last = strings.TrimSuffix(last, "; nothing was installed")
		reason = last
	}
	return tell, reason, fmt.Errorf("install.sh %s: %w", strings.Join(args, " "), err)
}

// run runs a program with no arguments from the network, at most
// runTimeout, and returns its standard output.
func (u *Updater) run(parent context.Context, max int, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, runTimeout)
	defer cancel()
	stdout := &capBuf{max: max}
	cmd := command(ctx, RootPath, nil, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = u.Out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if stdout.cut {
		return "", fmt.Errorf("%s %s printed more than %d bytes", name, strings.Join(args, " "), max)
	}
	return string(stdout.b), nil
}

// binVersion runs FILE version, and returns what it printed, on one line.
func (u *Updater) binVersion(ctx context.Context, file string) (string, error) {
	out, err := u.run(ctx, 256, file, "version")
	return strings.TrimSpace(out), err
}
