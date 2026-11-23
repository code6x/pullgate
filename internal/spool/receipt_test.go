package spool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kernelDrops makes FixMode play a kernel that does not keep the mode it
// was asked for: it sets mode &^ drop when mode is want (issue #19).
func kernelDrops(t *testing.T, want, drop os.FileMode) {
	t.Helper()
	old := FixMode
	FixMode = func(f *os.File, uid, gid int, mode os.FileMode) error {
		if mode == want {
			mode &^= drop
		}
		return old(f, uid, gid, mode)
	}
	t.Cleanup(func() { FixMode = old })
}

func TestEnsureReadsBackTheModes(t *testing.T) {
	d := dirs(t)
	kernelDrops(t, 0o770, 0o070)
	err := d.Ensure(uint32(os.Geteuid()), Owners{ListenerUID: os.Geteuid(), ListenerGID: os.Getegid()})
	if !errors.Is(err, ErrNotKept) || !strings.Contains(err.Error(), "outbox") || !strings.Contains(err.Error(), "pullgate doctor") {
		t.Fatalf("Ensure with a mode that the kernel changed: %v", err)
	}
}

func TestSendReadsBackTheMode(t *testing.T) {
	d := dirs(t)
	kernelDrops(t, 0o640, 0o040)
	if _, err := d.Send(os.Getegid(), Message{Kind: KindNotice, Text: "x"}); !errors.Is(err, ErrNotKept) {
		t.Fatalf("Send with a mode that the kernel changed: %v", err)
	}
	names, _ := os.ReadDir(d.Outbox())
	if len(names) != 0 {
		t.Fatalf("a file was left in the outbox: %v", names)
	}
}

func TestReceipts(t *testing.T) {
	d := dirs(t)
	a, err := d.Send(os.Getegid(), Message{Kind: KindNotice, Text: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.WriteReceipt(a, true); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteReceipt("b.json", false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", ".x", "../x", "a/b"} {
		if err := d.WriteReceipt(bad, true); err == nil {
			t.Fatalf("WriteReceipt(%q) passed", bad)
		}
	}
	if err := os.WriteFile(filepath.Join(d.Receipts(), "c.json"), []byte("sent\nmore"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(d.Receipts(), "d.json")); err != nil {
		t.Fatal(err)
	}
	if sent, ok := d.PeekReceipt(os.Geteuid(), a); !sent || !ok {
		t.Fatalf("PeekReceipt = %v, %v", sent, ok)
	}
	if _, ok := d.PeekReceipt(os.Geteuid()+1, a); ok {
		t.Fatal("PeekReceipt took a receipt of another user")
	}
	rs, bad, err := d.TakeReceipts(os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || !rs[a] || rs["b.json"] || len(bad) != 2 {
		t.Fatalf("TakeReceipts = %v, bad %v", rs, bad)
	}
	left, _ := os.ReadDir(d.Receipts())
	if len(left) != 0 {
		t.Fatalf("receipts left: %v", left)
	}
	// A receipt of another user is bad and removed.
	if err := d.WriteReceipt(a, true); err != nil {
		t.Fatal(err)
	}
	if rs, bad, _ := d.TakeReceipts(os.Geteuid() + 1); len(rs) != 0 || len(bad) != 1 {
		t.Fatalf("a receipt of another user was taken: %v %v", rs, bad)
	}
	// No receipts directory (an older version made the spool): no receipt.
	if err := os.Remove(d.Receipts()); err != nil {
		t.Fatal(err)
	}
	if rs, bad, err := d.TakeReceipts(os.Geteuid()); err != nil || len(rs)+len(bad) != 0 {
		t.Fatalf("no directory: %v %v %v", rs, bad, err)
	}
}

func TestDoctorTestFile(t *testing.T) {
	d := dirs(t)
	p, err := d.WriteDoctorTest(os.Getegid())
	if err != nil {
		t.Fatal(err)
	}
	fs, err := d.ReadOutbox()
	if err != nil || len(fs) != 0 {
		t.Fatalf("the listener would see the doctor file: %v %v", fs, err)
	}
	if err := d.RemoveDoctorTest(filepath.Join(d.Outbox(), "00000000000000000001-x.json")); err == nil {
		t.Fatal("RemoveDoctorTest removed another file")
	}
	if err := d.RemoveDoctorTest(p); err != nil {
		t.Fatal(err)
	}
}

// Dot files cannot fill the batch of TakeReceipts for good.
func TestDotFilesDoNotBlockReceipts(t *testing.T) {
	d := dirs(t)
	for i := range MaxReceipts {
		if err := os.WriteFile(filepath.Join(d.Receipts(), fmt.Sprintf(".x%d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.WriteReceipt("a.json", true); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if rs, _, err := d.TakeReceipts(os.Geteuid()); err != nil {
			t.Fatal(err)
		} else if rs["a.json"] {
			return
		}
	}
	t.Fatal("the receipt was never read")
}
