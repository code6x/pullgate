// Package config reads the instance config (/etc/pullgate/NAME.toml) and the
// listener config, and checks the files they name (DESIGN.md sections 4, 5
// and 13). Nothing here is ever read from the repo (rule 1).
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/code6x/pullgate/internal/gitx"
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

// userRE is a user name: what useradd accepts, and nothing a shell or an
// option could read.
var userRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

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
	// Settle is how long the tip must stay the same before it is checked
	// and asked (DESIGN.md section 7). 0 asks at once.
	Settle Duration `toml:"settle"`
	// ReaskAfter asks a skipped or expired tip once more when its mark is
	// this old (DESIGN.md section 7). 0 never asks again.
	ReaskAfter Duration `toml:"reask_after"`
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
	// GitHub is the optional [github] block: the deployment reports
	// (DESIGN.md section 22). nil: no GitHub code runs.
	GitHub *GitHub `toml:"github"`
}

// GitHub is the [github] block of an instance.
type GitHub struct {
	Repository  string `toml:"repository"`
	Environment string `toml:"environment"`
	TokenFile   string `toml:"token_file"`
	// APIURL is "" when the key is not set: API() gives the default. So
	// a recipe rewrite keeps the block as the owner wrote it.
	APIURL string `toml:"api_url,omitempty"`
}

// DefaultGitHubAPI is the GitHub API that [github] api_url defaults to.
const DefaultGitHubAPI = "https://api.github.com"

// API is the URL of the GitHub API, without a slash at the end.
func (g *GitHub) API() string {
	if g.APIURL == "" {
		return DefaultGitHubAPI
	}
	return strings.TrimSuffix(g.APIURL, "/")
}

// MaxGitHubToken is the largest GitHub token file that root reads.
const MaxGitHubToken = 1024

var ghRepoRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)

