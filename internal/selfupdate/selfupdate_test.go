package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/code6x/pullgate"
	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/spool"
)

// The tests run the real install.sh (built into the binary) against a fake
// release, as the tests of install.sh do: a fake curl serves the files of
// the release from a directory, a fake gh says that the attestation is
// valid or not, and each fake pullgate is a small script. They never use
// the network. They do not run as root: as root, install.sh runs only the
// programs of the system.

func TestMain(m *testing.M) {
	syscall.Umask(0o022)
	os.Exit(m.Run())
}

const fakeCurl = `#!/bin/sh
# A fake curl: https://HOST/PATH is the file WEB/HOST/PATH.
out=
url=
while [ "$#" -gt 0 ]; do
	case $1 in
	--output) out=$2; shift ;;
	esac
	url=$1
	shift
done
case $url in
https://*) ;;
*) echo "fake curl: not HTTPS: $url" >&2; exit 1 ;;
esac
file=WEB/${url#https://}
if [ ! -f "$file" ]; then
	echo "curl: (22) The requested URL returned error: 404" >&2
	exit 22
fi
cp "$file" "$out"
`

const fakeGh = `#!/bin/sh
# A fake gh: the attestation is valid unless WEB/gh-exit says otherwise.
if [ "$1" = --version ]; then
	echo "gh version 2.102.0 (2026-09-30)"
	exit 0
fi
echo "$*" >>WEB/gh-calls
exit "$(cat WEB/gh-exit 2>/dev/null || echo 0)"
`

const fakeUname = `#!/bin/sh
case $1 in
-s) echo Linux ;;
-m) echo x86_64 ;;
*) exit 1 ;;
esac
`

// fakeBin is a fake pullgate: "version" prints the line; "units" prints
// the file units.
func fakeBin(versionLine, units string) []byte {
	return fmt.Appendf(nil, "#!/bin/sh\ncase $1 in\nversion) echo '%s' ;;\nunits) cat '%s' ;;\n*) exit 1 ;;\nesac\n", versionLine, units)
}

type rig struct {
	t       *testing.T
	web     string // the fake GitHub
	fakes   string
	bin     string // DIR/pullgate
	units   string // the unit directory
	unitsV  string // the units of the new versions (JSON)
	spool   spool.Dirs
	u       *Updater
	now     time.Time
	calls   []string // systemctl calls
	mu      sync.Mutex
	enabled map[string]bool
	// listenerDown: the listener is not active after its restart, while
	// the binary says it is this version.
	listenerDown string
}

const installed = "0.2.0"

func newRig(t *testing.T) *rig {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("as root, install.sh runs only the programs of the system; the fakes would not count")
	}
	base := t.TempDir()
	r := &rig{t: t, web: filepath.Join(base, "web"), fakes: filepath.Join(base, "fakes"), units: filepath.Join(base, "systemd"),
		now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC), enabled: map[string]bool{}}
	for _, d := range []string{r.web, r.fakes, r.units, filepath.Join(base, "bin"), filepath.Join(base, "state"), filepath.Join(r.units, "pullgate-listen.service.d")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(r.fakes, "curl"), strings.ReplaceAll(fakeCurl, "WEB", r.web), 0o755)
	write(t, filepath.Join(r.fakes, "uname"), fakeUname, 0o755)
	gh := filepath.Join(r.fakes, "gh")
	write(t, gh, strings.ReplaceAll(fakeGh, "WEB", r.web), 0o755)
	r.unitsV = filepath.Join(r.web, "units.json")
	r.setUnits(pullgate.Units(), pullgate.Enable)
	// The units of the host: the listener unit is old, and has a drop-in.
	// The other units are as shipped.
	for n, c := range pullgate.Units() {
		write(t, filepath.Join(r.units, n), c, 0o644)
	}
	write(t, filepath.Join(r.units, "pullgate-listen.service"), "# an old unit\n", 0o644)
	write(t, filepath.Join(r.units, "pullgate-listen.service.d", "override.conf"), "[Service]\nRestartSec=1s\n", 0o644)
	r.bin = filepath.Join(base, "bin", "pullgate")
	write(t, r.bin, string(fakeBin("pullgate "+installed, r.unitsV)), 0o755)
	for _, u := range []string{"pullgate-run@config-repo.timer", "pullgate-apply@config-repo.path", "pullgate-check@config-repo.path", "pullgate-update.timer", "pullgate-update.path"} {
		r.enabled[u] = true
	}
	yes := true
	cfg := &config.Listener{Instances: []string{"config-repo"}, SpoolDir: filepath.Join(base, "spool"), Updates: config.Updates{Check: &yes, ListenerUser: "pullgate", GH: gh}}
	r.spool = spool.Dirs{Base: cfg.SpoolDir, Name: spool.SelfName}
	r.u = &Updater{
		Cfg: cfg, StateDir: filepath.Join(base, "state", spool.SelfName), Spool: r.spool,
		Owners: spool.Owners{ListenerUID: os.Geteuid(), ListenerGID: os.Getegid()}, UID: uint32(os.Geteuid()),
		Bin: r.bin, Version: installed, Script: pullgate.InstallScript, UnitDir: r.units, Host: "example-host",
		Path: r.fakes + ":" + os.Getenv("PATH"), Arch: "amd64", Systemctl: r.systemctl,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Out: io.Discard, Now: func() time.Time { return r.now },
	}
	if err := r.u.ensure(); err != nil {
		t.Fatal(err)
	}
	return r
}

