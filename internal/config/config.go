// Package config reads the instance config (/etc/pullgate/NAME.toml) and the
// listener config, and checks the files they name (DESIGN.md sections 4, 5
// and 13). Nothing here is ever read from the repo (rule 1).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/code6x/pullgate/internal/gitx"
	"github.com/code6x/pullgate/internal/safedir"
)

// Modes and failure modes.
const (
	ModeAsk  = "ask"
	ModeAuto = "auto"
	ModeOff  = "off"

	OnFailureRollback = "rollback"
	OnFailurePause    = "pause"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidName reports whether s is a valid instance name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Duration is a time.Duration written as "12h" in TOML.
type Duration struct{ time.Duration }

// UnmarshalText reads a duration such as "5m" or "12h".
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Repo is the [repo] section.
type Repo struct {
	URL        string `toml:"url"`
	Branch     string `toml:"branch"`
	DeployKey  string `toml:"deploy_key"`
	KnownHosts string `toml:"known_hosts"`
}

// Commands is the [commands] section.
type Commands struct {
	Check        []string `toml:"check"`
	Summary      []string `toml:"summary"`
	Apply        []string `toml:"apply"`
	Health       []string `toml:"health"`
	CheckUser    string   `toml:"check_user"`
	CheckTimeout Duration `toml:"check_timeout"`
	ApplyTimeout Duration `toml:"apply_timeout"`
}

// Watch is the [watch] section.
type Watch struct {
	Paths []string `toml:"paths"`
}

// Limits is the [limits] section.
type Limits struct {
	QuestionsPerDay int      `toml:"questions_per_day"`
	ApprovalTTL     Duration `toml:"approval_ttl"`
	MaxFailedTries  int      `toml:"max_failed_tries"`
	MaxFiles        int      `toml:"max_files"`
	MaxTreeBytes    int64    `toml:"max_tree_bytes"`
	AllowOlder      bool     `toml:"allow_older"`
	RefuseSymlinks  bool     `toml:"refuse_symlinks"`
}

// ListenerUser is the [listener] section of an instance.
type ListenerUser struct {
	User string `toml:"user"`
}

// Instance is one instance config.
type Instance struct {
	Name      string       `toml:"-"`
	Path      string       `toml:"-"`
	Mode      string       `toml:"mode"`
	OnFailure string       `toml:"on_failure"`
	Host      string       `toml:"host"`
	Repo      Repo         `toml:"repo"`
	Commands  Commands     `toml:"commands"`
	Watch     Watch        `toml:"watch"`
	Limits    Limits       `toml:"limits"`
	Listener  ListenerUser `toml:"listener"`
}

// MaxTTL is the longest approval_ttl.
const MaxTTL = 7 * 24 * time.Hour

func (c *Instance) defaults() {
	if c.Mode == "" {
		c.Mode = ModeAsk
	}
	if c.OnFailure == "" {
		c.OnFailure = OnFailurePause // the safe choice (issue #1)
	}
	if c.Host == "" {
		c.Host, _ = os.Hostname()
	}
	if c.Repo.Branch == "" {
		c.Repo.Branch = "main"
	}
	if c.Commands.CheckUser == "" {
		c.Commands.CheckUser = "pullgate-check"
	}
	if c.Commands.CheckTimeout.Duration == 0 {
		c.Commands.CheckTimeout.Duration = 5 * time.Minute
	}
	if c.Commands.ApplyTimeout.Duration == 0 {
		c.Commands.ApplyTimeout.Duration = 30 * time.Minute
	}
	if c.Limits.QuestionsPerDay == 0 {
		c.Limits.QuestionsPerDay = 10
	}
	if c.Limits.ApprovalTTL.Duration == 0 {
		c.Limits.ApprovalTTL.Duration = 12 * time.Hour
	}
	if c.Limits.MaxFailedTries == 0 {
		c.Limits.MaxFailedTries = 1
	}
	if c.Limits.MaxFiles == 0 {
		c.Limits.MaxFiles = 100000
	}
	if c.Limits.MaxTreeBytes == 0 {
		c.Limits.MaxTreeBytes = 1 << 30
	}
	if c.Listener.User == "" {
		c.Listener.User = "pullgate"
	}
}

func (c *Instance) errf(key, format string, a ...any) error {
	return fmt.Errorf("%s: %s: %s", c.Path, key, fmt.Sprintf(format, a...))
}

func (c *Instance) validate() error {
	switch c.Mode {
	case ModeAsk, ModeAuto, ModeOff:
	default:
		return c.errf("mode", "%q is not a mode. Fix: use \"ask\", \"auto\" or \"off\"", c.Mode)
	}
	switch c.OnFailure {
	case OnFailureRollback, OnFailurePause:
	default:
		return c.errf("on_failure", "%q is not allowed. Fix: use \"rollback\" or \"pause\"", c.OnFailure)
	}
	if strings.ContainsAny(c.Host, "<>&\n\r") || len(c.Host) > 64 {
		return c.errf("host", "the name must be at most 64 characters, without <, >, & or line breaks")
	}
	if _, err := gitx.LocalPath(c.Repo.URL); err != nil {
		return c.errf("[repo] url", "%v", err)
	}
	if !gitx.ValidBranch(c.Repo.Branch) {
		return c.errf("[repo] branch", "%q is not a plain branch name. Fix: use a name like \"main\"", c.Repo.Branch)
	}
	if len(c.Commands.Apply) == 0 {
		return c.errf("[commands] apply", "it is missing. Fix: set apply = [\"/absolute/path\", ...]")
	}
	for key, argv := range map[string][]string{"check": c.Commands.Check, "summary": c.Commands.Summary, "apply": c.Commands.Apply, "health": c.Commands.Health} {
		if len(argv) > 0 && !filepath.IsAbs(argv[0]) {
			return c.errf("[commands] "+key, "the program %q is not an absolute path. Fix: give the full path, like \"/usr/local/bin/x\"", argv[0])
		}
		for _, a := range argv {
			if strings.ContainsRune(a, 0) {
				return c.errf("[commands] "+key, "an argument holds a NUL")
			}
		}
	}
	if c.Commands.CheckUser == "root" {
		return c.errf("[commands] check_user", "check and summary must never run as root (rule 3). Fix: use an unprivileged user, like \"pullgate-check\"")
	}
	if c.Limits.ApprovalTTL.Duration <= 0 || c.Limits.ApprovalTTL.Duration > MaxTTL {
		return c.errf("[limits] approval_ttl", "must be more than 0 and at most 168h")
	}
	if c.Limits.QuestionsPerDay < 0 || c.Limits.MaxFailedTries < 0 || c.Limits.MaxFiles < 0 || c.Limits.MaxTreeBytes < 0 {
		return c.errf("[limits]", "a limit is below 0. Fix: use a positive number")
	}
	if c.Commands.CheckTimeout.Duration < 0 || c.Commands.ApplyTimeout.Duration < 0 {
		return c.errf("[commands]", "a timeout is below 0")
	}
	for _, p := range c.Watch.Paths {
		if p == "" || strings.HasPrefix(p, "/") {
			return c.errf("[watch] paths", "%q is empty or absolute. Fix: use a path inside the repo, like \"policy/**\"", p)
		}
	}
	return nil
}

// Load reads the instance config NAME from dir. The file must be owned by
// uid and not writable by others, with safe parent directories.
func Load(dir, name string, uid uint32) (*Instance, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%q is not an instance name. Fix: use [a-z0-9][a-z0-9-]{0,31}", name)
	}
	path := filepath.Join(dir, name+".toml")
	if err := checkFile(path, uid, false); err != nil {
		return nil, err
	}
	c := &Instance{Name: name, Path: path}
	if err := decodeFile(path, c); err != nil {
		return nil, err
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// CheckGateFiles checks the deploy key, the known hosts file and the
// program of each command (DESIGN.md section 4).
func (c *Instance) CheckGateFiles(uid uint32) error {
	local, _ := gitx.LocalPath(c.Repo.URL)
	if local == "" {
		if c.Repo.DeployKey == "" || c.Repo.KnownHosts == "" {
			return c.errf("[repo]", "an ssh url needs deploy_key and known_hosts. Fix: set both")
		}
		if err := checkFile(c.Repo.DeployKey, uid, true); err != nil {
			return c.errf("[repo] deploy_key", "%v", err)
		}
		if err := checkFile(c.Repo.KnownHosts, uid, false); err != nil {
			return c.errf("[repo] known_hosts", "%v", err)
		}
	}
	for key, argv := range map[string][]string{"check": c.Commands.Check, "summary": c.Commands.Summary, "apply": c.Commands.Apply, "health": c.Commands.Health} {
		if len(argv) == 0 {
			continue
		}
		if err := checkProgram(argv[0], uid); err != nil {
			return c.errf("[commands] "+key, "%v", err)
		}
	}
	return nil
}

// checkProgram checks a program file like a gate file. A symlink on the
// way (such as /bin -> usr/bin) is allowed only when the directory that
// holds it passes the check too, and the target passes it.
func checkProgram(p string, uid uint32) error {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("the program %s: %w. Fix: install it, or fix the path", p, err)
	}
	if real != p {
		if err := safedir.CheckParents(filepath.Dir(p), uid); err != nil && !errors.Is(err, safedir.ErrSymlink) {
			return fmt.Errorf("the program %s: %w", p, err)
		}
		if err := checkLinks(p, uid); err != nil {
			return err
		}
	}
	return checkFile(real, uid, false)
}

// checkLinks checks that each symlink on the way to p is owned by uid
// (or root) and that the directory that holds it passes the check.
func checkLinks(p string, uid uint32) error {
	cur := "/"
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(p), "/"), "/") {
		next := filepath.Join(cur, part)
		fi, err := os.Lstat(next)
		if err != nil {
			return fmt.Errorf("the program %s: %w", p, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return err
			}
			if err := safedir.CheckParents(real, uid); err != nil {
				return fmt.Errorf("the program %s: the symlink %s is in an unsafe directory: %w", p, next, err)
			}
			if err := safedir.CheckOwner(fi, uid); err != nil {
				return fmt.Errorf("the program %s: the symlink %s: %w", p, next, err)
			}
		}
		if r, err := filepath.EvalSymlinks(next); err == nil {
			cur = r
		} else {
			return err
		}
	}
	return nil
}

