package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/code6x/pullgate/internal/testgit"
)

func newGit(t *testing.T) (*Git, string) {
	t.Helper()
	testgit.NeedGit(t)
	d := testgit.PrivateDir(t)
	g := &Git{Dir: filepath.Join(d, "repo.git"), Home: d, UID: uint32(os.Geteuid())}
	if err := g.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return g, d
}

func TestFetchAncestorLogDiff(t *testing.T) {
	ctx := context.Background()
	r := testgit.New(t)
	a := r.Commit("first", map[string]string{"a.txt": "a", "dir/b.txt": "b"})
	b := r.Commit("second <b>", map[string]string{"a.txt": "a2", "c.txt": "c"})
	g, _ := newGit(t)
	tip, err := g.Fetch(ctx, r.Bare, "main")
	if err != nil {
		t.Fatal(err)
	}
	if tip != b {
		t.Fatalf("tip %s, want %s", tip, b)
	}
	if ok, err := g.IsAncestor(ctx, a, b); err != nil || !ok {
		t.Fatalf("a must be an ancestor of b: %v %v", ok, err)
	}
	if ok, err := g.IsAncestor(ctx, b, a); err != nil || ok {
		t.Fatalf("b is not an ancestor of a: %v %v", ok, err)
	}
	subs, n, err := g.Subjects(ctx, a, b, 50)
	if err != nil || n != 1 || len(subs) != 1 || subs[0] != "second <b>" {
		t.Fatalf("subjects %q %d %v", subs, n, err)
	}
	files, err := g.ChangedFiles(ctx, a, b)
	if err != nil || !slices.Equal(files, []string{"a.txt", "c.txt"}) {
		t.Fatalf("changed %q %v", files, err)
	}
	all, err := g.ChangedFiles(ctx, "", a)
	if err != nil || !slices.Equal(all, []string{"a.txt", "dir/b.txt"}) {
		t.Fatalf("all files %q %v", all, err)
	}
}

func TestFetchRefusesBadInput(t *testing.T) {
	g, _ := newGit(t)
	ctx := context.Background()
	if _, err := g.Fetch(ctx, "--upload-pack=touch /tmp/x", "main"); err == nil {
		t.Fatal("a url that starts with - must be refused")
	}
	for _, b := range []string{"-x", "a..b", "a b", "", "x.lock", "a//b"} {
		if _, err := g.Fetch(ctx, "/nowhere", b); err == nil {
			t.Fatalf("branch %q must be refused", b)
		}
	}
}

