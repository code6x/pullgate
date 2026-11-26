package gate

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/winsecure"
)

func TestWindowsPowerShell(t *testing.T) {
	root := filepath.Join(t.TempDir(), "protected")
	if err := winsecure.Mkdir(root); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "apply script.ps1")
	f, err := winsecure.CreateExclusive(script)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(f, "param([string]$Value)\nif ($Value -ne 'one argument' -or $Env:PULLGATE_SHA -ne 'example') { exit 4 }\nWrite-Output 'applied'\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{WindowsRoot: root, Cfg: &config.Instance{Name: "example"}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := e.runApplyCommand(ctx, []string{script, "one argument"}, cmdEnv{Tree: root, SHA: "example"}, &output); err != nil {
		t.Fatalf("PowerShell apply failed: %v: %s", err, output.String())
	}
	if !bytes.Contains(output.Bytes(), []byte("applied")) {
		t.Fatal("missing apply output")
	}
	if err := os.Link(script, filepath.Join(root, "another.ps1")); err != nil {
		t.Fatal(err)
	}
	if err := e.runApplyCommand(ctx, []string{script, "one argument"}, cmdEnv{Tree: root, SHA: "example"}, io.Discard); err == nil {
		t.Fatal("ran a script with two hard links")
	}
}
