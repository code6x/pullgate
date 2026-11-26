package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/code6x/pullgate/internal/config"
)

// Known fake values of each kind that a /doctor reply must never show.
const (
	fakeIPv4  = "192.0.2.77"
	fakeIPv6  = "2001:db8::77"
	fakeHost  = "example-host"
	fakeToken = "1234567890:AAHfakeTokenMadeByTheTestOnlyForThisTest"
	fakeKey   = "AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyMadeByTheTestOnlyForThisTest"
	fakePath  = "/home/example/.ssh/deploy_key"
	fakeURL   = "git@git.example.com:example-org/config-repo.git"
)

func withHost(t *testing.T, h string) {
	old := hostname
	hostname = func() (string, error) { return h, nil }
	t.Cleanup(func() { hostname = old })
}

func TestRedact(t *testing.T) {
	withHost(t, fakeHost+".example.com")
	own := []string{"/etc/pullgate", "/var/lib/pullgate", "/etc/systemd/system/pullgate-*"}
	in := strings.Join([]string{
		"ip " + fakeIPv4 + ", " + fakeIPv6 + " and [" + fakeIPv6 + "]:22.",
		"token " + fakeToken + " key ssh-ed25519 " + fakeKey + " -----BEGIN OPENSSH PRIVATE KEY-----",
		"host " + fakeHost + " and " + strings.ToUpper(fakeHost) + ".example.com, on " + fakeHost + ":",
		"path " + fakePath + ", and (" + fakePath + ") and /etc/pullgate/../shadow and /etc/systemd/system/other.service",
		"url " + fakeURL + " and https://git.example.com/x and ssh://git@" + fakeIPv4 + "/r",
		"keep /etc/pullgate/config-repo.toml. and /var/lib/pullgate and /etc/systemd/system/pullgate-run@.timer: pullgate 0.4.0 at 12:30, DESIGN.md section 4",
		"unit pullgate-run@config-repo.timer: not enabled",
	}, "\n")
	out := Redact(in, own)
	for _, bad := range []string{fakeIPv4, "2001:db8", fakeHost, strings.ToUpper(fakeHost), fakeToken, "AAHfake", fakeKey, "BEGIN", fakePath, "/home/", "shadow", "other.service", "git.example.com", "example-org"} {
		if strings.Contains(out, bad) {
			t.Errorf("the text still holds %q:\n%s", bad, out)
		}
	}
	for _, keep := range []string{"/etc/pullgate/config-repo.toml.", "/var/lib/pullgate ", "/etc/systemd/system/pullgate-run@.timer:", "pullgate 0.4.0", "12:30", "unit pullgate-run@config-repo.timer: not enabled", "DESIGN.md section 4", "<ip>", "<secret>", "<host>", "<path>", "<address>"} {
		if !strings.Contains(out, keep) {
			t.Errorf("the text must keep %q:\n%s", keep, out)
		}
	}
}

// TestRunPublicNeedsNoSecret: /doctor reads no token, calls no Telegram,
// runs no git with the deploy key, and shows only problems, notes and the
// summary.
func TestRunPublicNeedsNoSecret(t *testing.T) {
	f := newFix(t)
	withHost(t, fakeHost)
	f.o.Telegram = true
	f.o.Remote = func(context.Context, *config.Instance, uint32) error {
		t.Error("git with the deploy key must not run for /doctor")
		return nil
	}
	f.write("etc/bot-token", fakeToken+"\n", 0o600)
	out := RunPublic(context.Background(), f.o)
	for _, w := range []string{"All checks passed.", "checks", "note     bot token: not checked from Telegram", "git and the deploy key not checked from Telegram"} {
		if !strings.Contains(out, w) {
			t.Errorf("no %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "ok       version") || strings.Contains(out, "example_bot") || strings.Contains(out, fakeToken) || strings.Contains(out, token) {
		t.Fatalf("only problems, notes and the summary; no token, no Telegram:\n%s", out)
	}
	if f.out.Len() != 0 {
		t.Fatal("RunPublic must not write to Options.Out")
	}
}

// TestRunPublicRedactsEachKind: a problem whose text holds a known fake
// value of each kind comes out without it.
func TestRunPublicRedactsEachKind(t *testing.T) {
	f := newFix(t)
	withHost(t, fakeHost)
	leak := "failed on " + fakeHost + " " + fakeIPv4 + " " + fakeIPv6 + " " + fakePath + " " + fakeToken + " ssh-ed25519 " + fakeKey + " " + fakeURL
	f.o.Systemctl = func(_ context.Context, args ...string) (string, error) {
		return leak, errors.New("exit status 3")
	}
	out := RunPublic(context.Background(), f.o)
	if !strings.Contains(out, "PROBLEM") || !strings.Contains(out, "problem(s)") {
		t.Fatalf("want the problems:\n%s", out)
	}
	for _, bad := range []string{fakeHost, fakeIPv4, "2001:db8", fakePath, fakeToken, fakeKey, "git.example.com", f.base + "/bin"} {
		if strings.Contains(out, bad) {
			t.Errorf("the reply holds %q:\n%s", bad, out)
		}
	}
}
