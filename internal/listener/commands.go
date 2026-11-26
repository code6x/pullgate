package listener

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/telegram"
)

// What a command does in the listener.
const (
	doCheck  = "check"
	doHelp   = "help"
	doUpdate = "update"
)

// botCommand is one command of the bot. The command handling, /help and
// the menu that Telegram shows when the owner types "/" all come from the
// table commands, so they cannot drift apart (code6x/pullgate#69).
type botCommand struct {
	// Name is the command without the "/". Telegram allows only
	// [a-z0-9_]{1,32} in the menu.
	Name string
	// Aliases are older names that still work, without the "/". They are
	// never shown.
	Aliases []string
	// Do is doCheck, doHelp, doUpdate or the kind of a request file
	// (spool.Request*).
	Do string
	// Bare is true for a command that takes no words after it.
	Bare bool
	// Help is the lines of /help, in HTML.
	Help []string
	// Menu is the plain-text description in the menu, 1 to 256
	// characters. A tap on a menu entry sends the bare command, so a
	// command that needs NAME says so; the bare command answers with the
	// list of instances.
	Menu string
}

var commands = []botCommand{
	{Name: "check", Do: doCheck,
		Help: []string{"<code>/check</code>: look for a new commit now, on every instance", "<code>/check NAME</code>: the same, on one instance"},
		Menu: "Look for a new commit now: /check, or /check NAME for one instance"},
	{Name: "status", Do: spool.RequestStatus,
		Help: []string{"<code>/status</code> or <code>/status NAME</code>: the mode, the applied commit, the open question, paused or not"},
		Menu: "The mode, the applied commit, the open question: /status or /status NAME"},
	{Name: "pause", Do: spool.RequestPause,
		Help: []string{"<code>/pause NAME</code>: stop the instance"},
		Menu: "Stop an instance: /pause NAME"},
	{Name: "resume", Do: spool.RequestResume,
		Help: []string{"<code>/resume NAME</code>: go on after a <code>/pause</code>; any other pause needs root"},
		Menu: "Go on after /pause: /resume NAME"},
	{Name: "ask_again", Aliases: []string{"ask-again"}, Do: spool.RequestAskAgain,
		Help: []string{"<code>/ask_again NAME</code>: ask again about a question that expired or was lost"},
		Menu: "Ask again about a question that expired or was lost: /ask_again NAME"},
	{Name: "update", Do: doUpdate, Bare: true,
		Help: []string{"<code>/update</code>: look for a new release of pullgate; a question asks before it is installed"},
		Menu: "Look for a new release of pullgate"},
	{Name: "help", Do: doHelp, Bare: true,
		Help: []string{"<code>/help</code>: this list"},
		Menu: "The list of commands"},
}

// lookup returns the command that the first word of text names, or nil.
func lookup(text string) *botCommand {
	w, ok := strings.CutPrefix(firstWord(text), "/")
	if !ok {
		return nil
	}
	for i := range commands {
		if commands[i].Name == w || slices.Contains(commands[i].Aliases, w) {
			return &commands[i]
		}
	}
	return nil
}

// menu is the command menu that the listener sets for the owner's chat.
func menu() []telegram.BotCommand {
	out := make([]telegram.BotCommand, 0, len(commands))
	for _, c := range commands {
		out = append(out, telegram.BotCommand{Command: c.Name, Description: c.Menu})
	}
	return out
}

// MenuTimeout bounds the two menu calls at start.
const MenuTimeout = 30 * time.Second

// setMenu sets the command menu for the owner's chat only, when it differs
// from the one Telegram has. A menu of any other scope (for example one
// set by hand in BotFather) is left alone. A failure is logged and never
// stops the listener; the next start tries again.
func (l *Listener) setMenu(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, MenuTimeout)
	defer cancel()
	want := menu()
	have, err := l.TG.GetMyCommands(ctx, l.Cfg.ChatID)
	if err != nil {
		l.Log.Warn("read the command menu of the chat; the menu is not set. pullgate tries again at the next start. Fix: if it says \"chat not found\", send /help to the bot once from the chat", "err", err)
		return
	}
	if slices.Equal(have, want) {
		l.Log.Info("the command menu is up to date", "commands", len(want))
		return
	}
	if err := l.TG.SetMyCommands(ctx, l.Cfg.ChatID, want); err != nil {
		l.Log.Warn("set the command menu of the chat; the menu is not set. pullgate tries again at the next start. Fix: if it says \"chat not found\", send /help to the bot once from the chat", "err", err)
		return
	}
	l.Log.Info("the command menu is set", "commands", len(want))
}
