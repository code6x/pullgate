package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/gitx"
	"github.com/code6x/pullgate/internal/recipe"
	"github.com/code6x/pullgate/internal/state"
)

// recipeRun is the recipe of a "bootstrap --recipe": the checked source,
// the instance of the manifest for each --instance, and its config, made
// from the template before anything is written.
type recipeRun struct {
	src     recipe.Source
	res     *recipe.Result
	dir     string                      // the version folder
	picks   map[string]*recipe.Instance // by --instance name
	configs map[string][]byte           // the config from the template, by --instance name
}

// local is what an instance's config is made with: from the command
// line, or from the config that is there already.
type local struct {
	url, branch, key, knownHosts, checkUser string
	kept                                    *config.Instance // the NAME.toml that is there, or nil
}

// SetupTimeout is the most a recipe's setup may run.
const SetupTimeout = 30 * time.Minute

// Placeholders are the only placeholders of a template.
var Placeholders = []string{"@NAME@", "@HOST@", "@REPO_URL@", "@BRANCH@", "@RECIPE_DIR@"}

var placeholderRE = regexp.MustCompile(`@[A-Z][A-Z0-9_]*@`)

// checkValue refuses a placeholder value that could break out of a TOML
// string: a quote, a backslash, a newline or another control character.
func checkValue(key, v string) error {
	for _, r := range v {
		if r == '"' || r == '\'' || r == '\\' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("the value of %s (%q) holds a quote, a backslash, a newline or a control character, which pullgate does not put in a config. Nothing was written. Fix: give a value without them", key, v)
		}
	}
	if m := placeholderRE.FindString(v); m != "" {
		return fmt.Errorf("the value of %s (%q) holds %s, which looks like a placeholder. Nothing was written. Fix: give a value without it", key, v, m)
	}
	return nil
}

// host is the value of @HOST@.
func (o *Options) host() string {
	if o.Host != "" {
		return o.Host
	}
	h, _ := os.Hostname()
	return h
}

// locals finds what each instance's config is made with. A config that
// is there already must load, and give the same repo.
func (o *Options) locals() error {
	o.local = map[string]*local{}
	for _, in := range o.Instances {
		l := &local{url: in.URL, branch: in.Branch, checkUser: "pullgate-check-" + in.Name}
		if p, _ := gitx.LocalPath(in.URL); p == "" {
			l.key, l.knownHosts = filepath.Join(o.EtcDir, in.Name+".key"), filepath.Join(o.EtcDir, in.Name+".known_hosts")
		}
		if _, err := os.Lstat(o.instanceConfig(in.Name)); err == nil {
			c, err := config.Load(o.EtcDir, in.Name, uint32(os.Geteuid())) //nolint:gosec // a uid fits in uint32
			if err != nil {
				return fmt.Errorf("--instance %s: %w. Fix: correct it, or move it away so bootstrap writes a new one", in.Name, err)
			}
			if c.Repo.URL != in.URL || (in.branchSet && c.Repo.Branch != in.Branch) {
				return fmt.Errorf("--instance %s: %s has the repo %s (branch %s), not the --repo and --branch of this run. A kept config keeps its repo. Fix: give the same --repo and --branch, or move %s away", in.Name, c.Path, c.Repo.URL, c.Repo.Branch, c.Path)
			}
			l.url, l.branch, l.key, l.knownHosts, l.checkUser, l.kept = c.Repo.URL, c.Repo.Branch, c.Repo.DeployKey, c.Repo.KnownHosts, c.Commands.CheckUser, c
		}
		for k, v := range map[string]string{"@NAME@": in.Name, "@HOST@": o.host(), "@REPO_URL@": l.url, "@BRANCH@": l.branch} {
			if err := checkValue(k, v); err != nil {
				return fmt.Errorf("--instance %s: %w", in.Name, err)
			}
		}
		o.local[in.Name] = l
	}
	return nil
}

// pick is the instance of the manifest for --instance in: the one of
// --recipe-instance, else the one with the same name, else the only one.
func pick(m *recipe.Manifest, in Instance) (*recipe.Instance, error) {
	var names []string
	for i := range m.Instances {
		names = append(names, m.Instances[i].Name)
	}
	want := in.From
	if want == "" {
		want = in.Name
	}
	if i := slices.Index(names, want); i >= 0 {
		return &m.Instances[i], nil
	}
	if in.From == "" && len(names) == 1 {
		return &m.Instances[0], nil
	}
	return nil, fmt.Errorf("--instance %s: the recipe has no instance %q; it has %s. Nothing was written. Fix: give --recipe-instance NAME after --instance %s", in.Name, want, strings.Join(names, ", "), in.Name)
}

