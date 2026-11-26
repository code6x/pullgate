package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code6x/pullgate/internal/testgit"
	"github.com/code6x/pullgate/internal/winsecure"
)

func TestWindowsExportPath(t *testing.T) {
	for _, path := range []string{"dir/file.txt", "normal name.ps1"} {
		if err := checkExportPath(path, "100644"); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"../escape", "dir/../escape", `dir\escape`, "file:stream", "NUL.txt", ".git", "git~1", "dir./file", "invalid\xff"} {
		if err := checkExportPath(path, "100644"); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	if err := checkExportPath("link", "120000"); err == nil {
		t.Fatal("accepted a symlink")
	}
}

func TestWindowsFetchAndExport(t *testing.T) {
	testgit.NeedGit(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "protected")
	if err := winsecure.Mkdir(base); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	remote := &Git{WindowsRoot: base, WindowsGit: git, Home: base, Dir: filepath.Join(base, "remote.git")}
	if err := remote.Init(ctx); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := remote.command(ctx, args...)
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", args[0], err, out)
		}
		return strings.TrimSpace(string(out))
	}
	blob := run("expected file contents\n", "hash-object", "-w", "--stdin")
	tree := run("100644 blob "+blob+"\tnote.txt\n", "mktree")
	sha := run("", "commit-tree", tree, "-m", "test commit")
	run("", "update-ref", "refs/heads/main", sha)
	local := &Git{WindowsRoot: base, WindowsGit: git, Home: base, Dir: filepath.Join(base, "local.git")}
	if err := local.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if tip, err := local.Fetch(ctx, remote.Dir, "main"); err != nil || tip != sha {
		t.Fatalf("wrong fetched commit %q: %v", tip, err)
	}
	dest := filepath.Join(base, "export")
	if _, err := local.Export(ctx, sha, dest, Limits{MaxFiles: 2, MaxBytes: 128}, -1); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(dest, "note.txt")); err != nil || string(content) != "expected file contents\n" {
		t.Fatalf("wrong exported file: %v", err)
	}
}

func TestWindowsSSHConfig(t *testing.T) {
	g := &Git{WindowsGit: `C:\Program Files\Git\cmd\git.exe`, Key: `C:\ProgramData\pullgate\key`, KnownHosts: `C:\ProgramData\pullgate\known hosts`}
	if !g.sshAllowed() || !strings.Contains(g.sshCommand(), `'UserKnownHostsFile="C:/ProgramData/pullgate/known hosts"'`) {
		t.Fatal("known hosts path was not preserved")
	}
	g.Key += "%h"
	if g.sshAllowed() {
		t.Fatal("accepted an SSH key path with token expansion")
	}
	if _, err := LocalPath("ssh://example@example-host/config-repo.git"); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"ssh://example:placeholder@example-host/config-repo.git", "ssh://example-host/config-repo.git?q=example"} {
		if _, err := LocalPath(url); err == nil {
			t.Fatal("accepted credentials or a query in the repo URL")
		}
	}
}
