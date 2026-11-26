// Package bootstrap is "pullgate bootstrap": it reads the bot token
// without echo, pairs the owner's private chat with a one-time code, and
// sets up the deploy key, the host key and a minimal config of each
// instance (DESIGN.md section 17). The owner and chat IDs come only from
// the Telegram message that carries the code: no flag, file or variable
// sets them.
package bootstrap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/gitx"
	"github.com/code6x/pullgate/internal/recipe"
	"github.com/code6x/pullgate/internal/safedir"
	"github.com/code6x/pullgate/internal/state"
	"github.com/code6x/pullgate/internal/telegram"
)

// Instance is one --instance with its options.
type Instance struct {
	Name, URL, Branch string
	KnownHosts        string // a file with the host keys, for a git host other than GitHub
	Apply             string // the apply program, for a new config
	From              string // --recipe-instance: the instance of the recipe's manifest
	branchSet         bool   // --branch was given
}

// Options are the inputs of a bootstrap. Parse makes them from the
// command line; tests set the program paths and the times.
type Options struct {
	EtcDir        string
	APIURL        string
	TokenStdin    bool
	Instances     []Instance
	AskPassword   []string // reads the token from the console, with no echo
	SSHKeygen     string
	ListenerGroup string
	PairTTL       time.Duration
	RemoteWait    time.Duration
	RemoteEvery   time.Duration
	PollSeconds   int
	Stdin         io.Reader
	Out           io.Writer // the console: the code, the keys, the steps
	Err           io.Writer // the stderr of systemd-ask-password
	Now           func() time.Time
	// Setup makes the users, the directories and the units first, and
	// runs doctor and enables the units at the end. cmd/pullgate always
	// sets it; nil leaves both steps out (the tests of the other steps).
	Setup *Setup

	// Recipe is --recipe URL: the instances come from a checked recipe
	// (DESIGN.md section 20). Fetcher downloads it.
	Recipe        string
	Fetcher       *recipe.Fetcher
	Add           bool   // --add: the host is paired; no token, no pairing
	ReplaceConfig bool   // --replace-config: write the recipe's NAME.toml over the one there, with a backup
	Host          string // the value of @HOST@; "" is the host name (tests set it)

	local map[string]*local
}

// DefaultAPI is the Bot API of Telegram.
const DefaultAPI = "https://api.telegram.org"

// Usage is the help text of the command.
const Usage = `pullgate bootstrap [--token-stdin] [--api-url URL] [--add]
        [--recipe URL [--gh PATH] [--bundle NAME] [--signer-workflow PATH] [--replace-config]]
        --instance NAME --repo URL [--branch B] [--apply PROGRAM] [--known-hosts FILE]
        [--recipe-instance NAME] [--instance NAME2 ...]`

// Parse reads the arguments after "bootstrap". There is no flag for the
// owner or the chat ID, on purpose.
func Parse(args []string) (*Options, error) {
	o := &Options{
		EtcDir: "/etc/pullgate", APIURL: DefaultAPI,
		AskPassword:   []string{"systemd-ask-password", "--timeout=0", "Telegram bot token (from BotFather):"},
		SSHKeygen:     "ssh-keygen",
		ListenerGroup: "pullgate",
		PairTTL:       10 * time.Minute, RemoteWait: 30 * time.Minute, RemoteEvery: 10 * time.Second,
		PollSeconds: 25,
		Stdin:       os.Stdin, Out: io.Discard, Err: io.Discard, Now: time.Now,
		Fetcher: &recipe.Fetcher{Dir: recipe.DefaultDir, GH: "/usr/bin/gh"},
	}
	var cur *Instance
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--token-stdin":
			o.TokenStdin = true
			continue
		case "--add":
			o.Add = true
			continue
		case "--replace-config":
			o.ReplaceConfig = true
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("%s: a value is missing, or the option is unknown. Fix: see pullgate --help", a)
		}
		v := args[i+1] //nolint:gosec // checked just above
		i++
		switch a {
		case "--api-url":
			o.APIURL = v
		case "--recipe":
			o.Recipe = v
		case "--gh":
			o.Fetcher.GH = v
		case "--bundle":
			o.Fetcher.Bundle = v
		case "--signer-workflow":
			o.Fetcher.Workflow = v
		case "--instance":
			o.Instances = append(o.Instances, Instance{Name: v, Branch: "main"})
			cur = &o.Instances[len(o.Instances)-1]
		case "--repo", "--branch", "--apply", "--known-hosts", "--recipe-instance":
			if cur == nil {
				return nil, fmt.Errorf("%s comes before any --instance. Fix: give --instance NAME first", a)
			}
			switch a {
			case "--repo":
				cur.URL = v
			case "--branch":
				cur.Branch, cur.branchSet = v, true
			case "--recipe-instance":
				cur.From = v
			case "--apply":
				cur.Apply = v
			default:
				cur.KnownHosts = v
			}
		default:
			return nil, fmt.Errorf("%s: unknown option. The owner and chat IDs cannot be given: the pairing takes them from Telegram. Fix: see pullgate --help", a)
		}
	}
	return o, nil
}

