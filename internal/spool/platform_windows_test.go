package spool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/code6x/pullgate/internal/winsecure"
)

func TestWindowsApprovalSpool(t *testing.T) {
	base := filepath.Join(t.TempDir(), "protected")
	if err := winsecure.Mkdir(base); err != nil {
		t.Fatal(err)
	}
	d := Dirs{Base: filepath.Join(base, "spool"), Name: "example"}
	if err := d.Ensure(0, Owners{}); err != nil {
		t.Fatal(err)
	}
	a := Approval{Code: "AAAAAAAAAAAAAAAAAAAAAA", Action: "apply"}
	if err := d.WriteApproval(a); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(d.Inbox())
	if err != nil || len(entries) != 1 {
		t.Fatalf("read inbox: %v", err)
	}
	if err := os.Link(filepath.Join(d.Inbox(), entries[0].Name()), filepath.Join(base, "second-link")); err != nil {
		t.Fatal(err)
	}
	if got, err := d.TakeApprovals(0, 0); err != nil || len(got) != 1 || got[0].Err == nil {
		t.Fatalf("expected linked approval to be refused: %v", err)
	}
	if err := d.WriteApproval(a); err != nil {
		t.Fatal(err)
	}
	if got, err := d.TakeApprovals(0, 0); err != nil || len(got) != 1 || got[0].Err != nil || got[0].Approval != a {
		t.Fatalf("protected approval was not delivered: %v", err)
	}
}
