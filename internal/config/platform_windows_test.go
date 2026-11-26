package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code6x/pullgate/internal/winsecure"
)

func TestWindowsPolicy(t *testing.T) {
	base := strings.Join([]string{
		"[repo]",
		"url = 'C:\\ProgramData\\pullgate\\example.git'",
		"[commands]",
		"apply = ['C:\\ProgramData\\pullgate\\apply.ps1', 'example argument']",
	}, "\n")
	if _, err := Parse("example.toml", "example", []byte(base)); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"mode = 'auto'\n" + base,
		base + "\ncheck = ['C:\\ProgramData\\pullgate\\check.ps1']",
		strings.ReplaceAll(base, "apply.ps1", "apply.exe"),
	} {
		if _, err := Parse("example.toml", "example", []byte(text)); err == nil {
			t.Fatal("invalid Windows config was accepted")
		}
	}
}

func TestWindowsToken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "protected")
	if err := winsecure.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "bot-token.dpapi")
	cfg := Listener{TokenFile: tokenFile}
	f, err := winsecure.CreateExclusive(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	var suffix [24]byte
	rand.Read(suffix[:])
	token := []byte("12345:" + hex.EncodeToString(suffix[:]))
	blob, err := winsecure.ProtectToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(blob); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := cfg.ReadToken(0)
	if err != nil || got != string(token) {
		t.Fatal("could not read protected token")
	}
	if err := os.WriteFile(tokenFile, token, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ReadToken(0); err == nil {
		t.Fatal("read a plaintext bot token")
	}
}