func (o *Options) say(format string, a ...any) { _, _ = fmt.Fprintf(o.Out, format+"\n", a...) }

// childEnv is the whole environment of every program bootstrap starts.
// It never holds the token.
func childEnv() []string {
	return []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
}

// check looks at every instance before anything is asked or written.
func (o *Options) check() error {
	if len(o.Instances) == 0 {
		return errors.New("no instance. Fix: give --instance NAME --repo URL")
	}
	// Another server would see the token and every message, and could
	// plant the IDs. Only the real one, or a local fake for tests.
	if err := config.CheckAPIURL(o.APIURL); err != nil {
		return fmt.Errorf("--api-url: %w", err)
	}
	if o.APIURL != DefaultAPI && !strings.HasPrefix(o.APIURL, "http://127.0.0.1:") {
		return fmt.Errorf("--api-url %q: bootstrap talks only to %s, or to a test server on http://127.0.0.1:PORT. Another server would see the token and could plant the owner ID", o.APIURL, DefaultAPI)
	}
	if o.Add && (o.TokenStdin || o.APIURL != DefaultAPI) {
		return errors.New("--add keeps the token and the Bot API of listener.toml: it takes no --token-stdin and no --api-url. Fix: leave them out, or run bootstrap without --add to pair again")
	}
	if o.Recipe == "" && (o.ReplaceConfig || o.Fetcher.Bundle != "" || o.Fetcher.Workflow != "" || o.Fetcher.GH != "/usr/bin/gh") {
		return errors.New("--replace-config, --gh, --bundle and --signer-workflow go with --recipe URL. Fix: give --recipe, or leave them out")
	}
	if o.Recipe != "" {
		if _, err := recipe.ParseURL(o.Recipe); err != nil {
			return fmt.Errorf("--recipe: %w", err)
		}
	}
	// On a new host the setup makes the config directory: then only the
	// directories above it are checked here.
	etc := o.EtcDir
	if _, err := os.Lstat(etc); errors.Is(err, fs.ErrNotExist) && o.Setup != nil {
		etc = filepath.Dir(etc)
	}
	if err := safedir.CheckParents(etc, uint32(os.Geteuid())); err != nil { //nolint:gosec // a uid fits in uint32
		return fmt.Errorf("%s is not safe: %w", etc, err)
	}
	seen := map[string]bool{}
	for _, in := range o.Instances {
		if !config.ValidName(in.Name) || seen[in.Name] {
			return fmt.Errorf("--instance %q: not a valid name, or given twice. Fix: use [a-z0-9][a-z0-9-]{0,31}, once", in.Name)
		}
		seen[in.Name] = true
		local, err := gitx.LocalPath(in.URL)
		if err != nil {
			return fmt.Errorf("--instance %s --repo: %w", in.Name, err)
		}
		if !gitx.ValidBranch(in.Branch) {
			return fmt.Errorf("--instance %s --branch %q: not a plain branch name", in.Name, in.Branch)
		}
		if o.Recipe != "" && in.Apply != "" {
			return fmt.Errorf("--instance %s --apply: the recipe's template names the apply program. Fix: leave --apply out with --recipe", in.Name)
		}
		if o.Recipe == "" && in.From != "" {
			return fmt.Errorf("--instance %s --recipe-instance goes with --recipe URL. Fix: give --recipe, or leave it out", in.Name)
		}
		if in.Apply != "" && !filepath.IsAbs(in.Apply) {
			return fmt.Errorf("--instance %s --apply %q: not an absolute path. Fix: give the full path of the apply program", in.Name, in.Apply)
		}
		if _, err := os.Lstat(o.instanceConfig(in.Name)); errors.Is(err, os.ErrNotExist) && in.Apply == "" && o.Recipe == "" {
			return fmt.Errorf("--instance %s: %s does not exist, and --apply is missing. Fix: give --apply /path/of/the/apply/program", in.Name, o.instanceConfig(in.Name))
		}
		if local != "" {
			continue
		}
		host, err := sshHost(in.URL)
		if err != nil {
			return fmt.Errorf("--instance %s --repo: %w", in.Name, err)
		}
		if in.KnownHosts == "" && host != "github.com" {
			return fmt.Errorf("--instance %s: pullgate knows the host keys of github.com only. Fix: run ssh-keyscan %s > FILE, compare the fingerprints with the ones your git host publishes, and give --known-hosts FILE", in.Name, host)
		}
	}
	if o.Add {
		l, err := o.loadPaired()
		if err != nil {
			return err
		}
		o.say("--add: the host is paired. Kept from %s: owner_id %d, chat_id %d, the instances %s.", o.listenerPath(), l.OwnerID, l.ChatID, strings.Join(l.Instances, ", "))
	}
	if o.Recipe != "" {
		return o.locals()
	}
	return nil
}

