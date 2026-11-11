package safedir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func uid() uint32 { return uint32(os.Geteuid()) }

func TestCheckTreeOK(t *testing.T) {
	d := filepath.Join(tempDir(t), "repo.git")
	if err := os.MkdirAll(filepath.Join(d, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "config"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckTree(d, uid()); err != nil {
		t.Fatalf("want ok, got %v", err)
	}
}

func TestCheckTreeDenies(t *testing.T) {
	cases := map[string]func(t *testing.T, d string){
		"dir writable by others": func(t *testing.T, d string) {
			chmod(t, d, 0o777)
		},
		"dir writable by group": func(t *testing.T, d string) {
			chmod(t, d, 0o770)
		},
		"config writable by others": func(t *testing.T, d string) {
			chmod(t, filepath.Join(d, "config"), 0o666)
		},
		"hooks dir writable by others": func(t *testing.T, d string) {
			chmod(t, filepath.Join(d, "hooks"), 0o777)
		},
		"symlink inside": func(t *testing.T, d string) {
			if err := os.Symlink("/etc/passwd", filepath.Join(d, "hooks", "pre-commit")); err != nil {
				t.Fatal(err)
			}
		},
		"parent writable by others": func(t *testing.T, d string) {
			chmod(t, filepath.Dir(d), 0o777)
		},
		"fifo inside": func(t *testing.T, d string) {
			if err := mkfifo(filepath.Join(d, "fifo")); err != nil {
				t.Skip("no mkfifo:", err)
			}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			base := tempDir(t)
			parent := filepath.Join(base, "state")
			d := filepath.Join(parent, "repo.git")
			if err := os.MkdirAll(filepath.Join(d, "hooks"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "config"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			chmod(t, parent, 0o700)
			breakIt(t, d)
			err := CheckTree(d, uid())
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), "Fix") && !strings.Contains(err.Error(), "check ") {
				t.Errorf("error does not say how to fix it: %v", err)
			}
		})
	}
}

func TestCheckTreeOtherOwner(t *testing.T) {
	d := tempDir(t)
	if err := CheckTree(d, uid()+1); err == nil {
		t.Fatal("a directory of another user must be refused")
	}
}

func TestCheckParentsSymlink(t *testing.T) {
	base := tempDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckParents(link, uid()); err == nil {
		t.Fatal("a symlink must be refused")
	}
}

func TestCheckFile(t *testing.T) {
	d := tempDir(t)
	f := filepath.Join(d, "key")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(f, uid(), true); err != nil {
		t.Fatalf("want ok: %v", err)
	}
	chmod(t, f, 0o644)
	if err := CheckFile(f, uid(), true); err == nil {
		t.Fatal("a private file readable by others must be refused")
	}
	if err := CheckFile(f, uid(), false); err != nil {
		t.Fatalf("a public file readable by others is fine: %v", err)
	}
	chmod(t, f, 0o664)
	if err := CheckFile(f, uid(), false); err == nil {
		t.Fatal("a file writable by group must be refused")
	}
	l := filepath.Join(d, "link")
	if err := os.Symlink(f, l); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(l, uid(), false); err == nil {
		t.Fatal("a symlink must be refused")
	}
}

func chmod(t *testing.T, p string, m os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o700) })
}

// tempDir returns a test directory that only this user can write, and
// whose parents only this user can write too, whatever the umask is.
func tempDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for _, p := range []string{filepath.Dir(d), d} {
		if err := os.Chmod(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return d
}