func TestRunRefusesUnsafeGitDir(t *testing.T) {
	g, _ := newGit(t)
	if err := os.Chmod(filepath.Join(g.Dir, "config"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(context.Background(), "rev-parse", "--git-dir"); err == nil || !strings.Contains(err.Error(), "git refused") {
		t.Fatalf("root must refuse git in a directory others can write, got %v", err)
	}
}

func TestInitRefusesUnsafeParent(t *testing.T) {
	testgit.NeedGit(t)
	d := t.TempDir()
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	g := &Git{Dir: filepath.Join(d, "repo.git"), Home: d, UID: uint32(os.Geteuid())}
	if err := g.Init(context.Background()); err == nil {
		t.Fatal("init under a directory others can write must be refused")
	}
}

func TestHooksDoNotRun(t *testing.T) {
	ctx := context.Background()
	r := testgit.New(t)
	r.Commit("one", map[string]string{"a": "1"})
	g, d := newGit(t)
	marker := filepath.Join(d, "hook-ran")
	hooks := filepath.Join(g.Dir, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch " + marker + "\n"
	for _, h := range []string{"reference-transaction", "post-checkout", "fsmonitor-watchman"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.Fetch(ctx, r.Bare, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran")
	}
}

func TestExport(t *testing.T) {
	ctx := context.Background()
	r := testgit.New(t)
	r.Commit("one", map[string]string{
		"conf/app.toml":  "x = 1\n",
		"run.sh":         "#!/bin/sh\necho hi\n",
		".gitattributes": "*.toml filter=evil export-ignore\n*.sh export-subst\n",
	})
	if err := os.Chmod(filepath.Join(r.Work, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(r.Work, "link")); err != nil {
		t.Fatal(err)
	}
	sha := r.Commit("two $Format:%H$", nil)
	g, d := newGit(t)
	if _, err := g.Fetch(ctx, r.Bare, "main"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(d, "tree")
	tr, err := g.Export(ctx, sha, dest, Limits{MaxFiles: 100, MaxBytes: 1 << 20}, -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveTree(dest) })
	if len(tr.Submodules) != 0 {
		t.Fatalf("submodules %v", tr.Submodules)
	}
	if len(tr.Symlinks) != 1 || tr.Symlinks[0] != (Symlink{Path: "link", Target: "/etc/passwd"}) {
		t.Fatalf("symlinks out of the tree: %v", tr.Symlinks)
	}
	got, err := os.ReadFile(filepath.Join(dest, "conf/app.toml"))
	if err != nil || string(got) != "x = 1\n" {
		t.Fatalf("export-ignore or a filter applied: %q %v", got, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "run.sh")); string(got) != "#!/bin/sh\necho hi\n" {
		t.Fatalf("export-subst applied: %q", got)
	}
	fi, err := os.Lstat(filepath.Join(dest, "run.sh"))
	if err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("run.sh mode %v %v", fi.Mode(), err)
	}
	fi, err = os.Lstat(filepath.Join(dest, "link"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink must stay a symlink")
	}
	fi, err = os.Lstat(filepath.Join(dest, "conf"))
	if err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("conf must be read-only: %v", fi.Mode())
	}
	if err := os.WriteFile(filepath.Join(dest, "conf", "new"), []byte("x"), 0o600); err == nil && os.Geteuid() != 0 {
		t.Fatal("the tree must be read-only")
	}
	if err := RemoveTree(dest); err != nil {
		t.Fatal(err)
	}
}

func TestExportLimitsAndSubmodules(t *testing.T) {
	ctx := context.Background()
	r := testgit.New(t)
	r.Commit("one", map[string]string{"a": "12345", "b": "67890"})
	other := r.Git("rev-parse", "HEAD")
	r.Git("update-index", "--add", "--cacheinfo", "160000,"+other+",sub")
	r.Git("commit", "--quiet", "-m", "with a submodule")
	r.Push("HEAD")
	sha := r.Git("rev-parse", "HEAD")
	g, d := newGit(t)
	if _, err := g.Fetch(ctx, r.Bare, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Export(ctx, sha, filepath.Join(d, "t1"), Limits{MaxFiles: 2}, -1); err == nil {
		t.Fatal("max_files must stop the export")
	}
	if _, err := g.Export(ctx, sha, filepath.Join(d, "t2"), Limits{MaxBytes: 9}, -1); err == nil {
		t.Fatal("max_tree_bytes must stop the export")
	}
	left, _ := filepath.Glob(filepath.Join(d, "t2*"))
	tmps, _ := filepath.Glob(filepath.Join(d, ".tmp-*"))
	if len(left)+len(tmps) != 0 {
		t.Fatalf("a failed export must leave no tree: %v %v", left, tmps)
	}
	tr, err := g.Export(ctx, sha, filepath.Join(d, "t3"), Limits{MaxFiles: 10, MaxBytes: 100}, -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveTree(filepath.Join(d, "t3")) })
	if !slices.Equal(tr.Submodules, []string{"sub"}) {
		t.Fatalf("submodules %v", tr.Submodules)
	}
	if _, err := os.Lstat(filepath.Join(d, "t3", "sub")); err == nil {
		t.Fatal("a submodule must not be written")
	}
}

func TestMain(m *testing.M) {
	syscall.Umask(0o077)
	os.Exit(m.Run())
}

func TestLocalPathAndLocalRepoCheck(t *testing.T) {
	for url, want := range map[string]string{
		"/srv/x.git": "/srv/x.git", "file:///srv/x.git": "/srv/x.git",
		"ssh://git@git.example.com/x.git": "", "git@git.example.com:x.git": "",
	} {
		got, err := LocalPath(url)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", url, got, err)
		}
	}
	for _, url := range []string{"https://git.example.com/x.git", "git://x/y", "rel/path", "file://rel", "-oProxy", ""} {
		if _, err := LocalPath(url); err == nil {
			t.Errorf("%q must be refused", url)
		}
	}
	r := testgit.New(t)
	r.Commit("one", map[string]string{"a": "1"})
	if err := os.Chmod(r.Bare, 0o777); err != nil {
		t.Fatal(err)
	}
	g, _ := newGit(t)
	if _, err := g.Fetch(context.Background(), r.Bare, "main"); err == nil {
		t.Fatal("a local repo that others can write must be refused")
	}
	g.Key = "/k ey"
	g.KnownHosts = "/kh"
	if _, err := g.Fetch(context.Background(), "ssh://git@git.example.com/x.git", "main"); err == nil {
		t.Fatal("a key path with a space must be refused")
	}
}

func TestCheckPath(t *testing.T) {
	for _, p := range []string{"a", "a/b", ".github/x", "a.git", "gitx"} {
		if err := CheckPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "/a", "a//b", "../a", "a/../b", ".git/config", "x/.GIT/hooks", "a/./b", "a\x00b"} {
		if err := CheckPath(p); err == nil {
			t.Errorf("%q must be refused", p)
		}
	}
}

func TestParseLsTreeRejectsBadLines(t *testing.T) {
	for _, in := range []string{"100644 blob abc 5\tx\x00", "100644 blob\tx\x00", "garbage\x00"} {
		if _, err := parseLsTree([]byte(in)); err == nil {
			t.Errorf("%q must be refused", in)
		}
	}
}