func write(t *testing.T, p, s string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) setUnits(units map[string]string, enable []string) {
	b, err := json.Marshal(UnitList{Units: units, Enable: enable})
	if err != nil {
		r.t.Fatal(err)
	}
	write(r.t, r.unitsV, string(b), 0o644)
}

func (r *rig) systemctl(_ context.Context, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, strings.Join(args, " "))
	switch args[0] {
	case "is-enabled":
		if !r.enabled[args[len(args)-1]] {
			return errors.New("disabled")
		}
	case "is-active":
		out, _ := exec.Command(r.bin, "version").Output()
		if r.listenerDown != "" && strings.TrimSpace(string(out)) == "pullgate "+r.listenerDown {
			return errors.New("inactive")
		}
	}
	return nil
}

// release is one fake release.
type release struct {
	version  string
	binLine  string // what its pullgate version prints; default "pullgate VERSION"
	security bool
	badSum   bool // SHA256SUMS has another SHA-256
	license  string
}

// publish puts a release on the fake GitHub, as the newest one, and
// returns the SHA-256 of its tarball.
func (r *rig) publish(rel release) string {
	r.t.Helper()
	if rel.binLine == "" {
		rel.binLine = "pullgate " + rel.version
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
		mode int64
	}{{"pullgate", fakeBin(rel.binLine, r.unitsV), 0o755}, {"LICENSE", []byte("license\n" + rel.license), 0o644}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			r.t.Fatal(err)
		}
		_, _ = tw.Write(f.body)
	}
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	hexSum := hex.EncodeToString(sum[:])
	listed := hexSum
	if rel.badSum {
		listed = strings.Repeat("0", 64)
	}
	dl := filepath.Join(r.web, "github.com/code6x/pullgate/releases/download/v"+rel.version)
	api := filepath.Join(r.web, "api.github.com/repos/code6x/pullgate/releases")
	for _, d := range []string{dl, filepath.Join(api, "tags")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	tarball := "pullgate_" + rel.version + "_linux_amd64.tar.gz"
	write(r.t, filepath.Join(dl, tarball), buf.String(), 0o644)
	write(r.t, filepath.Join(dl, "SHA256SUMS"), listed+"  "+tarball+"\n", 0o644)
	write(r.t, filepath.Join(dl, "pullgate_"+rel.version+".sigstore.json"), "{}\n", 0o644)
	body := `## [` + rel.version + `](https://example.com)\n\n### Bug Fixes\n\n* a fix\n`
	if rel.security {
		body = `## [` + rel.version + `](https://example.com)\n\n### Security\n\n* a fix\n`
	}
	js := `{"tag_name": "v` + rel.version + `", "body": "` + body + `"}` + "\n"
	write(r.t, filepath.Join(api, "latest"), js, 0o644)
	write(r.t, filepath.Join(api, "tags", "v"+rel.version), js, 0o644)
	return hexSum
}

func (r *rig) run() Result {
	r.t.Helper()
	res, err := r.u.Run(context.Background())
	if err != nil {
		r.t.Fatalf("Run: %v", err)
	}
	return res
}

