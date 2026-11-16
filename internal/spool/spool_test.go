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
	if err := d.Send(os.Getegid(), Message{Kind: KindNotice, Text: "x"}); err != nil {
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

// otherGroup is a group of the test user other than its main group, or
// its main group when it has no other.
func otherGroup(t *testing.T) int {
	t.Helper()
	gs, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		if g != os.Getegid() {
			return g
		}
	}
	return os.Getegid()
}

func ownerMode(t *testing.T, p string) (uid, gid int, mode os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid), fi.Mode()
}

// TestOwnersAndModes checks the spool of DESIGN.md section 4: no setgid
// bit anywhere (pullgate-run@.service has RestrictSUIDSGID=yes, issue
// #19), and every outbox file has the listener's group, so the listener
// can read it.
func TestOwnersAndModes(t *testing.T) {
	uid, lgid := os.Geteuid(), otherGroup(t)
	if lgid == os.Getegid() {
		t.Log("the test user has only one group, so this test cannot show that Send sets the group")
	}
	d := Dirs{Base: filepath.Join(testgit.PrivateDir(t), "spool"), Name: "x"}
	// A spool made by v0.1.0 has a setgid outbox: Ensure takes the bit off.
	if err := os.MkdirAll(d.Outbox(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d.Outbox(), 0o770|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := d.Ensure(uint32(uid), Owners{ListenerUID: uid, ListenerGID: lgid}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		gid  int
		mode os.FileMode
	}{
		{d.Base, -1, os.ModeDir | 0o755},
		{filepath.Join(d.Base, d.Name), -1, os.ModeDir | 0o755},
		{d.Outbox(), lgid, os.ModeDir | 0o770},
		{d.Inbox(), lgid, os.ModeDir | 0o700},
		{d.CheckDir(), lgid, os.ModeDir | 0o700},
	} {
		u, g, m := ownerMode(t, c.path)
		if u != uid || (c.gid >= 0 && g != c.gid) || m != c.mode {
			t.Errorf("%s: owner %d group %d mode %v, want owner %d group %d mode %v", c.path, u, g, m, uid, c.gid, c.mode)
		}
	}
	if err := d.Send(lgid, Message{Kind: KindNotice, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	fs, err := d.ReadOutbox()
	if err != nil || len(fs) != 1 || fs[0].Err != nil || fs[0].Message.Text != "x" {
		t.Fatalf("the outbox must hold the message: %+v %v", fs, err)
	}
	u, g, m := ownerMode(t, filepath.Join(d.Outbox(), fs[0].Name))
	if u != uid || g != lgid || m != 0o640 {
		t.Errorf("outbox file: owner %d group %d mode %v, want owner %d group %d mode -rw-r-----", u, g, m, uid, lgid)
	}
}

func TestSendDeny(t *testing.T) {
	d := dirs(t)
	if err := d.Send(-1, Message{Kind: KindNotice, Text: "x"}); err == nil {
		t.Error("a message with no group must be refused")
	}
	if os.Geteuid() != 0 {
		// A group the user is not in: the chown fails, and no file stays.
		gs, _ := os.Getgroups()
		in := map[int]bool{os.Getegid(): true}
		for _, g := range gs {
			in[g] = true
		}
		other := 1
		for in[other] {
			other++
		}
		if err := d.Send(other, Message{Kind: KindNotice, Text: "x"}); err == nil {
			t.Error("a group the chown cannot give must be an error")
		}
	}
	if left, _ := os.ReadDir(d.Outbox()); len(left) != 0 {
		t.Fatalf("a refused message must leave no file: %v", left)
	}
}