func (o *Options) instanceConfig(name string) string { return filepath.Join(o.EtcDir, name+".toml") }

// Run is one "pullgate bootstrap".
func Run(ctx context.Context, o *Options) error {
	if err := o.check(); err != nil {
		return err
	}
	var rr *recipeRun
	if o.Recipe != "" {
		var err error
		if rr, err = o.fetchRecipe(ctx); err != nil {
			return err
		}
	}
	if o.Setup != nil {
		if err := o.users(ctx); err != nil {
			return err
		}
		if err := o.dirs(); err != nil {
			return err
		}
		if err := o.units(ctx); err != nil {
			return err
		}
	}
	gid, err := o.listenerGID()
	if err != nil {
		return err
	}
	if !o.Add {
		if err := o.pairAndWrite(ctx, gid); err != nil {
			return err
		}
	}
	for _, in := range o.Instances {
		if err := o.instance(ctx, in, rr); err != nil {
			return err
		}
		if o.Setup != nil {
			if err := o.instanceDirs(in.Name); err != nil {
				return err
			}
		}
	}
	if rr != nil {
		for _, in := range o.Instances {
			if err := o.runSetup(ctx, in.Name, rr); err != nil {
				return err
			}
		}
		for _, in := range o.Instances {
			if err := o.recipeConfig(in.Name, rr); err != nil {
				return err
			}
			if err := o.writeRecord(in.Name, rr); err != nil {
				return err
			}
		}
	}
	if o.Add {
		// Last: the listener asks for a new instance only once it is set up.
		if err := o.addInstances(gid); err != nil {
			return err
		}
	}
	if o.Setup == nil {
		return nil
	}
	if err := o.enable(ctx); err != nil {
		return err
	}
	o.say("Done. The first question comes when the timer runs, or at once with: systemctl start pullgate-run@%s.service", o.Instances[0].Name)
	return nil
}