// fetchRecipe downloads, checks and unpacks the recipe, and makes the
// config of each instance from its template. A bad template fails inside
// the fetch, so it writes nothing.
func (o *Options) fetchRecipe(ctx context.Context) (*recipeRun, error) {
	src, err := recipe.ParseURL(o.Recipe)
	if err != nil {
		return nil, err
	}
	rr := &recipeRun{src: src, dir: filepath.Join(o.Fetcher.Dir, src.Owner, src.Repo, src.Version), picks: map[string]*recipe.Instance{}, configs: map[string][]byte{}}
	if err := checkValue("@RECIPE_DIR@", rr.dir); err != nil {
		return nil, err
	}
	f := *o.Fetcher
	f.Out = o.Out
	f.Check = func(dir string, m *recipe.Manifest) error {
		for _, in := range o.Instances {
			ri, err := pick(m, in)
			if err != nil {
				return err
			}
			tpl, err := os.ReadFile(filepath.Join(dir, ri.Config)) //nolint:gosec // a checked path in the recipe folder
			if err != nil {
				return err
			}
			b, err := o.render(in.Name, o.local[in.Name], tpl, rr, ri)
			if err != nil {
				return err
			}
			rr.picks[in.Name], rr.configs[in.Name] = ri, b
		}
		return nil
	}
	res, err := f.Fetch(ctx, o.Recipe)
	if err != nil {
		return nil, err
	}
	rr.res = res
	return rr, nil
}

// render makes NAME.toml from a template: it fills the placeholders,
// refuses the keys that belong to pullgate, sets them itself, and checks
// the result like any instance config.
func (o *Options) render(name string, l *local, tpl []byte, rr *recipeRun, ri *recipe.Instance) ([]byte, error) {
	where := fmt.Sprintf("the template %s of the recipe instance %s", ri.Config, ri.Name)
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%s: %s. Nothing was written. Fix: do not use this recipe; report it to its project", where, fmt.Sprintf(format, a...))
	}
	// One pass: a value that holds a placeholder is not filled again.
	text := strings.NewReplacer("@NAME@", name, "@HOST@", o.host(), "@REPO_URL@", l.url, "@BRANCH@", l.branch, "@RECIPE_DIR@", rr.dir).Replace(string(tpl))
	if m := placeholderRE.FindString(text); m != "" {
		return nil, bad("the placeholder %s is not one of %s", m, strings.Join(Placeholders, ", "))
	}
	var t map[string]any
	md, err := toml.Decode(text, &t)
	if err != nil {
		return nil, bad("%v", err)
	}
	// "Mode" would set mode: the checks below must see every key as the
	// config reads it.
	if err := config.ExactKeys(md); err != nil {
		return nil, bad("%v", err)
	}
	if t == nil {
		t = map[string]any{}
	}
	table := func(k string) (map[string]any, error) {
		v, ok := t[k]
		if !ok {
			m := map[string]any{}
			t[k] = m
			return m, nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, bad("%s is not a table", k)
		}
		return m, nil
	}
	repo, err := table("repo")
	if err != nil {
		return nil, err
	}
	cmds, err := table("commands")
	if err != nil {
		return nil, err
	}
	// A recipe never picks the mode, a key, the host keys, the user that
	// runs the checks, or the listener user.
	switch {
	case t["mode"] != nil:
		return nil, bad("it sets mode; pullgate always writes mode = \"ask\"")
	case repo["deploy_key"] != nil, repo["known_hosts"] != nil:
		return nil, bad("it sets [repo] deploy_key or known_hosts; pullgate writes them")
	case cmds["check_user"] != nil:
		return nil, bad("it sets [commands] check_user; pullgate writes it")
	case t["listener"] != nil:
		return nil, bad("it sets [listener]; the listener user is pullgate's")
	case repo["url"] != nil && repo["url"] != l.url, repo["branch"] != nil && repo["branch"] != l.branch:
		return nil, bad("it sets [repo] url or branch to another value than @REPO_URL@ and @BRANCH@")
	}
	t["mode"] = config.ModeAsk
	repo["url"], repo["branch"] = l.url, l.branch
	if l.key != "" {
		repo["deploy_key"] = l.key
	}
	if l.knownHosts != "" {
		repo["known_hosts"] = l.knownHosts
	}
	cmds["check_user"] = l.checkUser
	var b bytes.Buffer
	// No date: the same recipe makes the same file, so a run again shows
	// no diff.
	fmt.Fprintf(&b, "# Written by pullgate bootstrap from the recipe %s %s (%s, instance %s).\n# pullgate owns mode, [repo] and check_user: no other config tool may write them.\n",
		rr.src.Slug(), rr.src.Tag(), rr.src.File, ri.Name)
	if err := toml.NewEncoder(&b).Encode(t); err != nil {
		return nil, bad("%v", err)
	}
	if _, err := config.Parse(o.instanceConfig(name), name, b.Bytes()); err != nil {
		return nil, bad("%v", err)
	}
	return b.Bytes(), nil
}