// outbox takes the messages that root wrote.
func (r *rig) outbox() []spool.Message {
	r.t.Helper()
	files, err := r.spool.ReadOutbox()
	if err != nil {
		r.t.Fatal(err)
	}
	var out []spool.Message
	for _, f := range files {
		if f.Err != nil {
			r.t.Fatalf("a bad outbox file: %v", f.Err)
		}
		out = append(out, f.Message)
		if err := r.spool.Done(f.Name); err != nil {
			r.t.Fatal(err)
		}
	}
	return out
}

// update sends /update and runs root.
func (r *rig) update() (Result, []spool.Message) {
	r.t.Helper()
	if err := r.spool.WriteCheck(); err != nil {
		r.t.Fatal(err)
	}
	r.now = r.now.Add(CheckInterval)
	res := r.run()
	return res, r.outbox()
}

// tap answers the open question as the listener does, and runs root.
func (r *rig) tap(code, action string) (Result, []spool.Message) {
	r.t.Helper()
	if err := r.spool.WriteApproval(spool.Approval{Code: code, Action: action}); err != nil {
		r.t.Fatal(err)
	}
	res := r.run()
	return res, r.outbox()
}

func (r *rig) binSays() string {
	r.t.Helper()
	out, err := exec.Command(r.bin, "version").Output()
	if err != nil {
		r.t.Fatalf("%s does not run: %v", r.bin, err)
	}
	return strings.TrimSpace(string(out))
}

func one(t *testing.T, ms []spool.Message, kind string, want ...string) spool.Message {
	t.Helper()
	if len(ms) != 1 || ms[0].Kind != kind {
		t.Fatalf("want one %s message, got %+v", kind, ms)
	}
	for _, w := range want {
		if !strings.Contains(ms[0].Text, w) {
			t.Fatalf("the %s must hold %q:\n%s", kind, w, ms[0].Text)
		}
	}
	return ms[0]
}

// ask publishes the release and sends /update; it returns the question.
func (r *rig) ask(rel release) spool.Message {
	r.t.Helper()
	sum := r.publish(rel)
	res, ms := r.update()
	if res != ResAsked {
		r.t.Fatalf("/update: got %q, want %q (%+v)", res, ResAsked, ms)
	}
	return one(r.t, ms, spool.KindAsk, "<code>"+rel.version+"</code>", "<code>"+sum+"</code>",
		"https://github.com/code6x/pullgate/releases/tag/v"+rel.version, "pullgate_"+rel.version+"_linux_amd64.tar.gz")
}

func TestUpdateAsksAndInstalls(t *testing.T) {
	r := newRig(t)
	q := r.ask(release{version: "0.3.0"})
	if r.binSays() != "pullgate "+installed {
		t.Fatal("a question must install nothing")
	}
	res, ms := r.tap(q.Code, "apply")
	if res != ResInstalled {
		t.Fatalf("got %q, want %q: %+v", res, ResInstalled, ms)
	}
	m := one(t, ms, spool.KindReply, "updated", "<code>0.2.0</code> → <code>0.3.0</code>", "<code>pullgate-listen.service</code>",
		"<code>pullgate-listen.service.d/override.conf</code>")
	if m.Question != q.Question {
		t.Fatal("the report must answer the question")
	}
	if r.binSays() != "pullgate 0.3.0" {
		t.Fatalf("the binary says %q, want 0.3.0", r.binSays())
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(r.bin), ".pullgate.prev")); err == nil {
		t.Fatal("the old binary must be removed after a good update")
	}
	// The unit that differed is the shipped one now, and the drop-in is
	// untouched.
	got, _ := os.ReadFile(filepath.Join(r.units, "pullgate-listen.service"))
	if string(got) != pullgate.Units()["pullgate-listen.service"] {
		t.Fatal("the listener unit must be the one of the new version")
	}
	if got, _ := os.ReadFile(filepath.Join(r.units, "pullgate-listen.service.d", "override.conf")); string(got) != "[Service]\nRestartSec=1s\n" {
		t.Fatal("a drop-in must not be changed")
	}
	for _, want := range []string{"daemon-reload", "reenable pullgate-run@config-repo.timer", "restart pullgate-run@config-repo.timer",
		"reenable pullgate-update.path", "reenable pullgate-listen.service", "restart pullgate-listen.service", "is-active --quiet pullgate-listen.service"} {
		if !slices.Contains(r.calls, want) {
			t.Errorf("systemctl %s was not called: %q", want, r.calls)
		}
	}
	if i, j := slices.Index(r.calls, "daemon-reload"), slices.Index(r.calls, "restart pullgate-listen.service"); i < 0 || j < i ||
		r.calls[len(r.calls)-1] != "is-active --quiet pullgate-listen.service" {
		t.Errorf("the reload comes first and the listener last: %q", r.calls)
	}
	// The code works once.
	if res, ms := r.tap(q.Code, "apply"); res != ResNothing || len(ms) != 1 || strings.Contains(ms[0].Text, "updated") {
		t.Fatalf("a second tap must install nothing: %q %+v", res, ms)
	}
}