// pairAndWrite reads the token, pairs the chat, and writes bot-token and
// listener.toml.
func (o *Options) pairAndWrite(ctx context.Context, gid int) error {
	token, err := o.readToken(ctx)
	if err != nil {
		return err
	}
	tg := telegram.New(o.APIURL, token)
	me, err := tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("check the token with getMe: %w. Fix: copy the token from BotFather again", err)
	}
	if o.APIURL != DefaultAPI {
		o.say("WARNING: the Bot API is %s, not %s. That server sees the token and every tap. Use it only for tests.", o.APIURL, DefaultAPI)
	}
	o.say("The token works: the bot is @%s.", me.Username)
	owner, chat, err := o.pair(ctx, tg, me.Username)
	if err != nil {
		if _, serr := os.Lstat(filepath.Join(o.EtcDir, "listener.toml")); serr == nil {
			o.say("Note: an old %s exists. This run did not write it. Remove it if you did not make it.", filepath.Join(o.EtcDir, "listener.toml"))
		}
		return err
	}
	if err := o.writeListener(token, owner, chat, gid); err != nil {
		return err
	}
	o.say("Paired. Wrote %s and %s.", filepath.Join(o.EtcDir, "bot-token"), filepath.Join(o.EtcDir, "listener.toml"))
	_, _ = tg.SendMessage(ctx, chat, "✅ pullgate bootstrap: this chat is paired. Questions of the instances come here.", nil, 0)
	return nil
}

func (o *Options) listenerGID() (int, error) {
	g, err := user.LookupGroup(o.ListenerGroup)
	if err != nil {
		return 0, fmt.Errorf("the group %q: %w. Fix: make the users first, as the README says", o.ListenerGroup, err)
	}
	return strconv.Atoi(g.Gid)
}