// recipeConfig writes NAME.toml from the template. A config that is there
// already is kept, and the diff is shown; with --replace-config it is
// replaced, and a backup kept. It says whether NAME.toml is now the
// recipe's config.
func (o *Options) recipeConfig(name string, rr *recipeRun) (bool, error) {
	p := o.instanceConfig(name)
	b := rr.configs[name]
	old, err := os.ReadFile(p) //nolint:gosec // root's own config folder
	if err == nil {
		if bytes.Equal(old, b) {
			o.say("Instance %s: %s is the same as the recipe's config.", name, p)
			return true, nil
		}
		o.say("Instance %s: %s exists. The recipe's config differs from it (- now, + recipe):\n%s", name, p, Diff(string(old), string(b)))
		if !o.ReplaceConfig {
			o.say("Instance %s: %s is kept, and no recipe record is written: the instance does not run the recipe's config. To write it, run bootstrap again with --replace-config.", name, p)
			// An old record would name a recipe that this config may not be.
			if err := os.Remove(recipe.RecordPath(o.EtcDir, name)); err == nil {
				o.say("Instance %s: removed the old %s.", name, recipe.RecordPath(o.EtcDir, name))
			} else if !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
			return false, nil
		}
		bak := p + ".bak-" + o.Now().Format("20060102-150405")
		if err := state.WriteFileAtomic(bak, old, 0o600); err != nil {
			return false, err
		}
		o.say("Instance %s: the old config is kept as %s.", name, bak)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := state.WriteFileAtomic(p, b, 0o600); err != nil {
		return false, err
	}
	o.say("Instance %s: wrote %s from the recipe (mode = \"ask\").", name, p)
	return true, nil
}

// runSetup runs the recipe's setup of an instance, as root, in the
// recipe folder, with only PATH, LC_ALL, PULLGATE_INSTANCE and
// PULLGATE_RECIPE_DIR.
func (o *Options) runSetup(ctx context.Context, name string, rr *recipeRun) error {
	ri := rr.picks[name]
	if len(ri.Setup) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, SetupTimeout)
	defer cancel()
	o.say("Instance %s: running the setup of the recipe: %s", name, strings.Join(ri.Setup, " "))
	cmd := exec.CommandContext(ctx, filepath.Join(rr.dir, ri.Setup[0]), ri.Setup[1:]...) //nolint:gosec // a checked program of the checked recipe
	cmd.Env = []string{"PATH=" + recipe.SearchPath, "LC_ALL=C", "PULLGATE_INSTANCE=" + name, "PULLGATE_RECIPE_DIR=" + rr.dir}
	cmd.Dir = rr.dir
	cmd.Stdout, cmd.Stderr = o.Out, o.Out
	// A child it leaves behind is killed with it at the timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("instance %s: the setup of the recipe failed: %w. No config was written and no unit was enabled: the instance is off, or runs on with the config it had. Fix: read its output above, fix the cause, then run pullgate bootstrap again", name, err)
	}
	return nil
}

// writeRecord writes NAME.recipe: what pullgate status shows.
func (o *Options) writeRecord(name string, rr *recipeRun) error {
	r := recipe.InstanceRecord{
		URL: rr.src.URL, Repo: rr.src.Slug(), Version: rr.src.Version, File: rr.src.File, SHA256: rr.res.SHA256,
		Dir: rr.dir, Instance: rr.picks[name].Name, Bundle: o.Fetcher.Bundle, SignerWorkflow: o.Fetcher.Workflow,
		Host: o.host(), Installed: o.Now().UTC().Format(time.RFC3339),
	}
	return state.WriteFileAtomic(recipe.RecordPath(o.EtcDir, name), r.Encode(), 0o644)
}

// MaxDiffLines caps the diff: configs are short.
const MaxDiffLines = 2000

// Diff is a line diff of a and b: "- " a line of a only, "+ " of b only,
// "  " of both.
func Diff(a, b string) string {
	x, y := strings.Split(strings.TrimSuffix(a, "\n"), "\n"), strings.Split(strings.TrimSuffix(b, "\n"), "\n")
	if len(x) > MaxDiffLines || len(y) > MaxDiffLines {
		return "    (too long to show)"
	}
	// lcs[i][j] is the longest common part of x[i:] and y[j:].
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			out = append(out, "    "+x[i])
			i++
			j++
		case i < len(x) && (j == len(y) || lcs[i+1][j] >= lcs[i][j+1]):
			out = append(out, "  - "+x[i])
			i++
		default:
			out = append(out, "  + "+y[j])
			j++
		}
	}
	return strings.Join(out, "\n")
}
