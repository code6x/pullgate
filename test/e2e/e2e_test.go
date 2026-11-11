//go:build e2e

// Package e2e is the end-to-end test (DESIGN.md section 14): the real
// binary, a local bare repo with two commits, the listener against a fake
// Telegram server, a tap, and the apply of the second commit.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/telegram/faketg"
	"github.com/code6x/pullgate/internal/testgit"
)

const (
	owner = 4242
	chat  = 4242
	token = "123:e2e-token-made-by-the-test"
)

func TestEndToEnd(t *testing.T) {
	syscall.Umask(0o077)
	testgit.NeedGit(t)
	base := testgit.PrivateDir(t)
	bin := filepath.Join(base, "pullgate")
	build := exec.Command("go", "build", "-o", bin, "github.com/code6x/pullgate/cmd/pullgate")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	etc, state, spool, live := filepath.Join(base, "etc"), filepath.Join(base, "state"), filepath.Join(base, "spool"), filepath.Join(base, "live")
	for _, d := range []string{etc, state, spool, live} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	apply := filepath.Join(etc, "apply.sh")
	write(t, apply, "#!/bin/sh\nset -e\ncp \"$PULLGATE_TREE/policy.toml\" "+live+"/policy.toml\necho \"$PULLGATE_SHA\" > "+live+"/sha\n", 0o700)

	r := testgit.New(t)
	write(t, filepath.Join(etc, "config-repo.toml"), fmt.Sprintf(`host = "example-host"
on_failure = "rollback"
[repo]
url = %q
[commands]
apply = [%q]
check_user = %q
[listener]
user = %q
[watch]
paths = ["policy.toml"]
`, r.Bare, apply, u.Username, u.Username), 0o600)

	tg := faketg.New(t, token)
	write(t, filepath.Join(etc, "token"), token+"\n", 0o600)
	write(t, filepath.Join(etc, "listener.toml"), fmt.Sprintf(`token_file = %q
owner_id = %d
chat_id = %d
instances = ["config-repo"]
spool_dir = %q
state_file = %q
api_url = %q
`, filepath.Join(etc, "token"), owner, chat, spool, filepath.Join(base, "listener.json"), tg.URL), 0o600)

	pg := func(args ...string) string {
		t.Helper()
		all := append([]string{"--config-dir", etc, "--state-dir", state, "--spool-dir", spool}, args...)
		out, err := exec.Command(bin, all...).CombinedOutput()
		if err != nil {
			t.Fatalf("pullgate %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	r.Commit("first policy", map[string]string{"policy.toml": "allow = 1\n"})
	second := r.Commit("second policy", map[string]string{"policy.toml": "allow = 2\n"})

	// The first run asks about the tip: the second commit.
	if out := pg("run", "config-repo"); !strings.Contains(out, "result=asked") {
		t.Fatalf("run: %s", out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listen := exec.CommandContext(ctx, bin, "listen", "--config", filepath.Join(etc, "listener.toml"), "--trust-own-config")
	var logs strings.Builder
	listen.Stdout, listen.Stderr = &logs, &logs
	listen.Cancel = func() error { return listen.Process.Signal(syscall.SIGTERM) }
	if err := listen.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			_ = listen.Wait()
		}
	}
	defer stop()

	var q faketg.Sent
	wait(t, "the question", func() bool {
		for _, m := range tg.Messages() {
			if m.Method == "sendMessage" && len(m.Buttons) == 2 {
				q = m
				return true
			}
		}
		return false
	})
	if !strings.Contains(q.Text, "<code>"+second+"</code>") || !strings.Contains(q.Text, "Watched path changed") {
		t.Fatalf("the question:\n%s", q.Text)
	}
	if strings.Contains(q.Buttons[0].CallbackData, second[:10]) {
		t.Fatal("the button must carry a code, not the SHA")
	}
	// A tap from someone else does nothing; then the owner taps Apply.
	tg.Tap(owner+1, chat, q.ID, q.Buttons[0].CallbackData)
	tg.Tap(owner, chat, q.ID, q.Buttons[0].CallbackData)
	inbox := filepath.Join(spool, "config-repo", "inbox")
	wait(t, "the approval", func() bool {
		es, _ := os.ReadDir(inbox)
		return len(es) == 1
	})

	if out := pg("apply", "config-repo"); !strings.Contains(out, "result=applied") {
		t.Fatalf("apply: %s", out)
	}
	got, _ := os.ReadFile(filepath.Join(live, "policy.toml"))
	gotSHA, _ := os.ReadFile(filepath.Join(live, "sha"))
	if string(got) != "allow = 2\n" || strings.TrimSpace(string(gotSHA)) != second {
		t.Fatalf("the second commit was not applied: %q %q", got, gotSHA)
	}
	if out := pg("status", "config-repo"); !strings.Contains(out, "applied:     "+second) {
		t.Fatalf("status: %s", out)
	}
	wait(t, "the report", func() bool {
		for _, m := range tg.Messages() {
			if strings.Contains(m.Text, "✅ Applied <code>"+second) {
				return true
			}
		}
		return false
	})
	stop()
	if strings.Contains(logs.String(), token) {
		t.Fatal("the listener logged the token")
	}
}

func write(t *testing.T, p, s string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
}

func wait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