var tokenRE = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{20,100}$`)

// readToken reads the token from systemd-ask-password (no echo, not in
// the shell history, not an argument), or with --token-stdin from the
// first line of stdin. The token is never printed.
func (o *Options) readToken(ctx context.Context) (string, error) {
	var raw []byte
	if o.TokenStdin {
		line, err := bufio.NewReader(io.LimitReader(o.Stdin, 256)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read the token from stdin: %w", err)
		}
		raw = []byte(line)
	} else {
		cmd := exec.CommandContext(ctx, o.AskPassword[0], o.AskPassword[1:]...) //nolint:gosec // fixed program
		cmd.Env = childEnv()
		cmd.Stderr = o.Err
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("read the token with %s: %w. Fix: run pullgate bootstrap on a console, or use --token-stdin", o.AskPassword[0], err)
		}
		raw = out.Bytes()
	}
	tok := strings.TrimSpace(string(raw))
	if !tokenRE.MatchString(tok) {
		return "", errors.New("this is not a bot token (it should look like 123456:ABC-...). Fix: copy the token from BotFather again")
	}
	return tok, nil
}

// MaxWrong is the most wrong codes before the pairing stops.
const MaxWrong = 5

// newCode is 8 random digits.
func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(100000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%08d", n.Int64()), nil
}

// pair prints a one-time code and waits for it in a private chat with the
// bot. It returns the user and the chat of the message that carries it.
func (o *Options) pair(ctx context.Context, tg *telegram.Client, bot string) (int64, int64, error) {
	code, err := newCode()
	if err != nil {
		return 0, 0, err
	}
	if err := tg.DeleteWebhook(ctx); err != nil {
		return 0, 0, fmt.Errorf("delete the webhook: %w", err)
	}
	// Messages from before the code cannot carry it: skip them.
	offset := int64(0)
	for range 1000 { // a batch has at most 100 updates
		old, err := tg.GetUpdates(ctx, offset, 0)
		if err != nil {
			return 0, 0, fmt.Errorf("read the updates: %w. Fix: stop pullgate-listen.service while bootstrap runs", err)
		}
		if len(old) == 0 {
			break
		}
		for _, u := range old {
			offset = u.UpdateID + 1
		}
	}
	deadline := o.Now().Add(o.PairTTL)
	o.say("Pairing: in Telegram, open a private chat with @%s and send this code:\n\n    %s %s\n\nIt works once, until %s. Nobody needs to type an ID.",
		bot, code[:4], code[4:], deadline.Format("15:04"))
	wrong := 0
	for {
		left := deadline.Sub(o.Now())
		if left <= 0 {
			return 0, 0, errors.New("the code did not come in time. Nothing was written. Fix: run pullgate bootstrap again")
		}
		wait := min(o.PollSeconds, int(left/time.Second))
		ups, err := tg.GetUpdates(ctx, offset, wait)
		if err != nil {
			return 0, 0, fmt.Errorf("read the updates: %w. Nothing was written", err)
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil {
				continue
			}
			got := strings.Map(func(r rune) rune {
				if r == ' ' || r == '-' {
					return -1
				}
				return r
			}, strings.TrimSpace(m.Text))
			if got == "" || strings.Trim(got, "0123456789") != "" {
				continue // not a code, for example /start
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(code)) != 1 {
				wrong++
				if wrong >= MaxWrong {
					return 0, 0, fmt.Errorf("%d wrong codes came. Someone else may be trying. Nothing was written. Fix: run pullgate bootstrap again", wrong)
				}
				continue
			}
			if m.Chat.Type != "private" || m.From == nil || m.From.IsBot || m.From.ID <= 0 || m.From.ID != m.Chat.ID {
				return 0, 0, errors.New("the code came from a group or a channel, not from a private chat. The code is used up and nothing was written. Fix: run pullgate bootstrap again, and send the new code in the private chat with the bot")
			}
			// Confirm the updates, so the listener does not see the code.
			_, _ = tg.GetUpdates(ctx, offset, 0)
			return m.From.ID, m.Chat.ID, nil
		}
	}
}

// listenerFile is listener.toml as bootstrap writes it.
type listenerFile struct {
	OwnerID   int64    `toml:"owner_id"`
	ChatID    int64    `toml:"chat_id"`
	Instances []string `toml:"instances"`
	APIURL    string   `toml:"api_url,omitempty"`
}

const header = "# Written by pullgate bootstrap on %s. pullgate owns this file:\n# no other config tool may write it (DESIGN.md section 17).\n"

func (o *Options) writeListener(token string, owner, chat int64, gid int) error {
	lf := listenerFile{OwnerID: owner, ChatID: chat}
	for _, in := range o.Instances {
		lf.Instances = append(lf.Instances, in.Name)
	}
	if o.APIURL != DefaultAPI {
		lf.APIURL = o.APIURL
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, header, o.Now().Format("2006-01-02"))
	if err := toml.NewEncoder(&b).Encode(lf); err != nil {
		return err
	}
	// listener.toml first: if the token write fails, the listener does not
	// start with an old token and the new IDs mixed.
	if err := writeOwned(filepath.Join(o.EtcDir, "listener.toml"), b.Bytes(), 0o640, gid); err != nil {
		return err
	}
	return state.WriteFileAtomic(filepath.Join(o.EtcDir, "bot-token"), []byte(token+"\n"), 0o600)
}

// writeOwned writes a file atomically with its group set before the rename.
func writeOwned(path string, data []byte, mode os.FileMode, gid int) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chown(-1, gid)
	}
	if werr == nil {
		werr = f.Chmod(mode)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("write %s: %w", path, werr)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// GitHubKnownHosts is the ed25519 host key of github.com, as GitHub
// publishes it. Its fingerprint is GitHubFingerprint.
const GitHubKnownHosts = "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"

// GitHubFingerprint is the fingerprint that GitHub publishes for it.
const GitHubFingerprint = "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU"

// sshHost is the host of an ssh URL (ssh://[user@]host[:port]/path or
// user@host:path).
func sshHost(raw string) (string, error) {
	if strings.HasPrefix(raw, "ssh://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("the url %q has no host", raw)
		}
		if p := u.Port(); p != "" && p != "22" {
			return u.Hostname() + ":" + p, nil
		}
		return u.Hostname(), nil
	}
	h := strings.SplitN(raw, ":", 2)[0]
	if i := strings.LastIndex(h, "@"); i >= 0 {
		h = h[i+1:]
	}
	if h == "" {
		return "", fmt.Errorf("the url %q has no host", raw)
	}
	return h, nil
}

// Fingerprints are the SHA256 fingerprints of the keys in a known_hosts
// text, one "type fingerprint" per key. A line that is not a key is an error.
func Fingerprints(text string) ([]string, error) {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 3 {
			return nil, fmt.Errorf("the line %q is not a known_hosts line (host type key)", l)
		}
		blob, err := base64.StdEncoding.DecodeString(f[2])
		if err != nil {
			return nil, fmt.Errorf("the key of the line for %q is not base64", f[0])
		}
		sum := sha256.Sum256(blob)
		out = append(out, f[1]+" SHA256:"+base64.RawStdEncoding.EncodeToString(sum[:]))
	}
	if len(out) == 0 {
		return nil, errors.New("no host key")
	}
	return out, nil
}

// instance makes the deploy key and the known hosts file, writes a
// minimal config if there is none, and waits until git can read the repo.
func (o *Options) instance(ctx context.Context, in Instance, rr *recipeRun) error {
	if rr != nil {
		return o.recipeInstance(ctx, in, rr)
	}
	if _, err := os.Lstat(o.instanceConfig(in.Name)); err == nil {
		// A kept config: probe with its own repo, key and known hosts.
		c, err := config.Load(o.EtcDir, in.Name, uint32(os.Geteuid())) //nolint:gosec // a uid fits in uint32
		if err != nil {
			return err
		}
		o.say("Instance %s: %s exists; it is kept, with its repo, deploy key and known hosts.", in.Name, c.Path)
		return o.waitRemote(ctx, Instance{Name: in.Name, URL: c.Repo.URL, Branch: c.Repo.Branch}, c.Repo.DeployKey, c.Repo.KnownHosts)
	}
	local, _ := gitx.LocalPath(in.URL)
	key, kh := filepath.Join(o.EtcDir, in.Name+".key"), filepath.Join(o.EtcDir, in.Name+".known_hosts")
	if local == "" {
		if err := o.deployKey(ctx, in.Name, key); err != nil {
			return err
		}
		if err := o.knownHosts(in, kh); err != nil {
			return err
		}
	} else {
		key, kh = "", ""
	}
	if err := o.minimalConfig(in, key, kh); err != nil {
		return err
	}
	return o.waitRemote(ctx, in, key, kh)
}

// recipeInstance is instance for --recipe: the config comes from the
// template. A kept config keeps its repo, key and known hosts.
func (o *Options) recipeInstance(ctx context.Context, in Instance, rr *recipeRun) error {
	l := o.local[in.Name]
	if l.kept == nil && l.key != "" {
		if err := o.deployKey(ctx, in.Name, l.key); err != nil {
			return err
		}
		if err := o.knownHosts(in, l.knownHosts); err != nil {
			return err
		}
	}
	// NAME.toml is written after the setup of the recipe: a failed setup
	// leaves the config that was there, or none.
	return o.waitRemote(ctx, Instance{Name: in.Name, URL: l.url, Branch: l.branch}, l.key, l.knownHosts)
}

func (o *Options) deployKey(ctx context.Context, name, key string) error {
	fi, err := os.Lstat(key)
	if errors.Is(err, os.ErrNotExist) {
		if _, perr := os.Lstat(key + ".pub"); perr == nil {
			return fmt.Errorf("%s.pub exists without %s. Fix: remove %s.pub, then run bootstrap again", key, key, key)
		}
		cmd := exec.CommandContext(ctx, o.SSHKeygen, "-q", "-t", "ed25519", "-N", "", "-C", "pullgate-"+name, "-f", key) //nolint:gosec // fixed program, checked name
		cmd.Env = childEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-keygen for %s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
	} else if err != nil {
		return err
	} else if st, ok := fi.Sys().(*syscall.Stat_t); !fi.Mode().IsRegular() || !ok || st.Uid != uint32(os.Geteuid()) { //nolint:gosec // a uid fits in uint32
		return fmt.Errorf("%s is not a regular file of root. Fix: remove it, then run bootstrap again", key)
	} else {
		o.say("Instance %s: the deploy key %s exists; it is kept. Remove it to make a new one.", name, key)
	}
	if err := os.Chmod(key, 0o600); err != nil {
		return err
	}
	pub, err := os.ReadFile(key + ".pub") //nolint:gosec // root's own config directory
	if err != nil {
		return fmt.Errorf("read the public key: %w", err)
	}
	o.say("\nInstance %s: add this public key to the repo as a deploy key, READ-ONLY (no write access).\nThe private key stays on this host, in %s.\n\n    %s\n", name, key, strings.TrimSpace(string(pub)))
	return nil
}

func (o *Options) knownHosts(in Instance, kh string) error {
	text := GitHubKnownHosts
	from := "the GitHub key that pullgate carries. Compare it with the fingerprint that GitHub publishes in its docs (\"GitHub's SSH key fingerprints\")"
	if in.KnownHosts != "" {
		b, err := os.ReadFile(in.KnownHosts)
		if err != nil {
			return fmt.Errorf("--known-hosts: %w", err)
		}
		text, from = string(b), in.KnownHosts+". Compare them with the fingerprints that your git host publishes"
	}
	fps, err := Fingerprints(text)
	if err != nil {
		return fmt.Errorf("--instance %s known hosts: %w", in.Name, err)
	}
	if err := state.WriteFileAtomic(kh, []byte(text), 0o600); err != nil {
		return err
	}
	o.say("Instance %s: the host keys come from %s:\n    %s", in.Name, from, strings.Join(fps, "\n    "))
	return nil
}

type instanceFile struct {
	Mode      string `toml:"mode"`
	OnFailure string `toml:"on_failure"`
	Repo      struct {
		URL        string `toml:"url"`
		Branch     string `toml:"branch"`
		DeployKey  string `toml:"deploy_key,omitempty"`
		KnownHosts string `toml:"known_hosts,omitempty"`
	} `toml:"repo"`
	Commands struct {
		Apply     []string `toml:"apply"`
		CheckUser string   `toml:"check_user"`
	} `toml:"commands"`
}

func (o *Options) minimalConfig(in Instance, key, kh string) error {
	p := o.instanceConfig(in.Name)
	if _, err := os.Lstat(p); err == nil {
		o.say("Instance %s: %s exists; it is kept.", in.Name, p)
		return nil
	}
	var f instanceFile
	f.Mode, f.OnFailure = config.ModeAsk, config.OnFailurePause
	f.Repo.URL, f.Repo.Branch, f.Repo.DeployKey, f.Repo.KnownHosts = in.URL, in.Branch, key, kh
	f.Commands.Apply, f.Commands.CheckUser = []string{in.Apply}, "pullgate-check-"+in.Name
	var b bytes.Buffer
	fmt.Fprintf(&b, header, o.Now().Format("2006-01-02"))
	if err := toml.NewEncoder(&b).Encode(f); err != nil {
		return err
	}
	if err := state.WriteFileAtomic(p, b.Bytes(), 0o600); err != nil {
		return err
	}
	o.say("Instance %s: wrote a minimal %s. Add check, health and watch as the README says.", in.Name, p)
	return nil
}

// waitRemote waits until git ls-remote can read the branch: the owner
// must add the deploy key first.
func (o *Options) waitRemote(ctx context.Context, in Instance, key, kh string) error {
	tmp, err := os.MkdirTemp("", "pullgate-bootstrap-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	g := &gitx.Git{Dir: filepath.Join(tmp, "probe.git"), Home: tmp, UID: uint32(os.Geteuid()), Key: key, KnownHosts: kh, Timeout: time.Minute} //nolint:gosec // a uid fits in uint32
	if err := g.Init(ctx); err != nil {
		return err
	}
	until := o.Now().Add(o.RemoteWait)
	said := false
	for {
		out, err := g.Run(ctx, "ls-remote", "--heads", "--", in.URL, "refs/heads/"+in.Branch)
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			o.say("Instance %s: git can read the branch %s.", in.Name, in.Branch)
			return nil
		}
		if err == nil {
			err = fmt.Errorf("the branch %s does not exist", in.Branch)
		}
		if !o.Now().Before(until) {
			return fmt.Errorf("instance %s: git cannot read the repo: %w. Fix: add the deploy key, check the url and the branch, then run pullgate bootstrap again", in.Name, err)
		}
		if !said {
			o.say("Instance %s: waiting until git can read the repo (add the deploy key now)…", in.Name)
			said = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.RemoteEvery):
		}
	}
}