// GitHubRepoOf returns "owner/repo" of a git url on github.com, or "" for
// a url that is not on GitHub. It knows git@github.com:owner/repo,
// ssh://git@github.com/owner/repo, ssh://git@ssh.github.com:443/owner/repo
// and https://github.com/owner/repo, each with or without ".git".
func GitHubRepoOf(raw string) string {
	var path string
	if rest, ok := strings.CutPrefix(raw, "git@"); ok && !strings.Contains(raw, "://") {
		host, p, ok := strings.Cut(rest, ":")
		if !ok || !strings.EqualFold(host, "github.com") {
			return ""
		}
		path = p
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		h := strings.ToLower(u.Hostname())
		switch {
		case (u.Scheme == "ssh" || u.Scheme == "https") && h == "github.com":
		case u.Scheme == "ssh" && h == "ssh.github.com":
		default:
			return ""
		}
		path = u.Path
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	return path
}

func (c *Instance) validateGitHub() error {
	g := c.GitHub
	if g == nil {
		return nil
	}
	switch {
	case g.Repository == "":
		return c.errf("[github] repository", "it is missing. Fix: set the config repo, like \"owner/config-repo\"")
	case !ghRepoRE.MatchString(g.Repository) || strings.HasSuffix(g.Repository, ".git") || strings.Trim(g.Repository[strings.Index(g.Repository, "/")+1:], ".") == "":
		return c.errf("[github] repository", "%q is not OWNER/REPO. Fix: write it like \"owner/config-repo\", without .git", g.Repository)
	case g.Environment == "":
		return c.errf("[github] environment", "it is missing. Fix: set one environment name for each host, like \"example-host\"")
	case !ValidName(g.Environment):
		return c.errf("[github] environment", "%q is not a valid name. Fix: use [a-z0-9][a-z0-9-]{0,31}, one name for each host", g.Environment)
	case g.TokenFile == "":
		return c.errf("[github] token_file", "it is missing. Fix: set the path of the token file, like \"/etc/pullgate/%s.github-token\"", c.Name)
	case !filepath.IsAbs(g.TokenFile) || filepath.Clean(g.TokenFile) != g.TokenFile:
		return c.errf("[github] token_file", "%q is not a clean absolute path. Fix: use a path like \"/etc/pullgate/%s.github-token\"", g.TokenFile, c.Name)
	}
	if err := CheckAPIURL(g.API(), DefaultGitHubAPI); err != nil {
		return c.errf("[github] api_url", "%v", err)
	}
	if r := GitHubRepoOf(c.Repo.URL); r != "" && !strings.EqualFold(r, g.Repository) {
		return c.errf("[github] repository", "%q is not the repo of [repo] url (%s). Fix: set repository to the repo that pullgate fetches", g.Repository, r)
	}
	return nil
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
	if c.Limits.Settle.Duration < 0 || c.Limits.Settle.Duration > MaxTTL {
		return c.errf("[limits] settle", "must be at least 0 and at most 168h. Fix: use a time like \"30m\", or \"0\" to ask at once")
	}
	if c.Limits.ReaskAfter.Duration < 0 {
		return c.errf("[limits] reask_after", "is below 0. Fix: use a time like \"24h\", or \"0\" to never ask again")
	}
	if c.Commands.CheckTimeout.Duration < 0 || c.Commands.ApplyTimeout.Duration < 0 {
		return c.errf("[commands]", "a timeout is below 0")
	}
	for _, p := range c.Watch.Paths {
		if p == "" || strings.HasPrefix(p, "/") {
			return c.errf("[watch] paths", "%q is empty or absolute. Fix: use a path inside the repo, like \"policy/**\"", p)
		}
	}
	if err := c.validateGitHub(); err != nil {
		return err
	}
	return validatePlatform(c)
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

// Parse checks the text of the instance config NAME, as Load does, but
// not the file: bootstrap checks a config before it writes it. path is
// only for the errors.
func Parse(path, name string, b []byte) (*Instance, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%q is not an instance name. Fix: use [a-z0-9][a-z0-9-]{0,31}", name)
	}
	c := &Instance{Name: name, Path: path}
	if err := decodeBytes(path, b, c); err != nil {
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

func decodeFile(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // the path is checked by the caller
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return decodeBytes(path, b, v)
}

func decodeBytes(path string, b []byte, v any) error {
	md, err := toml.Decode(string(b), v)
	if err != nil {
		return fmt.Errorf("%s: %w. Fix: correct the TOML", path, err)
	}
	if err := ExactKeys(md); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return fmt.Errorf("%s: unknown key %q. Fix: remove it, or check its spelling in DESIGN.md section 5", path, und[0].String())
	}
	return nil
}

var keyRE = regexp.MustCompile(`^[a-z0-9_]+$`)

// ExactKeys refuses a key that is not written in lower case. The TOML
// decoder matches a key to a field without case, so "Mode" would set
// mode, and could hide from a check that looks for "mode" (a recipe
// template, DESIGN.md section 20). Every key of pullgate is lower case.
func ExactKeys(md toml.MetaData) error {
	for _, k := range md.Keys() {
		for _, part := range k {
			if !keyRE.MatchString(part) {
				return fmt.Errorf("the key %q is not written as pullgate writes its keys (a-z, 0-9 and _). Fix: write it in lower case", k.String())
			}
		}
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
	Updates   Updates  `toml:"updates"`
}

// Updates is the [updates] table of the listener config: the updates of
// pullgate itself (DESIGN.md section 15). Root reads it; the listener
// only checks it.
type Updates struct {
	// Check: a notice for each new release, once a day. Default true.
	Check *bool `toml:"check"`
	// ListenerUser is the user of the listener, who owns the inbox of the
	// updates. Default "pullgate".
	ListenerUser string `toml:"listener_user"`
	// GH is the gh that checks the build attestation, as install.sh --gh.
	// Default: install.sh downloads gh and checks its SHA-256.
	GH string `toml:"gh"`
}

// Notices reports whether the daily notice of new releases is on.
func (u Updates) Notices() bool { return u.Check == nil || *u.Check }

// LoadListenerForUpdate reads the listener config for pullgate update,
// which runs as root. The file must be owned by root (uid) and not
// writable by others. It needs no token: root never reads the token.
func LoadListenerForUpdate(path string, uid uint32) (*Listener, error) {
	return loadListener(path, uid, false)
}

// LoadListener reads the listener config. The file must be owned by
// configUID (root) and not writable by others.
func LoadListener(path string, configUID uint32) (*Listener, error) {
	return loadListener(path, configUID, true)
}

func loadListener(path string, configUID uint32, needToken bool) (*Listener, error) {
	if err := checkFile(path, configUID, false); err != nil {
		return nil, err
	}
	c := &Listener{Path: path}
	if err := decodeFile(path, c); err != nil {
		return nil, err
	}
	listenerDefaults(c)
	if c.Updates.ListenerUser == "" {
		c.Updates.ListenerUser = "pullgate"
	}
	switch {
	case c.TokenFile == "" && needToken:
		return nil, fmt.Errorf("%s: token_file: it is missing. Fix: set token_file, or give a systemd credential named bot-token", path)
	case c.OwnerID <= 0:
		return nil, fmt.Errorf("%s: owner_id: it is missing. Fix: set the Telegram user ID of the owner", path)
	case c.ChatID == 0:
		return nil, fmt.Errorf("%s: chat_id: it is missing. Fix: set the chat ID (the private chat with the owner)", path)
	case len(c.Instances) == 0:
		return nil, fmt.Errorf("%s: instances: the list is empty. Fix: list the instance names", path)
	}
	if err := CheckAPIURL(c.APIURL, "https://api.telegram.org"); err != nil {
		return nil, fmt.Errorf("%s: api_url: %w", path, err)
	}
	for _, n := range c.Instances {
		if !ValidName(n) {
			return nil, fmt.Errorf("%s: instances: %q is not an instance name", path, n)
		}
	}
	if !userRE.MatchString(c.Updates.ListenerUser) {
		return nil, fmt.Errorf("%s: updates.listener_user: %q is not a user name. Fix: give the user of the listener, such as \"pullgate\"", path, c.Updates.ListenerUser)
	}
	if g := c.Updates.GH; g != "" && (!filepath.IsAbs(g) || filepath.Clean(g) != g || strings.HasPrefix(g, "-")) {
		return nil, fmt.Errorf("%s: updates.gh: %q is not a clean absolute path. Fix: give the path of the gh program, such as \"/usr/bin/gh\", or remove the key", path, g)
	}
	return c, nil
}

// CheckAPIURL checks the URL of an API: https:// with a host, or
// http://127.0.0.1:PORT for tests. No user info, which could hide another
// host (http://127.0.0.1:80@host.example). def is the URL that the
// errors name as the fix.
func CheckAPIURL(s, def string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		// Not quoted: the text may hold user info with a password.
		return fmt.Errorf("the url cannot be read. Fix: use %s", def)
	case u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "":
		// Not quoted: user info may hold a password.
		return fmt.Errorf("the url has user info, a query or a fragment. Fix: use %s", def)
	case u.Scheme == "https" && u.Hostname() != "":
		return nil
	case u.Scheme == "http" && u.Hostname() == "127.0.0.1" && u.Port() != "":
		return nil
	}
	return fmt.Errorf("%q: it must be an https:// url. Fix: use %s", s, def)
}
