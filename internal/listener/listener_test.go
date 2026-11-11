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
	if len(f.tg.Messages()) != 0 {
		t.Fatal("an expired question must not be sent")
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
