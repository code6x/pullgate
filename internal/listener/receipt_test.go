package listener

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/code6x/pullgate/internal/spool"
)

// The listener writes a receipt for each outbox file it handled: "sent"
// for a message that Telegram took, "failed" for a file it removed
// without a send. Root reads them (DESIGN.md section 8).
func TestReceipts(t *testing.T) {
	f := newL(t)
	good, err := f.d.Send(os.Getegid(), spool.Message{Kind: spool.KindNotice, Text: "good notice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.d.Outbox(), "0-broken.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.l.Outbox(context.Background())
	rs, bad, err := f.d.TakeReceipts(os.Geteuid())
	if err != nil || len(bad) != 0 {
		t.Fatal(err, bad)
	}
	if len(rs) != 2 || !rs[good] || rs["0-broken.json"] {
		t.Fatalf("receipts = %v", rs)
	}
}

// Telegram refuses the message for good: the receipt says failed.
func TestReceiptOfARefusedMessage(t *testing.T) {
	f := newL(t)
	f.tg.Refuse = 400
	n, err := f.d.Send(os.Getegid(), spool.Message{Kind: spool.KindNotice, Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	f.l.Outbox(context.Background())
	if rs, _, _ := f.d.TakeReceipts(os.Geteuid()); len(rs) != 1 || rs[n] {
		t.Fatalf("receipts = %v", rs)
	}
}

// The updates of pullgate itself have no receipts.
func TestNoReceiptsForUpdates(t *testing.T) {
	f := newL(t)
	self := spool.Dirs{Base: f.d.Base, Name: spool.SelfName}
	if err := self.Ensure(uint32(os.Geteuid()), spool.Owners{ListenerUID: os.Geteuid(), ListenerGID: os.Getegid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := self.Send(os.Getegid(), spool.Message{Kind: spool.KindNotice, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	f.l.Outbox(context.Background())
	if left, _ := os.ReadDir(self.Receipts()); len(left) != 0 {
		t.Fatalf("receipts for updates: %v", left)
	}
}
