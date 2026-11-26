package listener

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/telegram"
)

// TestCommandTable: the menu, /help and the handled commands are one list,
// and the menu fits what Telegram allows (code6x/pullgate#69).
func TestCommandTable(t *testing.T) {
	f := newL(t)
	help := f.l.helpText()
	valid := regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	if len(commands) > 100 {
		t.Fatalf("Telegram takes at most 100 commands, the table has %d", len(commands))
	}
	if !slices.Equal(menu(), tableMenu()) {
		t.Fatal("the menu is not the table")
	}
	seen := map[string]bool{}
	dos := map[string]bool{}
	for _, c := range commands {
		if !valid.MatchString(c.Name) {
			t.Errorf("%q is not a valid Telegram command name", c.Name)
		}
		if n := len([]rune(c.Menu)); n < 1 || n > 256 {
			t.Errorf("/%s: the menu description has %d characters, want 1 to 256", c.Name, n)
		}
		if strings.ContainsAny(c.Menu, "<>&") {
			t.Errorf("/%s: the menu description is plain text, not HTML", c.Name)
		}
		if len(c.Help) == 0 || !strings.Contains(help, "<code>/"+c.Name) {
			t.Errorf("/help does not list /%s", c.Name)
		}
		for _, line := range c.Help {
			if !strings.Contains(help, line) {
				t.Errorf("/help misses the line %q", line)
			}
		}
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if seen[n] {
				t.Errorf("the name %q is in the table twice", n)
			}
			seen[n] = true
			text := "/" + n
			if !c.Bare && c.Do != doCheck {
				text += " " + inst
			}
			names, why, _, got := f.l.checkNames(&telegram.Message{From: &telegram.User{ID: owner}, Chat: telegram.Chat{ID: chat}, Text: text})
			if got == nil || got.Name != c.Name || (why != c.Do && names == nil) {
				t.Errorf("%q is not handled as /%s: %q %v", text, c.Name, why, got)
			}
		}
		dos[c.Do] = true
	}
	for _, d := range []string{doCheck, doHelp, doUpdate, spool.RequestStatus, spool.RequestPause, spool.RequestResume, spool.RequestAskAgain} {
		if !dos[d] {
			t.Errorf("the handled command %q is not in the table", d)
		}
	}
}

func tableMenu() []telegram.BotCommand {
	var out []telegram.BotCommand
	for _, c := range commands {
		out = append(out, telegram.BotCommand{Command: c.Name, Description: c.Menu})
	}
	return out
}

// TestMenuIsSetForTheOwnersChatOnly: the menu is set with the scope of the
// owner's chat, and not set again when Telegram has the same one.
func TestMenuIsSetForTheOwnersChatOnly(t *testing.T) {
	f := newL(t)
	f.l.setMenu(context.Background())
	got, sets := f.tg.Menu(chat)
	if sets != 1 || !slices.Equal(got, menu()) {
		t.Fatalf("the menu of the chat: %d sets, %+v", sets, got)
	}
	f.l.setMenu(context.Background()) // the next start
	if _, sets := f.tg.Menu(chat); sets != 1 {
		t.Fatalf("the same menu must not be set again: %d sets", sets)
	}
	for _, s := range f.tg.Scopes() {
		if s != "chat:222" {
			t.Fatalf("a menu call with the scope %q: only the owner's chat may be used", s)
		}
	}
	// A menu that differs (an older pullgate) is replaced.
	f.tg.Menus[chat] = []telegram.BotCommand{{Command: "check", Description: "old"}}
	f.l.setMenu(context.Background())
	if got, sets := f.tg.Menu(chat); sets != 2 || !slices.Equal(got, menu()) {
		t.Fatalf("an old menu must be replaced: %d sets, %+v", sets, got)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestMenuFailureDoesNotStopTheListener: when Telegram refuses the menu
// (also "chat not found"), the listener logs it once and goes on.
func TestMenuFailureDoesNotStopTheListener(t *testing.T) {
	for _, desc := range []string{"Bad Request: chat not found", "Bad Request: refused by the fake"} {
		f := newL(t)
		var logs syncBuf
		f.l.Log = slog.New(slog.NewTextHandler(&logs, nil))
		f.l.LongPoll = 1
		f.l.Poll = 50 * time.Millisecond
		f.tg.SetRefuseMenu(desc)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- f.l.Run(ctx) }()
		f.tg.Say(owner, chat, "/help", false)
		deadline := time.Now().Add(10 * time.Second)
		for len(f.tg.Messages()) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("%s: Run: %v", desc, err)
		}
		if len(f.tg.Messages()) != 1 {
			t.Fatalf("%s: the listener must go on and answer /help", desc)
		}
		if n := strings.Count(logs.String(), "the menu is not set"); n != 1 || !strings.Contains(logs.String(), desc) {
			t.Fatalf("%s: want the failure logged once, got %d:\n%s", desc, n, logs.String())
		}
		if strings.Contains(logs.String(), token) {
			t.Fatal("the log holds the token")
		}
	}
}

