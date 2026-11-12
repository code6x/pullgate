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
	"html"
	"io/fs"
	"log/slog"
	"os"
	"slices"
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
	Seen      []int64    `json:"seen,omitempty"` // the last update IDs handled, at most MaxSeen
	Questions []Question `json:"questions"`
}

// MaxSeen is how many handled update IDs the listener keeps, so that a
// replayed update is ignored.
const MaxSeen = 200

// CheckGap is the least time between two check requests that the listener
// writes for one instance. Root keeps its own limit (DESIGN.md section 8);
// this one keeps a burst of /check from hitting the start limit of the
// run unit.
const CheckGap = 10 * time.Second

// Listener holds the parts.
type Listener struct {
	Cfg      *config.Listener
	TG       *telegram.Client
	Log      *slog.Logger
	Now      func() time.Time
	Poll     time.Duration // how often the outboxes are read
	LongPoll int           // getUpdates timeout in seconds

	mu        sync.Mutex
	st        State
	lastCheck map[string]time.Time // in memory: the last check request per instance
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
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
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
instances:
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
				continue instances // keep the order of this instance: try this one again first
			}
			// Save first: a question whose code is not saved could never
			// be answered. A repeated ask is safe (find skips it).
			if err := l.save(); err != nil {
				l.Log.Error("save the listener state", "err", err)
				return
			}
			if err := d.Done(f.Name); err != nil {
				l.Log.Warn("remove an outbox file", "instance", inst, "err", err)
			}
		}
	}
}

func (l *Listener) send(ctx context.Context, inst string, m spool.Message) error {
	chat := l.Cfg.ChatID
	switch m.Kind {
	case spool.KindAsk:
		if len(m.Code) != 22 || !l.now().Before(m.Expires) {
			// Expired or broken: never show buttons for it.
			l.Log.Warn("a question expired before it could be sent, dropped", "instance", inst, "question", m.Question)
			_, err := l.TG.SendMessage(ctx, chat, "pullgate: a question of instance <b>"+html.EscapeString(inst)+"</b> expired before it could be sent. The next commit, or <code>pullgate status "+html.EscapeString(inst)+" --ask-again</code>, asks again.", nil, 0)
			return err
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
			l.Log.Info("edit of a question that was never sent, dropped", "instance", inst, "question", m.Question)
			return nil
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

// Update handles one update: only a valid tap or /check of the owner
// counts. An update with an ID below the offset was handled before and is
// ignored.
func (l *Listener) Update(ctx context.Context, u telegram.Update) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if slices.Contains(l.st.Seen, u.UpdateID) {
		l.Log.Info("update ignored", "reason", "an update that was handled before", "update", u.UpdateID)
		return
	}
	// Telegram may start the IDs again at a random number after a quiet
	// week, so a lower ID is not dropped; only an ID that was handled.
	l.st.Seen = append(l.st.Seen, u.UpdateID)
	if len(l.st.Seen) > MaxSeen {
		l.st.Seen = l.st.Seen[len(l.st.Seen)-MaxSeen:]
	}
	l.st.Offset = u.UpdateID + 1
	defer func() {
		if err := l.save(); err != nil {
			l.Log.Error("save the listener state", "err", err)
		}
	}()
	switch {
	case u.CallbackQuery != nil:
		l.tap(ctx, u.CallbackQuery)
	case u.Message != nil:
		l.command(ctx, u.Message)
	case u.EditedMessage != nil:
		l.Log.Info("message ignored", "reason", "an edited message")
	}
}

// tap handles a button tap.
func (l *Listener) tap(ctx context.Context, cq *telegram.CallbackQuery) {
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

// checkCommand is the one command the listener takes (DESIGN.md section 9).
const checkCommand = "/check"

// command handles a message. Only "/check" or "/check NAME" of the owner,
// in the chat, unedited, counts. The text of a message is never logged.
func (l *Listener) command(ctx context.Context, m *telegram.Message) {
	names, why, help := l.checkNames(m)
	if names == nil {
		l.Log.Info("message ignored", "reason", why, "from", fromID(m))
		if help {
			l.sendReply(ctx, m, "Use <code>/check</code> for all instances, or <code>/check NAME</code>. NAME is one of: "+html.EscapeString(strings.Join(l.Cfg.Instances, ", "))+".")
		}
		return
	}
	var ok, failed, soon []string
	if l.lastCheck == nil {
		l.lastCheck = map[string]time.Time{}
	}
	now := l.now()
	for _, n := range names {
		if d := now.Sub(l.lastCheck[n]); d < CheckGap && d > -CheckGap {
			soon = append(soon, n)
			continue
		}
		if err := l.dirs(n).WriteCheck(); err != nil {
			l.Log.Error("write the check request", "instance", n, "err", err)
			failed = append(failed, n)
			continue
		}
		l.lastCheck[n] = now
		ok = append(ok, n)
	}
	l.Log.Info("check request taken", "instances", ok)
	var text []string
	if len(ok) > 0 {
		text = append(text, "Checking <b>"+html.EscapeString(strings.Join(ok, ", "))+"</b>…")
	}
	if len(soon) > 0 {
		text = append(text, "<b>"+html.EscapeString(strings.Join(soon, ", "))+"</b>: a check was asked a few seconds ago. Wait for its answer.")
	}
	if len(failed) > 0 {
		text = append(text, "Error: the check request for <b>"+html.EscapeString(strings.Join(failed, ", "))+"</b> was not saved. Check the journal of the listener, and that the host ran pullgate once for it.")
	}
	l.sendReply(ctx, m, strings.Join(text, "\n"))
}

// checkNames returns the instances that a message asks to check, or nil,
// why not, and whether the owner should get the usage text.
func (l *Listener) checkNames(m *telegram.Message) ([]string, string, bool) {
	switch {
	case m.From == nil || m.From.ID != l.Cfg.OwnerID:
		return nil, "not the owner", false
	case m.Chat.ID != l.Cfg.ChatID:
		return nil, "not the configured chat", false
	case m.EditDate != 0:
		return nil, "an edited message", false
	case !strings.HasPrefix(m.Text, checkCommand):
		return nil, "not a command", false
	case m.Text == checkCommand:
		return slices.Clone(l.Cfg.Instances), "", false
	}
	name, ok := strings.CutPrefix(m.Text, checkCommand+" ")
	if !ok || !slices.Contains(l.Cfg.Instances, name) {
		return nil, "not /check or /check NAME of a known instance", true
	}
	return []string{name}, "", false
}

func fromID(m *telegram.Message) int64 {
	if m.From == nil {
		return 0
	}
	return m.From.ID
}

func (l *Listener) sendReply(ctx context.Context, m *telegram.Message, text string) {
	if _, err := l.TG.SendMessage(ctx, l.Cfg.ChatID, text, nil, m.MessageID); err != nil {
		l.Log.Warn("send a reply", "err", err)
	}
}
