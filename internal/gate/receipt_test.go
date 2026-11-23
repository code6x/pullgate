package gate

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
)

// logTo sends the log of the Env to a buffer.
func (f *fixture) logTo() *strings.Builder {
	var b strings.Builder
	f.env.Log = slog.New(slog.NewTextHandler(&b, nil))
	return &b
}

// The kernel clears a bit of the outbox (issue #19): the run fails with a
// clear error and asks nothing.
func TestRunFailsWhenTheKernelChangesTheOutboxMode(t *testing.T) {
	f := newFixture(t, "", "")
	f.commit("first", "1", nil)
	old := spool.FixMode
	spool.FixMode = func(fl *os.File, uid, gid int, mode os.FileMode) error {
		if mode == 0o770 {
			mode = 0o750
		}
		return old(fl, uid, gid, mode)
	}
	t.Cleanup(func() { spool.FixMode = old })
	log := f.logTo()
	res, err := f.env.Run(context.Background())
	if !errors.Is(err, spool.ErrNotKept) || res == Asked || res == Sent {
		t.Fatalf("run = %q, %v; want an error that the kernel did not keep the mode", res, err)
	}
	if !strings.Contains(err.Error(), "outbox") || !strings.Contains(err.Error(), "pullgate doctor") {
		t.Fatalf("the error does not say where and how to fix it: %v", err)
	}
	if strings.Contains(log.String(), "msg=asked") {
		t.Fatalf("the run logged asked:\n%s", log)
	}
	if q := f.state().Open(); q != nil {
		t.Fatalf("a question is open: %+v", q)
	}
}

// outboxName is the outbox file name of the open question.
func (f *fixture) outboxName() string {
	f.t.Helper()
	q := f.state().Open()
	if q == nil || q.Outbox == "" {
		f.t.Fatalf("no open question with an outbox file: %+v", q)
	}
	return q.Outbox
}

// The listener removed the outbox file of the question as unreadable: its
// receipt says failed, status shows "not delivered", and the next run
// logs a warning, not "asked".
func TestQuestionRemovedByTheListenerIsNotDelivered(t *testing.T) {
	f := newFixture(t, "", "")
	f.commit("first", "1", nil)
	if r := f.run(); r != Sent {
		t.Fatalf("run = %q", r)
	}
	if s := f.status(); !strings.Contains(s, "delivery:    waiting for the listener") {
		t.Fatalf("status before the receipt:\n%s", s)
	}
	n := f.outboxName()
	if err := f.env.Spool.Done(n); err != nil {
		t.Fatal(err)
	}
	if err := f.env.Spool.WriteReceipt(n, false); err != nil {
		t.Fatal(err)
	}
	if s := f.status(); !strings.Contains(s, "question not delivered") || !strings.Contains(s, "--ask-again") {
		t.Fatalf("status shows no \"question not delivered\":\n%s", s)
	}
	log := f.logTo()
	f.run()
	if !strings.Contains(log.String(), "level=WARN msg=\"question not delivered") || strings.Contains(log.String(), "msg=asked") {
		t.Fatalf("the next run did not warn:\n%s", log)
	}
	if q := f.state().Open(); q == nil || q.Delivery != state.DeliveryFailed {
		t.Fatalf("question: %+v", q)
	}
	if s := f.status(); !strings.Contains(s, "question not delivered") {
		t.Fatalf("status after the run:\n%s", s)
	}
}

// No receipt at all (the listener is down, or removed the file with no
// receipt): after ReceiptLate the next run warns once, and status shows
// "not delivered".
func TestQuestionWithNoReceiptIsNotDelivered(t *testing.T) {
	f := newFixture(t, "", "")
	f.commit("first", "1", nil)
	if r := f.run(); r != Sent {
		t.Fatalf("run = %q", r)
	}
	if err := f.env.Spool.Done(f.outboxName()); err != nil {
		t.Fatal(err)
	}
	log := f.logTo()
	f.run()
	if strings.Contains(log.String(), "not delivered") {
		t.Fatalf("a warning before ReceiptLate:\n%s", log)
	}
	f.now = f.now.Add(ReceiptLate + time.Second)
	f.run()
	if !strings.Contains(log.String(), "level=WARN msg=\"question not delivered: no receipt") {
		t.Fatalf("no warning after ReceiptLate:\n%s", log)
	}
	if s := f.status(); !strings.Contains(s, "question not delivered: no receipt") {
		t.Fatalf("status:\n%s", s)
	}
	log.Reset()
	f.run()
	if strings.Contains(log.String(), "not delivered") {
		t.Fatalf("the warning came twice:\n%s", log)
	}
}

// A receipt that comes while the run waits: the run logs "asked" and its
// result is "asked".
func TestRunLogsAskedAfterTheReceipt(t *testing.T) {
	f := newFixture(t, "", "")
	f.commit("first", "1", nil)
	f.env.ReceiptWait = 5 * time.Second
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // a listener that sends every outbox file
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			fs, _ := f.env.Spool.ReadOutbox()
			for _, of := range fs {
				_ = f.env.Spool.WriteReceipt(of.Name, true)
				_ = f.env.Spool.Done(of.Name)
			}
		}
	}()
	log := f.logTo()
	r := f.run()
	close(stop)
	<-done
	if r != Asked || !strings.Contains(log.String(), "msg=asked") {
		t.Fatalf("run = %q, log:\n%s", r, log)
	}
	if s := f.status(); !strings.Contains(s, "delivery:    sent") {
		t.Fatalf("status:\n%s", s)
	}
}

// A failed receipt while the run waits: the run fails, not "asked".
func TestRunFailsOnAFailedReceipt(t *testing.T) {
	f := newFixture(t, "", "")
	f.commit("first", "1", nil)
	f.env.ReceiptWait = 5 * time.Second
	go func() {
		for range 250 {
			time.Sleep(20 * time.Millisecond)
			fs, _ := f.env.Spool.ReadOutbox()
			for _, of := range fs {
				_ = f.env.Spool.WriteReceipt(of.Name, false)
				_ = f.env.Spool.Done(of.Name)
				return
			}
		}
	}()
	res, err := f.env.Run(context.Background())
	if res != NotDelivered || err == nil || !strings.Contains(err.Error(), "pullgate doctor") {
		t.Fatalf("run = %q, %v", res, err)
	}
}