// TestAskAgainNewAndOldName: /ask_again is the name; /ask-again still
// works. Both write the request kind ask-again.
func TestAskAgainNewAndOldName(t *testing.T) {
	for _, name := range []string{"/ask_again", "/ask-again"} {
		f := newL(t)
		f.say(owner, chat, name+" "+inst, false)
		r, bad, err := f.d.TakeRequests(os.Geteuid())
		if r != (spool.Requests{AskAgain: 1}) || len(bad) != 0 || err != nil {
			t.Fatalf("%s: requests %+v %v %v", name, r, bad, err)
		}
		f.say(owner, chat, name, false) // needs a name
		ms := f.tg.Messages()
		if len(ms) != 2 || !strings.Contains(ms[0].Text, "Asking to ask again on") || !strings.Contains(ms[1].Text, "<code>/ask_again NAME</code>") {
			t.Fatalf("%s: replies %+v", name, ms)
		}
		f.say(999, chat, name+" "+inst, false) // not the owner
		if f.d.HasCheck() || len(f.tg.Messages()) != 2 {
			t.Fatalf("%s: another user writes no request and gets no reply", name)
		}
	}
}

// TestNoOldAskAgainInTexts: no Go source of pullgate (outside tests) has
// the string "/ask-again" any more, so no text sent to Telegram names it.
// The alias is in the table without its "/".
func TestNoOldAskAgainInTexts(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".claude") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte("/ask-again")) {
			t.Errorf("%s holds \"/ask-again\"; use /ask_again", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDoctorCommand: /doctor asks root for the checks of every instance,
// /doctor NAME for one; the request holds nothing from the message
// (code6x/pullgate#50).
func TestDoctorCommand(t *testing.T) {
	f, d2 := newL2(t)
	f.say(owner, chat, "/doctor", false)
	r1, _, _ := f.d.TakeRequests(os.Geteuid())
	r2, _, _ := d2.TakeRequests(os.Geteuid())
	if r1 != (spool.Requests{Doctor: 1}) || r2 != (spool.Requests{Doctor: 1}) {
		t.Fatalf("/doctor: %+v %+v", r1, r2)
	}
	f.now = f.now.Add(2 * RequestGap)
	f.say(owner, chat, "/doctor "+inst2, false)
	r1, _, _ = f.d.TakeRequests(os.Geteuid())
	r2, _, _ = d2.TakeRequests(os.Geteuid())
	if r1.Any() || r2 != (spool.Requests{Doctor: 1}) {
		t.Fatalf("/doctor NAME: %+v %+v", r1, r2)
	}
	ms := f.tg.Messages()
	if len(ms) != 2 || !strings.Contains(ms[0].Text, "Asking for the checks of pullgate doctor on <b>"+inst+", "+inst2+"</b>") {
		t.Fatalf("replies: %+v", ms)
	}
	// A bad name, more words: the usage text, no request.
	for _, bad := range []string{"/doctor nope", "/doctor " + inst + " x"} {
		f.say(owner, chat, bad, false)
	}
	if f.d.HasCheck() || d2.HasCheck() {
		t.Fatal("a bad /doctor writes no request")
	}
	if ms := f.tg.Messages(); len(ms) != 4 || !strings.Contains(ms[3].Text, "<code>/doctor</code> or <code>/doctor NAME</code>") {
		t.Fatalf("want the usage text: %+v", ms)
	}
	// The same within RequestGap is not written again.
	f.say(owner, chat, "/doctor "+inst2, false)
	if d2.HasCheck() {
		t.Fatal("a second /doctor within a minute must not write")
	}
}

func TestDoctorNotForOthers(t *testing.T) {
	f := newL(t)
	for _, m := range []struct {
		from, chat int64
		edited     bool
	}{{999, chat, false}, {owner, 999, false}, {999, 999, false}, {owner, chat, true}} {
		for _, text := range []string{"/doctor", "/doctor " + inst} {
			f.say(m.from, m.chat, text, m.edited)
		}
	}
	if f.d.HasCheck() || len(f.tg.Messages()) != 0 {
		t.Fatal("others get no reply and write no request")
	}
}

// TestDoctorInHelpAndMenu: /doctor is in /help and in the menu.
func TestDoctorInHelpAndMenu(t *testing.T) {
	f := newL(t)
	if !strings.Contains(f.l.helpText(), "<code>/doctor</code> or <code>/doctor NAME</code>") {
		t.Fatal("/help must list /doctor")
	}
	f.l.setMenu(context.Background())
	got, _ := f.tg.Menu(chat)
	if !slices.ContainsFunc(got, func(c telegram.BotCommand) bool { return c.Command == "doctor" }) {
		t.Fatalf("the menu must hold doctor: %+v", got)
	}
}
