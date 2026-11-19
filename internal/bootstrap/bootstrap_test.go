package bootstrap

import (
	"bytes"
	"context"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/telegram/faketg"
	"github.com/code6x/pullgate/internal/testgit"
)

// The token is made by the test.
const token = "123456:test-token-made-by-the-test-only"

const owner = 4242

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fx struct {
	t   *testing.T
	o   *Options
	tg  *faketg.Server
	out *syncBuf
	etc string
	r   *testgit.Remote
}

func newFx(t *testing.T) *fx {
	t.Helper()
	testgit.NeedGit(t)
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	f := &fx{t: t, tg: faketg.New(t, token), out: &syncBuf{}, etc: testgit.PrivateDir(t), r: testgit.New(t)}
	f.r.Commit("first", map[string]string{"value": "one\n"})
	o, err := Parse([]string{"--token-stdin", "--api-url", f.tg.URL, "--instance", "config-repo", "--repo", f.r.Bare, "--apply", "/usr/local/lib/example-apply"})
	if err != nil {
		t.Fatal(err)
	}
	o.EtcDir, o.ListenerGroup = f.etc, g.Name
	o.Stdin, o.Out = strings.NewReader(token+"\n"), f.out
	o.PairTTL, o.PollSeconds, o.RemoteWait, o.RemoteEvery = 20*time.Second, 1, 0, 100*time.Millisecond
	f.o = o
	return f
}

var codeRE = regexp.MustCompile(`\n    (\d{4}) (\d{4})\n`)

// start runs bootstrap and returns the code it prints and the result.
func (f *fx) start() (string, <-chan error) {
	f.t.Helper()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), f.o) }()
	for i := 0; i < 200; i++ {
		if m := codeRE.FindStringSubmatch(f.out.String()); m != nil {
			return m[1] + m[2], done
		}
		select {
		case err := <-done:
			f.t.Fatalf("bootstrap ended before the code: %v\n%s", err, f.out.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	f.t.Fatalf("no code printed:\n%s", f.out.String())
	return "", nil
}

func wait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("bootstrap did not end")
		return nil
	}
}

func (f *fx) listener() config.Listener {
	f.t.Helper()
	var l config.Listener
	if _, err := toml.DecodeFile(filepath.Join(f.etc, "listener.toml"), &l); err != nil {
		f.t.Fatal(err)
	}
	return l
}

func (f *fx) nothingWritten() {
	f.t.Helper()
	for _, n := range []string{"listener.toml", "bot-token", "config-repo.toml"} {
		if _, err := os.Lstat(filepath.Join(f.etc, n)); err == nil {
			f.t.Fatalf("%s was written", n)
		}
	}
}

func TestPairRightCode(t *testing.T) {
	f := newFx(t)
	code, done := f.start()
	f.tg.Say(owner, owner, "/start", false) // not a code: it does not count
	f.tg.Say(owner, owner, code[:4]+"-"+code[4:], false)
	if err := wait(t, done); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	l := f.listener()
	if l.OwnerID != owner || l.ChatID != owner || len(l.Instances) != 1 || l.Instances[0] != "config-repo" || l.APIURL != f.tg.URL {
		t.Fatalf("listener.toml: %+v", l)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", f.etc) // as the unit gives it
	if _, err := config.LoadListener(filepath.Join(f.etc, "listener.toml"), uint32(os.Geteuid())); err != nil {
		t.Fatalf("the listener cannot read it: %v", err)
	}
	fi, err := os.Stat(filepath.Join(f.etc, "listener.toml"))
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("listener.toml mode: %v %v", fi.Mode(), err)
	}
	b, err := os.ReadFile(filepath.Join(f.etc, "bot-token"))
	fi2, _ := os.Stat(filepath.Join(f.etc, "bot-token"))
	if err != nil || strings.TrimSpace(string(b)) != token || fi2.Mode().Perm() != 0o600 {
		t.Fatal("the token file is wrong")
	}
	if _, err := config.Load(f.etc, "config-repo", uint32(os.Geteuid())); err != nil {
		t.Fatalf("the minimal instance config does not load: %v", err)
	}
	if !strings.Contains(f.out.String(), "git can read the branch main") {
		t.Fatalf("no ls-remote:\n%s", f.out.String())
	}
	if ms := f.tg.Messages(); len(ms) != 1 || ms[0].Chat != owner || !strings.Contains(ms[0].Text, "paired") {
		t.Fatalf("messages: %+v", ms)
	}
	if strings.Contains(f.out.String(), token) {
		t.Fatal("the token was printed")
	}
}