// TestUnitUpdate: a new unit is installed and enabled; a unit that the
// owner turned off stays off; a masked unit is left alone.
func TestUnitUpdate(t *testing.T) {
	r := newRig(t)
	units := pullgate.Units()
	units["pullgate-new.timer"] = "[Timer]\nOnCalendar=daily\n\n[Install]\nWantedBy=timers.target\n"
	units["pullgate-run@.service"] += "# changed\n"
	r.setUnits(units, append(slices.Clone(pullgate.Enable), "pullgate-new.timer"))
	r.enabled["pullgate-update.timer"] = false
	if err := os.Remove(filepath.Join(r.units, "pullgate-check@.path")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(r.units, "pullgate-check@.path")); err != nil {
		t.Fatal(err)
	}
	q := r.ask(release{version: "0.3.0"})
	res, ms := r.tap(q.Code, "apply")
	if res != ResInstalled {
		t.Fatalf("got %q: %+v", res, ms)
	}
	one(t, ms, spool.KindReply, "<code>pullgate-new.timer</code>", "<code>pullgate-run@.service</code>", "links", "<code>pullgate-check@.path</code>")
	if got, _ := os.ReadFile(filepath.Join(r.units, "pullgate-new.timer")); string(got) != units["pullgate-new.timer"] {
		t.Fatal("a new unit must be installed")
	}
	if l, err := os.Readlink(filepath.Join(r.units, "pullgate-check@.path")); err != nil || l != "/dev/null" {
		t.Fatal("a masked unit must stay masked")
	}
	if !slices.Contains(r.calls, "reenable pullgate-new.timer") {
		t.Errorf("a new unit must be enabled: %q", r.calls)
	}
	if slices.Contains(r.calls, "reenable pullgate-update.timer") {
		t.Errorf("a unit that the owner turned off must stay off: %q", r.calls)
	}
}

func TestUpdateDenyPaths(t *testing.T) {
	for _, tt := range []struct {
		name string
		rel  release
		gh   string // the exit of gh attestation verify
		want string
	}{
		{name: "wrong SHA-256", rel: release{version: "0.3.0", badSum: true}, want: "SHA-256"},
		{name: "failed attestation", rel: release{version: "0.3.0"}, gh: "1", want: "attestation"},
	} {
		t.Run(tt.name+" before the question", func(t *testing.T) {
			r := newRig(t)
			r.publish(tt.rel)
			if tt.gh != "" {
				write(t, filepath.Join(r.web, "gh-exit"), tt.gh+"\n", 0o644)
			}
			res, ms := r.update()
			if res != ResNoticed {
				t.Fatalf("got %q: %+v", res, ms)
			}
			one(t, ms, spool.KindNotice, "failed a check", "not offered", tt.want)
			if strings.Contains(ms[0].Text, "/update</code> to install") {
				t.Fatal("a release that failed a check must never get the /update hint")
			}
			// The daily look warns once, not every day.
			r.now = r.now.Add(24 * time.Hour)
			if res := r.run(); res != ResNoticed || len(r.outbox()) != 0 {
				t.Fatal("the warning must come once")
			}
			if r.binSays() != "pullgate "+installed {
				t.Fatal("nothing must be installed")
			}
		})
		t.Run(tt.name+" on Apply", func(t *testing.T) {
			r := newRig(t)
			q := r.ask(release{version: "0.3.0"})
			// The release changes after the question.
			r.publish(tt.rel)
			if tt.gh != "" {
				write(t, filepath.Join(r.web, "gh-exit"), tt.gh+"\n", 0o644)
			}
			res, ms := r.tap(q.Code, "apply")
			if res != ResFailed {
				t.Fatalf("got %q: %+v", res, ms)
			}
			one(t, ms, spool.KindReply, "failed", "Nothing was installed")
			if r.binSays() != "pullgate "+installed {
				t.Fatal("nothing must be installed")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(r.bin), ".pullgate.prev")); err == nil {
				t.Fatal("the kept old binary must be removed when nothing was installed")
			}
		})
	}
}

