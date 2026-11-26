//go:build e2e

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/gate"
	"github.com/code6x/pullgate/internal/state"
	"github.com/code6x/pullgate/internal/telegram/faketg"
	"github.com/code6x/pullgate/internal/testgit"
	"github.com/code6x/pullgate/internal/winsecure"
)

func TestWindowsEndToEnd(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "protected")
	if err := initialize(root); err != nil {
		t.Fatal(err)
	}
	const name = "config-repo"
	const owner = 4242
	remote := testgit.NewAt(t, root)
	remote.Commit("first", map[string]string{"policy.txt": "first\n"})
	second := remote.Commit("second", map[string]string{"policy.txt": "second\n"})
	result := filepath.Join(root, "applied.txt")
	apply := filepath.Join(root, "apply.ps1")
	script := `param([string]$Result)
$ErrorActionPreference = 'Stop'
$policy = [System.IO.File]::ReadAllText((Join-Path $Env:PULLGATE_TREE 'policy.txt'))
[System.IO.File]::WriteAllText($Result, $Env:PULLGATE_SHA + ' ' + $policy.Trim())
`
	write := func(path string, data []byte) {
		t.Helper()
		if err := state.WriteFileAtomic(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(apply, []byte(script))
	cfg, err := encodeWindowsConfig(map[string]any{
		"host":     "example-host",
		"repo":     map[string]any{"url": remote.Bare},
		"commands": map[string]any{"apply": []string{apply, result}},
	})
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, name+".toml"), cfg)
	var tokenBytes [24]byte
	rand.Read(tokenBytes[:])
	token := "123:" + hex.EncodeToString(tokenBytes[:])
	tg := faketg.New(t, token)
	protected, err := winsecure.ProtectToken([]byte(token))
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "bot-token.dpapi"), protected)
	cfg, err = encodeWindowsConfig(map[string]any{
		"owner_id": owner, "chat_id": owner,
		"api_url": tg.URL, "instances": []string{name},
	})
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "listener.toml"), cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	finished := make(chan error, 1)
	go func() { finished <- runAgent(ctx, root, git, log, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("agent stopped: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("agent did not stop")
		}
		if strings.Contains(logs.String(), token) {
			t.Fatal("agent logged the token")
		}
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	wait := func(forWhat string, ready func() bool) {
		t.Helper()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for !ready() {
			select {
			case <-ctx.Done():
				t.Fatalf("timed out waiting for %s", forWhat)
			case <-ticker.C:
			}
		}
	}
	var ask faketg.Sent
	wait("a question for the exact commit", func() bool {
		for _, sent := range tg.Messages() {
			if len(sent.Buttons) == 2 && strings.Contains(sent.Text, "Full: <code>"+second+"</code>") {
				ask = sent
				return true
			}
		}
		return false
	})
	tg.Tap(owner+1, owner, ask.ID, ask.Buttons[0].CallbackData)
	wait("the refused tap", func() bool { return len(tg.CallbackAnswers()) != 0 })
	if _, err := os.Stat(result); !os.IsNotExist(err) {
		t.Fatal("apply ran before the owner tapped")
	}
	tg.Tap(owner, owner, ask.ID, ask.Buttons[0].CallbackData)
	wait("the PowerShell apply", func() bool {
		b, err := os.ReadFile(result)
		return err == nil && string(b) == second+" second"
	})
	wait("the applied status", func() bool {
		e, err := loadGate(root, name, git, log, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		var status bytes.Buffer
		if err := e.Status(&status, gate.StatusFlags{}); err != nil {
			t.Fatal(err)
		}
		return strings.Contains(status.String(), "applied:     "+second)
	})
}
