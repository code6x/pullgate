package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestInstalledGitForWindows(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("requires the all-users Git for Windows installation on CI")
	}
	path, err := gitExecutable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}