// TestApplyInstallsOnlyTheApprovedFile: a release whose tarball changed
// after the question (and still passes every other check) is refused.
func TestApplyInstallsOnlyTheApprovedFile(t *testing.T) {
	r := newRig(t)
	q := r.ask(release{version: "0.3.0"})
	r.publish(release{version: "0.3.0", license: "changed\n"}) // another file, same version
	res, ms := r.tap(q.Code, "apply")
	if res != ResFailed {
		t.Fatalf("got %q: %+v", res, ms)
	}
	one(t, ms, spool.KindReply, "not the one that was approved")
	if r.binSays() != "pullgate "+installed {
		t.Fatal("nothing must be installed")
	}
}

func TestOlderVersionIsNeverInstalled(t *testing.T) {
	r := newRig(t)
	r.publish(release{version: "0.1.1"})
	res, ms := r.update()
	if res != ResChecked {
		t.Fatalf("got %q", res)
	}
	one(t, ms, spool.KindNotice, "It is the newest release")
	// A question for an older version in root's state (the state is
	// root's own; this stands for a bug) is refused on Apply.
	r.publish(release{version: "0.3.0"})
	q := r.ask(release{version: "0.3.0"})
	r.u.Version = "0.4.0"
	res, ms = r.tap(q.Code, "apply")
	if res != ResFailed {
		t.Fatalf("got %q", res)
	}
	one(t, ms, spool.KindReply, "Refused", "never installs an older version")
	if r.binSays() != "pullgate "+installed {
		t.Fatal("nothing must be installed")
	}
}

func TestNewerReleaseClosesTheOpenQuestion(t *testing.T) {
	r := newRig(t)
	q1 := r.ask(release{version: "0.3.0"})
	sum := r.publish(release{version: "0.4.0"})
	// The daily look finds it, closes the old question and asks again.
	r.now = r.now.Add(time.Hour)
	if res := r.run(); res != ResAsked {
		t.Fatalf("got %q", res)
	}
	ms := r.outbox()
	if len(ms) != 2 || ms[0].Kind != spool.KindEdit || ms[0].Question != q1.Question || ms[1].Kind != spool.KindAsk ||
		!strings.Contains(ms[1].Text, sum) {
		t.Fatalf("want the edit of the old question and a new one: %+v", ms)
	}
	// A tap on the old question installs nothing.
	if res, ms := r.tap(q1.Code, "apply"); res != ResNothing || len(ms) != 1 || !strings.Contains(ms[0].Text, "closed") {
		t.Fatalf("a tap on the closed question must install nothing: %q %+v", res, ms)
	}
	if res, _ := r.tap(ms[1].Code, "apply"); res != ResInstalled || r.binSays() != "pullgate 0.4.0" {
		t.Fatalf("the new question installs 0.4.0: %q %s", res, r.binSays())
	}
}

func TestSkip(t *testing.T) {
	r := newRig(t)
	q := r.ask(release{version: "0.3.0"})
	res, ms := r.tap(q.Code, "skip")
	if res != ResSkipped {
		t.Fatalf("got %q", res)
	}
	one(t, ms, spool.KindReply, "Skipped", "nothing was installed")
	if r.binSays() != "pullgate "+installed || len(r.calls) != 0 {
		t.Fatal("Skip must install and restart nothing")
	}
}

func TestExpiredQuestion(t *testing.T) {
	r := newRig(t)
	q := r.ask(release{version: "0.3.0"})
	r.now = r.now.Add(QuestionTTL)
	res, ms := r.tap(q.Code, "apply")
	if res != ResNothing || r.binSays() != "pullgate "+installed {
		t.Fatalf("an expired question must install nothing: %q", res)
	}
	if len(ms) != 2 || ms[0].Kind != spool.KindEdit || !strings.Contains(ms[0].Text, "Expired") {
		t.Fatalf("want the edit that it expired: %+v", ms)
	}
}

