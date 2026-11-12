package spool

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/testgit"
)

func TestMain(m *testing.M) {
	syscall.Umask(0o077)
	os.Exit(m.Run())
}

func dirs(t *testing.T) Dirs {
	t.Helper()
	d := Dirs{Base: filepath.Join(testgit.PrivateDir(t), "spool"), Name: "x"}
	if err := d.Ensure(uint32(os.Geteuid()), Owners{ListenerUID: os.Geteuid(), ListenerGID: os.Getegid()}); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestApprovalRoundTrip(t *testing.T) {
	d := dirs(t)
	a := Approval{Code: "abcdefghijklmnopqrstuv", Action: "apply"}
	if err := d.WriteApproval(a); err != nil {
		t.Fatal(err)
	}
	es, err := d.TakeApprovals(os.Geteuid(), 0)
	if err != nil || len(es) != 1 || es[0].Err != nil || es[0].Approval != a {
		t.Fatalf("%+v %v", es, err)
	}
	if es, _ := d.TakeApprovals(os.Geteuid(), 0); len(es) != 0 {
		t.Fatal("an approval is used once")
	}
}

func TestBadApprovals(t *testing.T) {
	cases := map[string]func(t *testing.T, d Dirs){
		"unknown field": func(t *testing.T, d Dirs) {
			put(t, d, `{"code":"abcdefghijklmnopqrstuv","action":"apply","sha":"x"}`)
		},
		"bad action": func(t *testing.T, d Dirs) { put(t, d, `{"code":"abcdefghijklmnopqrstuv","action":"run"}`) },
		"bad code":   func(t *testing.T, d Dirs) { put(t, d, `{"code":"x","action":"apply"}`) },
		"too big":    func(t *testing.T, d Dirs) { put(t, d, string(make([]byte, 2000))) },
		"two objects": func(t *testing.T, d Dirs) {
			put(t, d, `{"code":"abcdefghijklmnopqrstuv","action":"apply"}{}`)
		},
		"symlink": func(t *testing.T, d Dirs) {
			if err := os.Symlink("/etc/hostname", filepath.Join(d.Inbox(), "1.json")); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, d Dirs) {
			if err := os.MkdirAll(filepath.Join(d.Inbox(), "1.json", "x"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, d Dirs) {
			if err := syscall.Mkfifo(filepath.Join(d.Inbox(), "1.json"), 0o600); err != nil {
				t.Skip(err)
			}
		},
	}
	for n, c := range cases {
		t.Run(n, func(t *testing.T) {
			d := dirs(t)
			c(t, d)
			es, err := d.TakeApprovals(os.Geteuid(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(es) != 1 || es[0].Err == nil {
				t.Fatalf("must be refused: %+v", es)
			}
			if left, _ := os.ReadDir(d.Inbox()); len(left) != 0 {
				t.Fatal("a bad file must be removed")
			}
		})
	}
	d := dirs(t)
	if err := d.WriteApproval(Approval{Code: "abcdefghijklmnopqrstuv", Action: "apply"}); err != nil {
		t.Fatal(err)
	}
	if es, _ := d.TakeApprovals(os.Geteuid()+1, 0); len(es) != 1 || es[0].Err == nil {
		t.Fatal("a file of another owner must be refused")
	}
}

func put(t *testing.T, d Dirs, s string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.Inbox(), "1.json"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSendRefusesSymlinkedOutboxAndUnsafeSpool(t *testing.T) {
	d := dirs(t)
	// The listener plants a symlink at a name: root's write must not follow it.
	target := filepath.Join(filepath.Dir(d.Base), "target")
	if err := os.Symlink(target, filepath.Join(d.Outbox(), "00000000000000000001-x.json")); err != nil {
		t.Fatal(err)
	}
	if err := d.Send(Message{Kind: KindNotice, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("root followed a symlink in the outbox")
	}
	// Ensure refuses a spool under a directory that others can write.
	if err := os.Chmod(filepath.Dir(d.Base), 0o777); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(filepath.Dir(d.Base), 0o700) }()
	if err := d.Ensure(uint32(os.Geteuid()), Owners{ListenerUID: os.Geteuid(), ListenerGID: os.Getegid()}); err == nil {
		t.Fatal("a spool that others can write must be refused")
	}
}

func TestOldTempFilesAreRemoved(t *testing.T) {
	d := dirs(t)
	p := filepath.Join(d.Inbox(), ".tmp-old")
	put(t, d, "{}")
	if err := os.Rename(filepath.Join(d.Inbox(), "1.json"), p); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if es, _ := d.TakeApprovals(os.Geteuid(), 0); len(es) != 0 {
		t.Fatal("a temp file is not an approval")
	}
	if _, err := os.Lstat(p); err == nil {
		t.Fatal("an old temp file must be removed")
	}
}

func TestCheckRequestRoundTrip(t *testing.T) {
	d := dirs(t)
	if d.HasCheck() {
		t.Fatal("an empty check directory has no request")
	}
	// Many requests before root reads them are one file.
	for range 3 {
		if err := d.WriteCheck(); err != nil {
			t.Fatal(err)
		}
	}
	if !d.HasCheck() {
		t.Fatal("the request must be seen")
	}
	n, bad, err := d.TakeChecks(os.Geteuid())
	if err != nil || n != 1 || len(bad) != 0 {
		t.Fatalf("n=%d bad=%v err=%v", n, bad, err)
	}
	if n, _, _ := d.TakeChecks(os.Geteuid()); n != 0 {
		t.Fatal("a request is used once")
	}
}

func TestBadCheckRequests(t *testing.T) {
	putCheck := func(t *testing.T, d Dirs, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(d.CheckDir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T, d Dirs) int{
		"symlink": func(t *testing.T, d Dirs) int {
			if err := os.Symlink("/etc/hostname", filepath.Join(d.CheckDir(), CheckFile)); err != nil {
				t.Fatal(err)
			}
			return os.Geteuid()
		},
		"wrong owner": func(t *testing.T, d Dirs) int {
			putCheck(t, d, CheckFile, CheckContent)
			return os.Geteuid() + 1 // the listener is another user
		},
		"too big": func(t *testing.T, d Dirs) int {
			putCheck(t, d, CheckFile, string(make([]byte, MaxCheck+1)))
			return os.Geteuid()
		},
		"other content": func(t *testing.T, d Dirs) int {
			putCheck(t, d, CheckFile, "check config-repo\n")
			return os.Geteuid()
		},
		"other name": func(t *testing.T, d Dirs) int {
			putCheck(t, d, "1.json", CheckContent)
			return os.Geteuid()
		},
		"directory": func(t *testing.T, d Dirs) int {
			if err := os.MkdirAll(filepath.Join(d.CheckDir(), CheckFile, "x"), 0o700); err != nil {
				t.Fatal(err)
			}
			return os.Geteuid()
		},
		"fifo": func(t *testing.T, d Dirs) int {
			if err := syscall.Mkfifo(filepath.Join(d.CheckDir(), CheckFile), 0o600); err != nil {
				t.Skip(err)
			}
			return os.Geteuid()
		},
	}
	for n, c := range cases {
		t.Run(n, func(t *testing.T) {
			d := dirs(t)
			uid := c(t, d)
			got, bad, err := d.TakeChecks(uid)
			if err != nil {
				t.Fatal(err)
			}
			if got != 0 || len(bad) != 1 {
				t.Fatalf("must be refused: n=%d bad=%v", got, bad)
			}
			if left, _ := os.ReadDir(d.CheckDir()); len(left) != 0 {
				t.Fatal("a bad request must be removed")
			}
		})
	}
}