// checkFile checks a file and the directories above it.
func checkFile(path string, uid uint32, private bool) error {
	if err := safedir.CheckParents(filepath.Dir(path), uid); err != nil {
		return err
	}
	if err := safedir.CheckFile(path, uid, private); err != nil {
		return err
	}
	return nil
}

func decodeFile(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // the path is checked by the caller
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	md, err := toml.Decode(string(b), v)
	if err != nil {
		return fmt.Errorf("%s: %w. Fix: correct the TOML", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return fmt.Errorf("%s: unknown key %q. Fix: remove it, or check its spelling in DESIGN.md section 5", path, und[0].String())
	}
	return nil
}

// Listener is the listener config.
type Listener struct {
	Path      string   `toml:"-"`
	TokenFile string   `toml:"token_file"`
	OwnerID   int64    `toml:"owner_id"`
	ChatID    int64    `toml:"chat_id"`
	Instances []string `toml:"instances"`
	SpoolDir  string   `toml:"spool_dir"`
	StateFile string   `toml:"state_file"`
	APIURL    string   `toml:"api_url"`
}

// LoadListener reads the listener config. The file must be owned by
// configUID (root) and not writable by others.
func LoadListener(path string, configUID uint32) (*Listener, error) {
	if err := checkFile(path, configUID, false); err != nil {
		return nil, err
	}
	c := &Listener{Path: path}
	if err := decodeFile(path, c); err != nil {
		return nil, err
	}
	if c.SpoolDir == "" {
		c.SpoolDir = "/var/spool/pullgate"
	}
	if c.StateFile == "" {
		c.StateFile = "/var/lib/pullgate-listener/state.json"
	}
	if c.APIURL == "" {
		c.APIURL = "https://api.telegram.org"
	}
	if c.TokenFile == "" {
		if d := os.Getenv("CREDENTIALS_DIRECTORY"); d != "" {
			c.TokenFile = filepath.Join(d, "bot-token")
		}
	}
	switch {
	case c.TokenFile == "":
		return nil, fmt.Errorf("%s: token_file: it is missing. Fix: set token_file, or give a systemd credential named bot-token", path)
	case c.OwnerID <= 0:
		return nil, fmt.Errorf("%s: owner_id: it is missing. Fix: set the Telegram user ID of the owner", path)
	case c.ChatID == 0:
		return nil, fmt.Errorf("%s: chat_id: it is missing. Fix: set the chat ID (the private chat with the owner)", path)
	case len(c.Instances) == 0:
		return nil, fmt.Errorf("%s: instances: the list is empty. Fix: list the instance names", path)
	case !strings.HasPrefix(c.APIURL, "https://") && !strings.HasPrefix(c.APIURL, "http://127.0.0.1:"):
		return nil, fmt.Errorf("%s: api_url: it must be an https:// url", path)
	}
	for _, n := range c.Instances {
		if !ValidName(n) {
			return nil, fmt.Errorf("%s: instances: %q is not an instance name", path, n)
		}
	}
	return c, nil
}

// ReadToken reads the bot token. The file must be owned by uid (the
// listener user) and readable by nobody else. The token is never printed.
func (c *Listener) ReadToken(uid uint32) (string, error) {
	if err := safedir.CheckFile(c.TokenFile, uid, true); err != nil {
		if !strings.HasPrefix(c.TokenFile, os.Getenv("CREDENTIALS_DIRECTORY")+"/") || os.Getenv("CREDENTIALS_DIRECTORY") == "" {
			return "", fmt.Errorf("token_file: %w", err)
		}
	}
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read the token file %s: %w", c.TokenFile, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, " /\n\t") {
		return "", errors.New("the token file holds no valid bot token. Fix: put the token from BotFather in it, on one line")
	}
	return tok, nil
}