// TestNewBinaryDoesNotStart: the listener of the new version does not
// stay active, so the old binary and the old units come back.
func TestNewBinaryDoesNotStart(t *testing.T) {
	r := newRig(t)
	r.listenerDown = "0.3.0"
	before, _ := os.ReadFile(filepath.Join(r.units, "pullgate-listen.service"))
	q := r.ask(release{version: "0.3.0"})
	res, ms := r.tap(q.Code, "apply")
	if res != ResFailed {
		t.Fatalf("got %q: %+v", res, ms)
	}
	one(t, ms, spool.KindReply, "failed", "is not active", "pullgate <code>0.2.0</code> is back")
	if r.binSays() != "pullgate "+installed {
		t.Fatalf("the old binary must be back: %s", r.binSays())
	}
	if got, _ := os.ReadFile(filepath.Join(r.units, "pullgate-listen.service")); !bytes.Equal(got, before) {
		t.Fatal("the old units must be back")
	}
	if n := strings.Count(strings.Join(r.calls, "\n"), "restart pullgate-listen.service"); n != 2 {
		t.Fatalf("the listener must be restarted again with the old binary: %q", r.calls)
	}
	st, _ := r.u.load()
	if st.InProgress != nil {
		t.Fatal("the rollback must clear the update in progress")
	}
}

