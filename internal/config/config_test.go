package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/code6x/pullgate/internal/testgit"
)

func TestMain(m *testing.M) {
	syscall.Umask(0o077)
	os.Exit(m.Run())
}

const good = `
[repo]
url = "ssh://git@git.example.com/owner/config-repo.git"
deploy_key = "/etc/pullgate/x.key"
known_hosts = "/etc/pullgate/x.known_hosts"
[commands]
apply = ["/usr/local/sbin/install-config"]
`

func write(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	d := testgit.PrivateDir(t)
	if err := os.WriteFile(filepath.Join(d, "x.toml"), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(d, "x.toml"), mode); err != nil {
		t.Fatal(err)
	}
	return d
}

func uid() uint32 { return uint32(os.Geteuid()) }

func TestDefaults(t *testing.T) {
	c, err := Load(write(t, good, 0o600), "x", uid())
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != "ask" || c.OnFailure != "pause" || c.Limits.QuestionsPerDay != 10 || c.Limits.MaxFailedTries != 1 ||
		c.Limits.ApprovalTTL.Hours() != 12 || c.Limits.AllowOlder || c.Commands.CheckUser != "pullgate-check" {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestRefused(t *testing.T) {
	cases := map[string]string{
		"unknown key":         good + "\n[limits]\nquestions = 3\n",
		"bad mode":            `mode = "yes"` + good,
		"bad on_failure":      `on_failure = "retry"` + good,
		"relative program":    strings.Replace(good, "/usr/local/sbin/install-config", "install-config", 1),
		"no apply":            strings.Replace(good, `apply = ["/usr/local/sbin/install-config"]`, "", 1),
		"check_user root":     good + "check_user = \"root\"\n",
		"https url":           strings.Replace(good, "ssh://", "https://", 1),
		"url as option":       strings.Replace(good, "ssh://git@git.example.com/owner/config-repo.git", "--upload-pack=x", 1),
		"branch option":       good + "[repo]\n",
		"ttl too long":        good + "[limits]\napproval_ttl = \"200h\"\n",
		"absolute watch path": good + "[watch]\npaths = [\"/etc/x\"]\n",
		"host html":           `host = "<b>"` + good,
	}
	for n, body := range cases {
		t.Run(n, func(t *testing.T) {
			if _, err := Load(write(t, body, 0o600), "x", uid()); err == nil {
				t.Fatal("must be refused")
			}
		})
	}
}

func TestFileChecks(t *testing.T) {
	d := write(t, good, 0o666)
	if _, err := Load(d, "x", uid()); err == nil {
		t.Fatal("a config that others can write must be refused")
	}
	d = write(t, good, 0o600)
	if _, err := Load(d, "x", uid()+1); err == nil {
		t.Fatal("a config of another user must be refused")
	}
	if _, err := Load(d, "../x", uid()); err == nil {
		t.Fatal("a bad name must be refused")
	}
}

func TestGateFiles(t *testing.T) {
	d := testgit.PrivateDir(t)
	key := filepath.Join(d, "k")
	kh := filepath.Join(d, "kh")
	prog := filepath.Join(d, "apply")
	for _, p := range []string{key, kh, prog} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := &Instance{Path: "x.toml", Repo: Repo{URL: "ssh://h/x", DeployKey: key, KnownHosts: kh}, Commands: Commands{Apply: []string{prog}}}
	if err := c.CheckGateFiles(uid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckGateFiles(uid()); err == nil {
		t.Fatal("a deploy key readable by group must be refused")
	}
	_ = os.Chmod(key, 0o600)
	if err := os.Chmod(prog, 0o722); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckGateFiles(uid()); err == nil {
		t.Fatal("a program that others can write must be refused")
	}
	_ = os.Chmod(prog, 0o700)
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(d, 0o700) }()
	if err := c.CheckGateFiles(uid()); err == nil {
		t.Fatal("a directory above that others can write must be refused")
	}
}

func TestListener(t *testing.T) {
	d := testgit.PrivateDir(t)
	tok := filepath.Join(d, "token")
	p := filepath.Join(d, "listener.toml")
	body := "token_file = \"" + tok + "\"\nowner_id = 1\nchat_id = 1\ninstances = [\"a\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tok, []byte("1:abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadListener(p, uid())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.ReadToken(uid()); err != nil || got != "1:abc" {
		t.Fatalf("token: %v", err)
	}
	if err := os.Chmod(tok, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadToken(uid()); err == nil {
		t.Fatal("a token file that others can read must be refused")
	}
	if err := os.WriteFile(p, []byte(body+"api_url = \"http://api.example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadListener(p, uid()); err == nil {
		t.Fatal("a plain http api_url must be refused")
	}
	if err := os.WriteFile(p, []byte("owner_id = 1\nchat_id = 1\ninstances = [\"a\"]\nowner = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadListener(p, uid()); err == nil {
		t.Fatal("an unknown key must be refused")
	}
}

// TestExamples checks that each example config is valid and sets
// on_failure itself: rollback for the config file, pause for ansible.
func TestExamples(t *testing.T) {
	for file, want := range map[string]string{
		"../../contrib/examples/config-file/config-repo.toml": OnFailureRollback,
		"../../contrib/examples/ansible/playbooks.toml":       OnFailurePause,
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "\non_failure = \""+want+"\"") {
			t.Errorf("%s must set on_failure = %q", file, want)
		}
		c := &Instance{Path: file}
		if err := decodeFile(file, c); err != nil {
			t.Fatal(err)
		}
		c.defaults()
		if err := c.validate(); err != nil {
			t.Errorf("%s: %v", file, err)
		}
		if c.OnFailure != want {
			t.Errorf("%s: on_failure %q", file, c.OnFailure)
		}
	}
	l := &Listener{}
	if err := decodeFile("../../contrib/examples/listener.toml", l); err != nil {
		t.Fatal(err)
	}
}
