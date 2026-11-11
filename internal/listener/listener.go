// Package listener is "pullgate listen" (rule 4, DESIGN.md section 9). It
// is the only process with the bot token. It sends what root puts in the
// outboxes, and turns a valid tap of the owner into an approval file. It
// opens no port.
package listener

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/state"
	"github.com/code6x/pullgate/internal/telegram"
)

// Closed is the answer to a tap that does not count.
const Closed = "This question is closed."

// Question is a question that the listener knows.
type Question struct {
	Instance string    `json:"instance"`
	ID       int64     `json:"id"`
	Code     string    `json:"code,omitempty"` // "" once tapped or closed
	MsgID    int64     `json:"msg_id"`
	Text     string    `json:"text"`      // the HTML that was sent
	TextHash string    `json:"text_hash"` // SHA-256 of the text Telegram gave back
	Expires  time.Time `json:"expires"`
}

// State is the listener's state file.
type State struct {
	Offset    int64      `json:"offset"`
	Questions []Question `json:"questions"`
}

// Listener holds the parts.
type Listener struct {
	Cfg      *config.Listener
	TG       *telegram.Client
	Log      *slog.Logger
	Now      func() time.Time
	Poll     time.Duration // how often the outboxes are read
	LongPoll int           // getUpdates timeout in seconds

	mu sync.Mutex
	st State
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (l *Listener) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Load reads the state file. A missing file is an empty state.
func (l *Listener) Load() error {
	b, err := os.ReadFile(l.Cfg.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the listener state: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l.st); err != nil {
		return fmt.Errorf("the listener state %s is broken: %w. Fix: remove it; open questions then stop working, and the next run asks again", l.Cfg.StateFile, err)
	}
	return nil
}

func (l *Listener) save() error {
	// Drop questions a day after they expire.
	keep := l.st.Questions[:0]
	for _, q := range l.st.Questions {
		if l.now().Before(q.Expires.Add(24 * time.Hour)) {
			keep = append(keep, q)
		}
	}
	l.st.Questions = keep
	b, err := json.Marshal(&l.st)
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(l.Cfg.StateFile, b, 0o600)
}

func (l *Listener) find(inst string, id int64) *Question {
	for i := range l.st.Questions {
		if l.st.Questions[i].Instance == inst && l.st.Questions[i].ID == id {
			return &l.st.Questions[i]
		}
	}
	return nil
}

func (l *Listener) dirs(inst string) spool.Dirs {
	return spool.Dirs{Base: l.Cfg.SpoolDir, Name: inst}
}

// Run sends the outboxes and takes taps until ctx ends.
func (l *Listener) Run(ctx context.Context) error {
	if err := l.TG.DeleteWebhook(ctx); err != nil {
		return fmt.Errorf("%w. Fix: check the bot token and the network", err)
	}
	poll := l.Poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	go func() {
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			l.Outbox(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	long := l.LongPoll
	if long == 0 {
		long = 50
	}
	for ctx.Err() == nil {
		l.mu.Lock()
		off := l.st.Offset
		l.mu.Unlock()
		ups, err := l.TG.GetUpdates(ctx, off, long)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			l.Log.Warn("getUpdates failed", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range ups {
			l.Update(ctx, u)
		}
	}
	return nil
}

// Outbox sends every message in the outboxes, in order.
func (l *Listener) Outbox(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, inst := range l.Cfg.Instances {
		d := l.dirs(inst)
		files, err := d.ReadOutbox()
		if err != nil {
			l.Log.Warn("read the outbox", "instance", inst, "err", err)
			continue
		}
		for _, f := range files {
			if f.Err != nil {
				l.Log.Warn("bad outbox file, removed", "instance", inst, "file", f.Name, "err", f.Err)
				_ = d.Done(f.Name)
				continue
			}
			err := l.send(ctx, inst, f.Message)
			var ae *telegram.APIError
			switch {
			case err == nil:
			case errors.As(err, &ae) && ae.Permanent():
				l.Log.Warn("telegram refused a message, dropped", "instance", inst, "kind", f.Message.Kind, "err", err)
			case f.Age > 24*time.Hour:
				l.Log.Warn("a message could not be sent for a day, dropped", "instance", inst, "kind", f.Message.Kind, "err", err)
			default:
				l.Log.Warn("send failed, will try again", "instance", inst, "kind", f.Message.Kind, "err", err)
				return // keep the order: try this one again first
			}
			if err := d.Done(f.Name); err != nil {
				l.Log.Warn("remove an outbox file", "instance", inst, "err", err)
			}
			if err := l.save(); err != nil {
				l.Log.Error("save the listener state", "err", err)
			}
		}
	}
}

func (l *Listener) send(ctx context.Context, inst string, m spool.Message) error {
	chat := l.Cfg.ChatID
	switch m.Kind {
	case spool.KindAsk:
		if len(m.Code) != 22 || !l.now().Before(m.Expires) {
			return nil // expired or broken: never show buttons for it
		}
		if q := l.find(inst, m.Question); q != nil {
			return nil // sent already
		}
		msg, err := l.TG.SendMessage(ctx, chat, m.Text, []telegram.Button{
			{Text: "Apply", CallbackData: "a:" + m.Code},
			{Text: "Skip", CallbackData: "s:" + m.Code},
		}, 0)
		if err != nil {
			return err
		}
		l.st.Questions = append(l.st.Questions, Question{
			Instance: inst, ID: m.Question, Code: m.Code, MsgID: msg.MessageID,
			Text: m.Text, TextHash: hash(msg.Text), Expires: m.Expires,
		})
		l.Log.Info("question sent", "instance", inst, "question", m.Question)
		return nil
	case spool.KindEdit:
		q := l.find(inst, m.Question)
		if q == nil {
			_, err := l.TG.SendMessage(ctx, chat, m.Text, nil, 0)
			return err
		}
		q.Code = "" // closed: a tap after this never counts
		q.Text = m.Text
		return l.TG.EditMessageText(ctx, chat, q.MsgID, m.Text)
	case spool.KindReply:
		var to int64
		if q := l.find(inst, m.Question); q != nil {
			to = q.MsgID
		}
		_, err := l.TG.SendMessage(ctx, chat, m.Text, nil, to)
		return err
	case spool.KindNotice:
		_, err := l.TG.SendMessage(ctx, chat, m.Text, nil, 0)
		return err
	default:
		return &telegram.APIError{Method: "outbox", Code: 400, Desc: "unknown kind"}
	}
}

// Update handles one update: only a valid tap of the owner counts.
func (l *Listener) Update(ctx context.Context, u telegram.Update) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if u.UpdateID >= l.st.Offset {
		l.st.Offset = u.UpdateID + 1
	}
	defer func() {
		if err := l.save(); err != nil {
			l.Log.Error("save the listener state", "err", err)
		}
	}()
	cq := u.CallbackQuery
	if cq == nil {
		return
	}
	answer := Closed
	defer func() {
		if err := l.TG.AnswerCallbackQuery(ctx, cq.ID, answer); err != nil {
			l.Log.Warn("answer a tap", "err", err)
		}
	}()
	q, action, why := l.check(cq)
	if q == nil {
		l.Log.Info("tap ignored", "reason", why, "from", cq.From.ID)
		return
	}
	if err := l.dirs(q.Instance).WriteApproval(spool.Approval{Code: q.Code, Action: action}); err != nil {
		l.Log.Error("write the approval", "instance", q.Instance, "err", err)
		answer = "Error: the approval was not saved. Tap again."
		return
	}
	q.Code = "" // a second tap does nothing
	note := "Apply tapped. Waiting for the host."
	answer = "Apply tapped."
	if action == "skip" {
		note, answer = "Skipped.", "Skipped."
	}
	l.Log.Info("tap taken", "instance", q.Instance, "question", q.ID, "action", action)
	q.Text = q.Text + "\n\n<b>" + note + "</b>"
	if err := l.TG.EditMessageText(ctx, l.Cfg.ChatID, q.MsgID, q.Text); err != nil {
		l.Log.Warn("edit the question", "err", err)
	}
}

// check returns the question that a tap is for, or nil and why not.
func (l *Listener) check(cq *telegram.CallbackQuery) (*Question, string, string) {
	if cq.From.ID != l.Cfg.OwnerID {
		return nil, "", "not the owner"
	}
	if cq.Message == nil || cq.Message.Chat.ID != l.Cfg.ChatID {
		return nil, "", "not the configured chat"
	}
	action := ""
	switch {
	case strings.HasPrefix(cq.Data, "a:"):
		action = "apply"
	case strings.HasPrefix(cq.Data, "s:"):
		action = "skip"
	default:
		return nil, "", "unknown button"
	}
	code := cq.Data[2:]
	if len(code) != 22 {
		return nil, "", "bad code"
	}
	var q *Question
	for i := range l.st.Questions {
		c := l.st.Questions[i].Code
		if c != "" && subtle.ConstantTimeCompare([]byte(c), []byte(code)) == 1 {
			q = &l.st.Questions[i]
		}
	}
	switch {
	case q == nil:
		return nil, "", "no open question with this code"
	case q.MsgID != cq.Message.MessageID:
		return nil, "", "another message"
	case cq.Message.EditDate != 0 || subtle.ConstantTimeCompare([]byte(hash(cq.Message.Text)), []byte(q.TextHash)) != 1:
		return nil, "", "the message text was changed"
	case !l.now().Before(q.Expires):
		q.Code = ""
		return nil, "", "expired"
	}
	return q, action, ""
}