func TestPairWrongCode(t *testing.T) {
	f := newFx(t)
	code, done := f.start()
	wrong := "00000000"
	if code == wrong {
		wrong = "11111111"
	}
	for i := 0; i < MaxWrong; i++ {
		f.tg.Say(666, 666, wrong, false)
	}
	f.tg.Say(owner, owner, code, false) // too late: the pairing stopped
	err := wait(t, done)
	if err == nil || !strings.Contains(err.Error(), "wrong codes") {
		t.Fatalf("want the wrong codes error, got %v", err)
	}
	f.nothingWritten()
}

func TestPairGroupChat(t *testing.T) {
	f := newFx(t)
	code, done := f.start()
	f.tg.Say(owner, -1001, code, false)
	err := wait(t, done)
	if err == nil || !strings.Contains(err.Error(), "not from a private chat") {
		t.Fatalf("want the group error, got %v", err)
	}
	f.nothingWritten()
}

func TestPairTimeout(t *testing.T) {
	f := newFx(t)
	f.o.PairTTL = 2 * time.Second
	_, done := f.start()
	err := wait(t, done)
	if err == nil || !strings.Contains(err.Error(), "did not come in time") {
		t.Fatalf("want the timeout, got %v", err)
	}
	f.nothingWritten()
}

func TestPlantedIDsAreNotUsed(t *testing.T) {
	for _, flag := range []string{"--owner-id", "--chat-id", "--owner_id", "--config"} {
		if _, err := Parse([]string{flag, "666", "--instance", "config-repo", "--repo", "/srv/config-repo.git"}); err == nil {
			t.Fatalf("%s must be refused", flag)
		}
	}
	f := newFx(t)
	t.Setenv("PULLGATE_OWNER_ID", "666")
	planted := "owner_id = 666\nchat_id = 666\ninstances = [\"config-repo\"]\n"
	if err := os.WriteFile(filepath.Join(f.etc, "listener.toml"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	code, done := f.start()
	f.tg.Say(owner, owner, code, false)
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	if l := f.listener(); l.OwnerID != owner || l.ChatID != owner {
		t.Fatalf("the planted IDs stayed: %+v", l)
	}
}

func TestPlantedFileStaysWhenPairingFails(t *testing.T) {
	f := newFx(t)
	f.o.PairTTL = time.Second
	if err := os.WriteFile(filepath.Join(f.etc, "listener.toml"), []byte("owner_id = 666\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, done := f.start()
	if err := wait(t, done); err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(f.out.String(), "This run did not write it") {
		t.Fatalf("no note on the old file:\n%s", f.out.String())
	}
}

// script writes a program that logs its arguments and environment.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho \"args: $*\" >> "+dir+"/log\nenv >> "+dir+"/log\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTokenNeverLeaks(t *testing.T) {
	f := newFx(t)
	bin := testgit.PrivateDir(t)
	f.o.TokenStdin = false
	f.o.AskPassword = []string{script(t, bin, "ask", "echo '"+token+"'\n"), "Telegram bot token:"}
	f.o.SSHKeygen = script(t, bin, "ssh-keygen", `for a; do f=$a; done
echo private > "$f"; echo "ssh-ed25519 AAAAexample pullgate" > "$f.pub"
`)
	code, done := f.start()
	f.tg.Say(owner, owner, code, false)
	if err := wait(t, done); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	if err := f.o.deployKey(context.Background(), "config-repo", filepath.Join(f.etc, "config-repo.key")); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join(bin, "log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "args: -q -t ed25519") || !strings.Contains(string(log), "args: Telegram bot token:") {
		t.Fatalf("the programs did not run:\n%s", log)
	}
	if strings.Contains(string(log), "test-token") {
		t.Fatal("the token is in the arguments or the environment of a child")
	}
	if strings.Contains(f.out.String(), "test-token") {
		t.Fatal("the token was printed")
	}
	for _, kv := range os.Environ() {
		if strings.Contains(kv, "test-token") {
			t.Fatal("the token is in the environment")
		}
	}
	if !strings.Contains(f.out.String(), "READ-ONLY") || !strings.Contains(f.out.String(), "ssh-ed25519 AAAAexample") {
		t.Fatalf("the public key is not shown:\n%s", f.out.String())
	}
}

func TestBadToken(t *testing.T) {
	f := newFx(t)
	f.o.Stdin = strings.NewReader("not a token at all\n")
	err := Run(context.Background(), f.o)
	if err == nil || !strings.Contains(err.Error(), "not a bot token") || strings.Contains(err.Error(), "not a token at all") {
		t.Fatalf("bad token: %v", err)
	}
	f.o.Stdin = strings.NewReader("123456:wrong-token-made-by-the-test-only\n")
	if err := Run(context.Background(), f.o); err == nil || !strings.Contains(err.Error(), "getMe") || strings.Contains(err.Error(), "wrong-token") {
		t.Fatalf("a token that getMe refuses: %v", err)
	}
	f.nothingWritten()
}

func TestGitHubKey(t *testing.T) {
	fps, err := Fingerprints(GitHubKnownHosts)
	if err != nil || len(fps) != 1 || fps[0] != "ssh-ed25519 "+GitHubFingerprint {
		t.Fatalf("the carried GitHub key does not match its published fingerprint: %v %v", fps, err)
	}
	f := newFx(t)
	kh := filepath.Join(f.etc, "x.known_hosts")
	if err := f.o.knownHosts(Instance{Name: "config-repo"}, kh); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(kh); string(b) != GitHubKnownHosts {
		t.Fatal("known_hosts is not the GitHub key")
	}
	if !strings.Contains(f.out.String(), GitHubFingerprint) {
		t.Fatal("the fingerprint is not shown")
	}
	bad := filepath.Join(f.etc, "bad")
	_ = os.WriteFile(bad, []byte("git.example.com ssh-ed25519 !!!\n"), 0o600)
	if err := f.o.knownHosts(Instance{Name: "config-repo", KnownHosts: bad}, kh); err == nil {
		t.Fatal("a bad known_hosts file must be refused")
	}
}

func TestOtherHostNeedsKnownHosts(t *testing.T) {
	f := newFx(t)
	f.o.Instances[0].URL = "ssh://git@git.example.com:2222/config-repo.git"
	err := f.o.check()
	if err == nil || !strings.Contains(err.Error(), "ssh-keyscan git.example.com:2222") {
		t.Fatalf("want a known hosts error: %v", err)
	}
	f.o.Instances[0].URL = "git@github.com:example/config-repo.git"
	if err := f.o.check(); err != nil {
		t.Fatalf("github.com: %v", err)
	}
	f.o.Instances = nil
	if err := f.o.check(); err == nil {
		t.Fatal("no instance must be refused")
	}
}

func TestWaitRemote(t *testing.T) {
	f := newFx(t)
	later := filepath.Join(filepath.Dir(f.r.Bare), "later.git")
	in := Instance{Name: "config-repo", URL: later, Branch: "main"}
	if err := f.o.waitRemote(context.Background(), in, "", ""); err == nil || !strings.Contains(err.Error(), "git cannot read the repo") {
		t.Fatalf("a missing repo: %v", err)
	}
	f.o.RemoteWait = 20 * time.Second
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = os.Rename(f.r.Bare, later)
	}()
	if err := f.o.waitRemote(context.Background(), in, "", ""); err != nil {
		t.Fatalf("the repo came later: %v", err)
	}
	if !strings.Contains(f.out.String(), "waiting until git can read the repo") {
		t.Fatal("no waiting note")
	}
}

func TestAPIURLOnlyTelegramOrLoopback(t *testing.T) {
	f := newFx(t)
	for _, u := range []string{"https://tg-mirror.example", "http://127.0.0.1:80@evil.example", "http://192.0.2.1:8080", "https://user@api.telegram.org"} {
		f.o.APIURL = u
		if err := f.o.check(); err == nil {
			t.Fatalf("%s must be refused", u)
		}
	}
	f.o.APIURL = DefaultAPI
	if err := f.o.check(); err != nil {
		t.Fatalf("the real API: %v", err)
	}
}

func TestKeptConfigIsProbedWithItsOwnRepo(t *testing.T) {
	f := newFx(t)
	code, done := f.start()
	f.tg.Say(owner, owner, code, false)
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	// A second run: the config is kept, the --repo of the run is not used.
	f.out = &syncBuf{}
	f.o.Out, f.o.Stdin = f.out, strings.NewReader(token+"\n")
	f.o.Instances[0].URL = "/nonexistent/other.git"
	code, done = f.start()
	f.tg.Say(owner, owner, code, false)
	if err := wait(t, done); err != nil {
		t.Fatalf("second run: %v\n%s", err, f.out.String())
	}
	if !strings.Contains(f.out.String(), "it is kept, with its repo") {
		t.Fatalf("not kept:\n%s", f.out.String())
	}
}

func TestExistingKeyMustBeRootsFile(t *testing.T) {
	f := newFx(t)
	key := filepath.Join(f.etc, "config-repo.key")
	if err := os.Symlink("/etc/hostname", key); err != nil {
		t.Fatal(err)
	}
	if err := f.o.deployKey(context.Background(), "config-repo", key); err == nil {
		t.Fatal("a symlink as the key must be refused")
	}
	_ = os.Remove(key)
	_ = os.WriteFile(key+".pub", []byte("ssh-ed25519 AAAA x\n"), 0o600)
	if err := f.o.deployKey(context.Background(), "config-repo", key); err == nil || !strings.Contains(err.Error(), ".pub exists") {
		t.Fatalf("a stray .pub: %v", err)
	}
}