// TestCrashInTheSwap (THREAT-MODEL.md T10): after a crash at each step,
// the binary is the old file or the new one, never none or a part of one,
// and the next start reports the cut-off update.
func TestCrashInTheSwap(t *testing.T) {
	for _, step := range []string{"kept", "installed", "units"} {
		t.Run(step, func(t *testing.T) {
			r := newRig(t)
			q := r.ask(release{version: "0.3.0"})
			r.u.Crash = func(s string) bool { return s == step }
			if err := r.spool.WriteApproval(spool.Approval{Code: q.Code, Action: "apply"}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.u.Run(context.Background()); !errors.Is(err, errCrash) {
				t.Fatalf("want the crash, got %v", err)
			}
			if says := r.binSays(); says != "pullgate 0.2.0" && says != "pullgate 0.3.0" {
				t.Fatalf("after a crash the binary says %q", says)
			}
			// No temporary file of the swap is left as the binary.
			r.u.Crash = nil
			res := r.run()
			ms := r.outbox()
			if res != ResChecked && res != ResNothing && res != ResNoticed {
				t.Fatalf("got %q", res)
			}
			if len(ms) == 0 || !strings.Contains(ms[0].Text, "was cut off") {
				t.Fatalf("the next start must report the cut-off update: %+v", ms)
			}
			if st, _ := r.u.load(); st.InProgress != nil {
				t.Fatal("the report must clear the update in progress")
			}
		})
	}
}

func TestDailyNotice(t *testing.T) {
	r := newRig(t)
	// Up to date: no notice.
	r.publish(release{version: installed})
	if res := r.run(); res != ResChecked || len(r.outbox()) != 0 {
		t.Fatalf("up to date must send nothing: %q", res)
	}
	r.publish(release{version: "0.3.0"})
	if res := r.run(); res != ResNoticed {
		t.Fatalf("got %q", res)
	}
	m := one(t, r.outbox(), spool.KindNotice, "<code>0.3.0</code> is out", "https://github.com/code6x/pullgate/releases/tag/v0.3.0", "<code>/update</code>")
	if m.Code != "" || strings.Contains(m.Text, "Security") {
		t.Fatalf("a notice asks nothing: %+v", m)
	}
	// One notice per release.
	r.now = r.now.Add(25 * time.Hour)
	if res := r.run(); res != ResChecked || len(r.outbox()) != 0 {
		t.Fatal("a release gets one notice")
	}
	// A security release: it says so first, and comes again each day.
	r.publish(release{version: "0.3.1", security: true})
	r.run()
	m = one(t, r.outbox(), spool.KindNotice, "Security release", "<code>0.3.1</code> is out")
	if strings.Index(m.Text, "Security release") > strings.Index(m.Text, "is out") {
		t.Fatal("a security release must say so first")
	}
	r.now = r.now.Add(time.Hour)
	if r.run(); len(r.outbox()) != 0 {
		t.Fatal("not again within a day")
	}
	r.now = r.now.Add(RepeatSecurity)
	r.run()
	one(t, r.outbox(), spool.KindNotice, "Security release")
	// check = false stops the notice.
	no := false
	r.u.Cfg.Updates.Check = &no
	r.publish(release{version: "0.4.0"})
	r.now = r.now.Add(RepeatSecurity)
	if res := r.run(); res != ResNothing || len(r.outbox()) != 0 {
		t.Fatal("check = false must stop the notices")
	}
	if _, err := os.Stat(filepath.Join(r.web, "gh-calls")); err != nil {
		t.Fatal("the notice must come only after the attestation check")
	}
}

func TestUpdateRequestLimit(t *testing.T) {
	r := newRig(t)
	r.publish(release{version: installed})
	r.update()
	if err := r.spool.WriteCheck(); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(time.Second)
	if res := r.run(); res != ResNothing {
		t.Fatalf("a second /update within a minute must be dropped: %q", res)
	}
	one(t, r.outbox(), spool.KindNotice, "less than a minute ago")
}

func TestDevBuildDoesNotUpdate(t *testing.T) {
	r := newRig(t)
	r.u.Version = "dev"
	r.publish(release{version: "0.3.0"})
	res, ms := r.update()
	if res != ResDev {
		t.Fatalf("got %q", res)
	}
	one(t, ms, spool.KindNotice, "build from the source")
	if _, err := os.Stat(filepath.Join(r.web, "gh-calls")); err == nil {
		t.Fatal("a dev build must not look for a release")
	}
}

func TestUnitListCheck(t *testing.T) {
	for _, tt := range []UnitList{
		{},
		{Units: map[string]string{"../evil.service": "x"}},
		{Units: map[string]string{"sshd.service": "x"}},
		{Units: map[string]string{"pullgate-a.service": "x\x00"}},
		{Units: map[string]string{"pullgate-a.service": "x"}, Enable: []string{"pullgate-b.service"}},
		{Units: map[string]string{"pullgate-a@.service": "x"}, Enable: []string{"pullgate-a@x;y.service"}},
	} {
		if err := tt.check(); err == nil {
			t.Errorf("%+v must be refused", tt)
		}
	}
	ok := UnitList{Units: pullgate.Units(), Enable: pullgate.Enable}
	if err := ok.check(); err != nil {
		t.Fatalf("the units of this version must pass: %v", err)
	}
}

func TestNewer(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{{"0.3.0", "0.2.0", true}, {"0.10.0", "0.9.0", true}, {"1.0.0", "0.99.99", true}, {"0.2.0", "0.2.0", false},
		{"0.1.9", "0.2.0", false}, {"dev", "0.1.0", false}, {"0.3.0", "dev", false}, {"0.3", "0.2.0", false}, {"01.0.0", "0.2.0", false}} {
		if got := newer(tt.a, tt.b); got != tt.want {
			t.Errorf("newer(%q, %q) = %v", tt.a, tt.b, got)
		}
	}
}

// TestWrongVersion: no code of a release runs before the tap; on Apply, a
// binary that does not say it is the version asked for is not installed.
func TestWrongVersion(t *testing.T) {
	r := newRig(t)
	q := r.ask(release{version: "0.3.0", binLine: "pullgate 0.2.9"})
	res, ms := r.tap(q.Code, "apply")
	if res != ResFailed {
		t.Fatalf("got %q: %+v", res, ms)
	}
	one(t, ms, spool.KindReply, "does not say that it is pullgate 0.3.0", "Nothing was installed")
	if r.binSays() != "pullgate "+installed {
		t.Fatal("nothing must be installed")
	}
}

// TestRollbackOfANewTemplate: a template unit that is new in the release
// is disabled per instance and removed when the update rolls back.
func TestRollbackOfANewTemplate(t *testing.T) {
	r := newRig(t)
	r.listenerDown = "0.3.0"
	units := pullgate.Units()
	units["pullgate-x@.path"] = "[Path]\nPathExists=/x\n\n[Install]\nWantedBy=paths.target\n"
	r.setUnits(units, append(slices.Clone(pullgate.Enable), "pullgate-x@NAME.path"))
	q := r.ask(release{version: "0.3.0"})
	if res, _ := r.tap(q.Code, "apply"); res != ResFailed {
		t.Fatalf("got %q", res)
	}
	if !slices.Contains(r.calls, "disable --now pullgate-x@config-repo.path") {
		t.Errorf("the new instance must be disabled: %q", r.calls)
	}
	if _, err := os.Lstat(filepath.Join(r.units, "pullgate-x@.path")); err == nil {
		t.Error("the new template must be removed")
	}
}

func TestCapBuf(t *testing.T) {
	c := &capBuf{max: 10}
	_, _ = c.Write([]byte("123456789"))
	if c.cut {
		t.Fatal("9 bytes fit in 10")
	}
	_, _ = c.Write([]byte("ab"))
	if !c.cut || len(c.b) != 10 {
		t.Fatalf("11 bytes do not fit: %v %d", c.cut, len(c.b))
	}
}
