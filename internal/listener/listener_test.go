package listener

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/telegram"
	"github.com/code6x/pullgate/internal/telegram/faketg"
	"github.com/code6x/pullgate/internal/testgit"
)

const (
	owner = 111
	chat  = 222
	inst  = "config-repo"
	token = "123:test-token-made-by-the-test"
	code  = "abcdefghijklmnopqrstuv"
)

func TestMain(m *testing.M) {
	syscall.Umask(0o077)
	os.Exit(m.Run())
}

type lfix struct {
	t   *testing.T
	tg  *faketg.Server
	l   *Listener
	d   spool.Dirs
	now time.Time
}

func newL(t *testing.T) *lfix {
	base := testgit.PrivateDir(t)
	f := &lfix{t: t, tg: faketg.New(t, token), now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	f.d = spool.Dirs{Base: filepath.Join(base, "spool"), Name: inst}
	uid := uint32(os.Geteuid())
	if err := f.d.Ensure(uid, spool.Owners{ListenerUID: os.Geteuid(), ListenerGID: os.Getegid()}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Listener{OwnerID: owner, ChatID: chat, Instances: []string{inst}, SpoolDir: f.d.Base, StateFile: filepath.Join(base, "listener.json")}
	f.l = &Listener{Cfg: cfg, TG: telegram.New(f.tg.URL, token), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return f.now }}
	return f
}

// ask puts a question in the outbox and lets the listener send it.
func (f *lfix) ask(id int64, c string) int64 {
	f.t.Helper()
	if err := f.d.Send(spool.Message{Kind: spool.KindAsk, Question: id, Code: c, Expires: f.now.Add(time.Hour), Text: "pullgate on <b>example-host</b>\nApply this commit?"}); err != nil {
		f.t.Fatal(err)
	}
	f.l.Outbox(context.Background())
	ms := f.tg.Messages()
	m := ms[len(ms)-1]
	if m.Method != "sendMessage" || len(m.Buttons) != 2 || m.Buttons[0].CallbackData != "a:"+c || m.Buttons[1].CallbackData != "s:"+c {
		f.f("the question was not sent with two buttons: %+v", m)
	}
	return m.ID
}

func (f *lfix) f(format string, a ...any) { f.t.Helper(); f.t.Fatalf(format, a...) }

// tap sends one tap through getUpdates.
func (f *lfix) tap(from, chatID, msg int64, data string) {
	f.t.Helper()
	f.tg.Tap(from, chatID, msg, data)
	ups, err := f.l.TG.GetUpdates(context.Background(), f.l.st.Offset, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, u := range ups {
		f.l.Update(context.Background(), u)
	}
}

func (f *lfix) approvals() []spool.Entry {
	f.t.Helper()
	es, err := f.d.TakeApprovals(os.Geteuid(), 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return es
}

func TestValidTapWritesOnlyTheCode(t *testing.T) {
	f := newL(t)
	msg := f.ask(1, code)
	f.tap(owner, chat, msg, "a:"+code)
	es := f.approvals()
	if len(es) != 1 || es[0].Err != nil || es[0].Approval != (spool.Approval{Code: code, Action: "apply"}) {
		t.Fatalf("approvals %+v", es)
	}
	// A second tap does nothing.
	f.tap(owner, chat, msg, "a:"+code)
	if es := f.approvals(); len(es) != 0 {
		t.Fatal("a second tap must do nothing")
	}
	last := f.tg.Messages()[len(f.tg.Messages())-1]
	if last.Method != "editMessageText" || !strings.Contains(last.Text, "Apply tapped") || len(last.Buttons) != 0 {
		t.Fatalf("the question must say so and lose its buttons: %+v", last)
	}
}

func TestSkipTap(t *testing.T) {
	f := newL(t)
	msg := f.ask(1, code)
	f.tap(owner, chat, msg, "s:"+code)
	if es := f.approvals(); len(es) != 1 || es[0].Approval.Action != "skip" {
		t.Fatalf("approvals %+v", es)
	}
}

func TestTapsThatDoNotCount(t *testing.T) {
	cases := map[string]func(f *lfix, msg int64){
		"another user":    func(f *lfix, msg int64) { f.tap(owner+1, chat, msg, "a:"+code) },
		"another chat":    func(f *lfix, msg int64) { f.tap(owner, chat+1, msg, "a:"+code) },
		"another message": func(f *lfix, msg int64) { f.tap(owner, chat, msg+5, "a:"+code) },
		"unknown code":    func(f *lfix, msg int64) { f.tap(owner, chat, msg, "a:zzzzzzzzzzzzzzzzzzzzzz") },
		"bad data":        func(f *lfix, msg int64) { f.tap(owner, chat, msg, "x:"+code) },
		"expired": func(f *lfix, msg int64) {
			f.now = f.now.Add(2 * time.Hour)
			f.tap(owner, chat, msg, "a:"+code)
		},
		"closed question": func(f *lfix, msg int64) {
			if err := f.d.Send(spool.Message{Kind: spool.KindEdit, Question: 1, Text: "closed"}); err != nil {
				f.t.Fatal(err)
			}
			f.l.Outbox(context.Background())
			f.tap(owner, chat, msg, "a:"+code)
		},
		"text edited by a token thief": func(f *lfix, msg int64) {
			f.tg.EditByThief(msg, "harmless text")
			f.tap(owner, chat, msg, "a:"+code)
		},
	}
	for n, c := range cases {
		t.Run(n, func(t *testing.T) {
			f := newL(t)
			msg := f.ask(1, code)
			c(f, msg)
			if es := f.approvals(); len(es) != 0 {
				t.Fatalf("the tap must be ignored, got %+v", es)
			}
			if a := f.tg.Answers; len(a) == 0 || a[len(a)-1] != Closed {
				t.Fatalf("the tap must get the answer %q, got %v", Closed, a)
			}
		})
	}
}

func TestExpiredAskIsNotSent(t *testing.T) {
	f := newL(t)
	if err := f.d.Send(spool.Message{Kind: spool.KindAsk, Question: 1, Code: code, Expires: f.now.Add(-time.Minute), Text: "old"}); err != nil {
		t.Fatal(err)
	}
	f.l.Outbox(context.Background())
	ms := f.tg.Messages()
	if len(ms) != 1 || len(ms[0].Buttons) != 0 || !strings.Contains(ms[0].Text, "expired before it could be sent") {
		t.Fatalf("an expired question must not be sent, only a notice: %+v", ms)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	f := newL(t)
	msg := f.ask(1, code)
	l2 := &Listener{Cfg: f.l.Cfg, TG: f.l.TG, Log: f.l.Log, Now: f.l.Now}
	if err := l2.Load(); err != nil {
		t.Fatal(err)
	}
	f.l = l2
	f.tap(owner, chat, msg, "a:"+code)
	if es := f.approvals(); len(es) != 1 {
		t.Fatal("a restarted listener must know the open question")
	}
}

func TestReplyAndNotice(t *testing.T) {
	f := newL(t)
	msg := f.ask(1, code)
	_ = f.d.Send(spool.Message{Kind: spool.KindReply, Question: 1, Text: "applied"})
	_ = f.d.Send(spool.Message{Kind: spool.KindNotice, Text: "notice"})
	f.l.Outbox(context.Background())
	ms := f.tg.Messages()
	if len(ms) != 3 || ms[1].ReplyTo != msg || ms[2].Text != "notice" {
		t.Fatalf("messages %+v", ms)
	}
	if left, _ := f.d.ReadOutbox(); len(left) != 0 {
		t.Fatal("sent messages must be removed from the outbox")
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	c := telegram.New("http://127.0.0.1:1", token)
	_, err := c.SendMessage(context.Background(), 1, "x", nil, 0)
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "test-token") {
		t.Fatalf("the error must not hold the token: %v", err)
	}
	tg := faketg.New(t, "other-token")
	c = telegram.New(tg.URL, token)
	_, err = c.SendMessage(context.Background(), 1, "x", nil, 0)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("the error must not hold the token: %v", err)
	}
}

const inst2 = "playbooks"

// newL2 is newL with two instances.
func newL2(t *testing.T) (*lfix, spool.Dirs) {
	f := newL(t)
	d2 := spool.Dirs{Base: f.d.Base, Name: inst2}
	if err := d2.Ensure(uint32(os.Geteuid()), spool.Owners{ListenerUID: os.Geteuid(), ListenerGID: os.Getegid()}); err != nil {
		t.Fatal(err)
	}
	f.l.Cfg.Instances = append(f.l.Cfg.Instances, inst2)
	return f, d2
}

// say sends one message through getUpdates.
func (f *lfix) say(from, chatID int64, text string, edited bool) {
	f.t.Helper()
	f.tg.Say(from, chatID, text, edited)
	ups, err := f.l.TG.GetUpdates(context.Background(), f.l.st.Offset, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, u := range ups {
		f.l.Update(context.Background(), u)
	}
}

func TestCheckCommandWritesRequests(t *testing.T) {
	f, d2 := newL2(t)
	f.say(owner, chat, "/check "+inst2, false)
	if f.d.HasCheck() || !d2.HasCheck() {
		t.Fatal("/check NAME must write a request for that instance only")
	}
	ms := f.tg.Messages()
	if len(ms) != 1 || !strings.Contains(ms[0].Text, "Checking <b>"+inst2+"</b>") || ms[0].ReplyTo == 0 {
		t.Fatalf("the reply must say checking: %+v", ms)
	}
	if n, bad, err := d2.TakeChecks(os.Geteuid()); n != 1 || len(bad) != 0 || err != nil {
		t.Fatalf("the request must be valid for root: %d %v %v", n, bad, err)
	}
	f.now = f.now.Add(CheckGap)
	f.say(owner, chat, "/check", false)
	if !f.d.HasCheck() || !d2.HasCheck() {
		t.Fatal("/check must write a request for every instance")
	}
}

func TestCheckCommandsThatDoNotCount(t *testing.T) {
	cases := map[string]struct {
		from, chat int64
		text       string
		edited     bool
		help       bool // the owner gets the usage text
	}{
		"another user":       {owner + 1, chat, "/check", false, false},
		"another chat":       {owner, chat + 1, "/check", false, false},
		"edited message":     {owner, chat, "/check", true, false},
		"unknown name":       {owner, chat, "/check other", false, true},
		"extra text":         {owner, chat, "/check " + inst + " now", false, true},
		"two spaces":         {owner, chat, "/check  " + inst, false, true},
		"to another bot":     {owner, chat, "/check@otherbot", false, true},
		"name with @":        {owner, chat, "/check@otherbot " + inst, false, true},
		"other text":         {owner, chat, "hello", false, false},
		"command in a line":  {owner, chat, "please /check", false, false},
		"other user, known":  {owner + 1, chat, "/check " + inst, false, false},
		"trailing new line":  {owner, chat, "/check\n", false, true},
		"upper case command": {owner, chat, "/CHECK", false, false},
	}
	for n, c := range cases {
		t.Run(n, func(t *testing.T) {
			f, d2 := newL2(t)
			f.say(c.from, c.chat, c.text, c.edited)
			if f.d.HasCheck() || d2.HasCheck() {
				t.Fatal("no request may be written")
			}
			ms := f.tg.Messages()
			switch {
			case c.help && (len(ms) != 1 || ms[0].Chat != chat || !strings.Contains(ms[0].Text, "Use <code>/check</code>")):
				t.Fatalf("the owner must get the usage text: %+v", ms)
			case !c.help && len(ms) != 0:
				t.Fatalf("no answer may be sent: %+v", ms)
			}
			for _, m := range ms {
				if strings.Contains(m.Text, "other") || strings.Contains(m.Text, "now") {
					t.Fatalf("the reply must not repeat the text of the message: %q", m.Text)
				}
			}
		})
	}
}

func TestReplayedUpdateIsIgnored(t *testing.T) {
	f := newL(t)
	f.say(owner, chat, "/check", false)
	if _, _, err := f.d.TakeChecks(os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute) // past CheckGap
	u := telegram.Update{UpdateID: f.l.st.Offset - 1, Message: &telegram.Message{MessageID: 1, From: &telegram.User{ID: owner}, Chat: telegram.Chat{ID: chat}, Text: "/check"}}
	f.l.Update(context.Background(), u)
	if f.d.HasCheck() {
		t.Fatal("a replayed update_id must do nothing")
	}
	// A replayed tap too, even if the code were still open.
	msg := f.ask(1, code)
	f.tap(owner, chat, msg, "a:"+code)
	f.approvals()
	old := f.l.st.Offset - 1
	f.find(inst, 1).Code = code
	f.l.Update(context.Background(), telegram.Update{UpdateID: old, CallbackQuery: &telegram.CallbackQuery{ID: "cq", From: telegram.User{ID: owner}, Message: &telegram.Message{MessageID: msg, Chat: telegram.Chat{ID: chat}}, Data: "a:" + code}})
	if es := f.approvals(); len(es) != 0 {
		t.Fatal("a replayed tap must do nothing")
	}
	// A new, lower ID (Telegram starts again after a quiet week) counts.
	f.l.Update(context.Background(), telegram.Update{UpdateID: 1000000 + 7, Message: &telegram.Message{MessageID: 2, From: &telegram.User{ID: owner}, Chat: telegram.Chat{ID: chat}, Text: "/check"}})
	_, _, _ = f.d.TakeChecks(os.Geteuid())
	f.now = f.now.Add(time.Minute)
	f.l.Update(context.Background(), telegram.Update{UpdateID: 5, Message: &telegram.Message{MessageID: 3, From: &telegram.User{ID: owner}, Chat: telegram.Chat{ID: chat}, Text: "/check"}})
	if !f.d.HasCheck() || f.l.st.Offset != 6 {
		t.Fatalf("a new lower update_id must count, offset %d", f.l.st.Offset)
	}
}

func TestCheckGapInTheListener(t *testing.T) {
	f := newL(t)
	f.say(owner, chat, "/check", false)
	_, _, _ = f.d.TakeChecks(os.Geteuid())
	f.now = f.now.Add(CheckGap / 2)
	f.say(owner, chat, "/check "+inst, false)
	if f.d.HasCheck() {
		t.Fatal("a second /check within CheckGap must write nothing")
	}
	if ms := f.tg.Messages(); !strings.Contains(ms[len(ms)-1].Text, "a few seconds ago") {
		t.Fatalf("the owner must learn why: %+v", ms[len(ms)-1])
	}
	f.now = f.now.Add(CheckGap)
	f.say(owner, chat, "/check", false)
	if !f.d.HasCheck() {
		t.Fatal("after CheckGap /check works again")
	}
}

func (f *lfix) find(i string, id int64) *Question { return f.l.find(i, id) }

func TestCheckRequestWriteFailsSaysSo(t *testing.T) {
	f := newL(t)
	if err := os.RemoveAll(f.d.CheckDir()); err != nil {
		t.Fatal(err)
	}
	f.say(owner, chat, "/check", false)
	ms := f.tg.Messages()
	if len(ms) != 1 || !strings.Contains(ms[0].Text, "was not saved") {
		t.Fatalf("the owner must learn that the request was not saved: %+v", ms)
	}
}

func TestListenerAsksForMessages(t *testing.T) {
	// getUpdates must ask for messages, or /check never arrives.
	f := newL(t)
	f.say(owner, chat, "/check", false)
	if !f.d.HasCheck() {
		t.Fatal("the message must arrive")
	}
}
