package gate

import (
	"time"

	"github.com/code6x/pullgate/internal/state"
)

// ReceiptLate is how long a question may wait for the receipt of the
// listener. After it, a run logs a warning and pullgate status shows
// "not delivered" (DESIGN.md section 8).
const ReceiptLate = 3 * time.Minute

// DefaultReceiptWait is how long a run waits for the receipt of a new
// question before it ends. The listener reads the outbox every 2 seconds.
const DefaultReceiptWait = 20 * time.Second

func (e *Env) receiptWait() time.Duration {
	if e.ReceiptWait < 0 {
		return 0
	}
	if e.ReceiptWait == 0 {
		return DefaultReceiptWait
	}
	return e.ReceiptWait
}

// takeReceipts reads and removes the receipts of the listener, and marks
// the questions they belong to. It logs "asked" for a question that the
// listener sent, and a warning for one it could not send. A receipt of
// another message (a notice, a reply) is only removed. It says whether it
// changed the state.
func (e *Env) takeReceipts(st *state.State) bool {
	rs, bad, err := e.Spool.TakeReceipts(e.Owners.ListenerUID)
	if err != nil {
		e.Log.Warn("read the receipts of the listener", "dir", e.Spool.Receipts(), "err", err)
		return false
	}
	changed := false
	for _, b := range bad {
		e.Log.Warn("bad receipt, removed", "err", b)
	}
	for i := range st.Questions {
		q := &st.Questions[i]
		if q.Outbox == "" || q.Delivery != "" {
			continue
		}
		sent, ok := rs[q.Outbox]
		if !ok {
			continue
		}
		changed = true
		if sent {
			q.Delivery = state.DeliverySent
			e.Log.Info("asked", "sha", q.SHA, "question", q.ID)
		} else {
			q.Delivery = state.DeliveryFailed
			e.Log.Warn("question not delivered: the listener could not read or send it, and removed it. "+
				"Fix: see the journal of pullgate-listen.service and run \"pullgate doctor\" as root; then \"pullgate status "+e.Cfg.Name+" --ask-again\"",
				"sha", q.SHA, "question", q.ID)
		}
	}
	return changed
}

// warnLate logs one warning for each open question that has no receipt
// after ReceiptLate. It says whether it changed the state.
func (e *Env) warnLate(st *state.State, now time.Time) bool {
	changed := false
	for i := range st.Questions {
		q := &st.Questions[i]
		if !q.Open || q.Auto || q.Outbox == "" || q.Delivery != "" || q.Warned || now.Sub(q.Asked) < ReceiptLate {
			continue
		}
		q.Warned = true
		changed = true
		e.Log.Warn("question not delivered: no receipt from the listener after "+ReceiptLate.String()+
			". Fix: check that pullgate-listen.service runs, and run \"pullgate doctor\" as root",
			"sha", q.SHA, "question", q.ID, "outbox_file_left", e.Spool.OutboxHas(q.Outbox))
	}
	return changed
}

// waitReceipt waits up to receiptWait for the receipt of the question with
// the code c, and returns its delivery ("" when none came in time).
func (e *Env) waitReceipt(st *state.State, c string) string {
	deadline := time.Now().Add(e.receiptWait())
	for {
		e.takeReceipts(st)
		i := st.FindCode(c)
		if i < 0 {
			return ""
		}
		if d := st.Questions[i].Delivery; d != "" || !time.Now().Before(deadline) {
			return d
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// delivery is the line of pullgate status about the delivery of q.
func (e *Env) delivery(q *state.Question, now time.Time) string {
	d := q.Delivery
	if d == "" {
		if sent, ok := e.Spool.PeekReceipt(e.Owners.ListenerUID, q.Outbox); ok {
			d = state.DeliveryFailed
			if sent {
				d = state.DeliverySent
			}
		}
	}
	switch {
	case q.Outbox == "":
		return "unknown (asked by an older version)"
	case d == state.DeliverySent:
		return "sent (the listener sent it to Telegram)"
	case d == state.DeliveryFailed:
		return "question not delivered: the listener could not read or send it. Fix: run \"pullgate doctor\" as root, then \"pullgate status " + e.Cfg.Name + " --ask-again\""
	case now.Sub(q.Asked) >= ReceiptLate:
		return "question not delivered: no receipt from the listener after " + ReceiptLate.String() + ". Fix: check that pullgate-listen.service runs, and run \"pullgate doctor\" as root"
	}
	return "waiting for the listener"
}
