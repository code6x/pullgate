package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/recipe"
	"github.com/code6x/pullgate/internal/recipe/recipetest"
	"github.com/code6x/pullgate/internal/testgit"
)

const recipeManifest = `format = 1
version = "1.2.3"

[[instance]]
name = "example"
config = "example.toml.in"
requires = ["sh"]
setup = ["setup.sh", "one"]
`

const recipeTemplate = `# The config of @NAME@ on @HOST@.
host = "@HOST@"

[repo]
url = "@REPO_URL@"
branch = "@BRANCH@"

[commands]
apply = ["@RECIPE_DIR@/apply.sh"]
`

// setupScript writes its arguments and its whole environment to a file
// next to the recipes.
const setupScript = `#!/bin/sh
{ echo "args=$*"; env | sort; } > "$PULLGATE_RECIPE_DIR/../../../../setup.out"
`

func recipeFiles() map[string]string {
	return map[string]string{
		recipe.ManifestName: recipeManifest,
		"example.toml.in":   recipeTemplate,
		"apply.sh":          "#!/bin/sh\necho applied\n",
		"setup.sh":          setupScript,
	}
}

// recipeFx is a bootstrap --recipe with a fake release server and a fake gh.
type recipeFx struct {
	*setupFx
	rel *recipetest.Release
	url string
}

func newRecipeFx(t *testing.T, files map[string]string, extra ...string) *recipeFx {
	t.Helper()
	f := &recipeFx{setupFx: newSetupFx(t), rel: recipetest.New(t)}
	f.url = f.rel.Publish("1.2.3", "example_1.2.3_recipe.tar.gz", recipetest.Tarball(t, files))
	args := append([]string{"--token-stdin", "--api-url", f.tg.URL, "--recipe", f.url, "--gh", f.rel.GH,
		"--instance", "config-repo", "--repo", f.r.Bare}, extra...)
	o, err := Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	o.EtcDir, o.ListenerGroup, o.Stdin, o.Out = f.o.EtcDir, f.o.ListenerGroup, f.o.Stdin, f.o.Out
	o.PairTTL, o.PollSeconds, o.RemoteWait, o.RemoteEvery, o.Setup = f.o.PairTTL, f.o.PollSeconds, f.o.RemoteWait, f.o.RemoteEvery, f.o.Setup
	o.Host = "example-host"
	o.Fetcher.Dir = filepath.Join(testgit.PrivateDir(t), "lib", "recipes")
	o.Fetcher.UID = uint32(os.Geteuid())
	o.Fetcher.Look = func(n string) (string, error) {
		if n == "sh" || n == "install" || n == "systemctl" {
			return "/bin/sh", nil
		}
		return "", errors.New("not found")
	}
	f.o = o
	return f
}

func (f *recipeFx) versionDir() string {
	return filepath.Join(f.o.Fetcher.Dir, recipetest.Owner, recipetest.Repo, "1.2.3")
}

// nothing checks that nothing was written: no config, no record, no
// pairing, no recipe folder, no user, no unit.
func (f *recipeFx) nothing() {
	f.t.Helper()
	f.nothingWritten()
	if _, err := os.Lstat(recipe.RecordPath(f.etc, "config-repo")); err == nil {
		f.t.Fatal("the record was written")
	}
	if _, err := os.Lstat(f.versionDir()); err == nil {
		f.t.Fatal("the recipe was unpacked")
	}
	if len(f.added) > 0 || len(f.calls) > 0 {
		f.t.Fatalf("users %v or systemctl %v", f.added, f.calls)
	}
}

func (f *recipeFx) config() map[string]any {
	f.t.Helper()
	var m map[string]any
	if _, err := toml.DecodeFile(f.o.instanceConfig("config-repo"), &m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func TestRecipeBootstrap(t *testing.T) {
	f := newRecipeFx(t, recipeFiles())
	if err := f.pair(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	out := f.out.String()
	for _, w := range []string{"WARNING: the release server", "Recipe: example-org/example-recipe v1.2.3", "SHA-256: ", "Signed by: https://github.com/example-org/example-recipe/.github/workflows/release.yml@refs/heads/main"} {
		if !strings.Contains(out, w) {
			t.Fatalf("output lacks %q:\n%s", w, out)
		}
	}
	dir := f.versionDir()
	c, err := config.Load(f.etc, "config-repo", uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != config.ModeAsk || c.Host != "example-host" || c.Repo.URL != f.r.Bare || c.Repo.Branch != "main" ||
		c.Commands.CheckUser != "pullgate-check-config-repo" || !slices.Equal(c.Commands.Apply, []string{dir + "/apply.sh"}) {
		t.Fatalf("config: %+v", c)
	}
	if !slices.Contains(f.added, "pullgate-check-config-repo") || !f.enabled() {
		t.Fatalf("users %v, enabled %v", f.added, f.enabled())
	}
	// The setup ran as root in the recipe folder, with only four variables.
	b, err := os.ReadFile(filepath.Join(f.o.Fetcher.Dir, "..", "setup.out"))
	if err != nil {
		t.Fatalf("the setup did not run: %v\n%s", err, out)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{"args=one", "LC_ALL=C", "PATH=" + recipe.SearchPath, "PULLGATE_INSTANCE=config-repo", "PULLGATE_RECIPE_DIR=" + dir, "PWD=" + dir}
	if !slices.Equal(got, want) {
		t.Fatalf("the setup saw:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	r, err := recipe.ReadInstanceRecord(recipe.RecordPath(f.etc, "config-repo"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Repo != "example-org/example-recipe" || r.Version != "1.2.3" || r.File != "example_1.2.3_recipe.tar.gz" || r.Dir != dir || r.Instance != "example" || r.Host != "example-host" || len(r.SHA256) != 64 {
		t.Fatalf("record: %+v", r)
	}
}

func TestRecipeDeny(t *testing.T) {
	tpl := func(s string) map[string]string {
		fs := recipeFiles()
		fs["example.toml.in"] = s
		return fs
	}
	for _, c := range []struct {
		name  string
		files map[string]string
		extra []string
		host  string
		want  string
	}{
		{"mode", tpl("mode = \"auto\"\n" + recipeTemplate), nil, "", "sets mode"},
		{"deploy key", tpl(strings.Replace(recipeTemplate, "[repo]\n", "[repo]\ndeploy_key = \"/tmp/k\"\n", 1)), nil, "", "deploy_key or known_hosts"},
		{"known hosts", tpl(strings.Replace(recipeTemplate, "[repo]\n", "[repo]\nknown_hosts = \"/tmp/k\"\n", 1)), nil, "", "deploy_key or known_hosts"},
		{"check user", tpl(recipeTemplate + "check_user = \"root\"\n"), nil, "", "check_user"},
		{"listener", tpl(recipeTemplate + "[listener]\nuser = \"root\"\n"), nil, "", "[listener]"},
		{"Mode", tpl("Mode = \"auto\"\n" + recipeTemplate), nil, "", "lower case"},
		{"Repo URL", tpl(recipeTemplate + "[Repo]\nURL = \"/srv/other.git\"\n"), nil, "", "lower case"},
		{"Deploy_Key", tpl(strings.Replace(recipeTemplate, "[repo]\n", "[repo]\nDeploy_Key = \"/tmp/k\"\n", 1)), nil, "", "lower case"},
		{"Check_User", tpl(recipeTemplate + "Check_User = \"pullgate\"\n"), nil, "", "lower case"},
		{"Listener", tpl(recipeTemplate + "[Listener]\nuser = \"root\"\n"), nil, "", "lower case"},
		{"Kelvin sign", tpl(recipeTemplate + "\"chec\u212a_user\" = \"pullgate\"\n"), nil, "", "lower case"},
		{"another repo", tpl(strings.Replace(recipeTemplate, "@REPO_URL@", "/srv/other.git", 1)), nil, "", "url or branch"},
		{"unknown placeholder", tpl(recipeTemplate + "# @SECRET@\n"), nil, "", "the placeholder @SECRET@"},
		{"unknown key", tpl(recipeTemplate + "[nothing]\nx = 1\n"), nil, "", "unknown key"},
		{"no apply", tpl("host = \"@HOST@\"\n"), nil, "", "apply"},
		{"host with a quote", recipeFiles(), nil, "example\"host", "a quote"},
		{"host with a newline", recipeFiles(), nil, "example\nmode = \"auto\"", "a newline"},
		{"apply with a recipe", recipeFiles(), []string{"--apply", "/bin/true"}, "", "leave --apply out"},
		{"no such recipe instance", recipeFiles(), []string{"--recipe-instance", "other"}, "", "has no instance \"other\""},
		{"missing program", func() map[string]string {
			fs := recipeFiles()
			fs[recipe.ManifestName] = strings.Replace(recipeManifest, `["sh"]`, `["sh", "ansible-playbook"]`, 1)
			return fs
		}(), nil, "", "ansible-playbook"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRecipeFx(t, c.files, c.extra...)
			if c.host != "" {
				f.o.Host = c.host
			}
			err := Run(context.Background(), f.o)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			f.nothing()
		})
	}
}

func TestRecipeURLDeny(t *testing.T) {
	for _, u := range []string{
		"https://example.com/example-org/example-recipe/releases/download/v1.2.3/x.tar.gz",
		"https://github.com/example-org/example-recipe/releases/latest/download/x.tar.gz",
		"http://github.com/example-org/example-recipe/releases/download/v1.2.3/x.tar.gz",
	} {
		f := newRecipeFx(t, recipeFiles())
		f.o.Recipe = u
		if err := Run(context.Background(), f.o); err == nil || !strings.Contains(err.Error(), "not a file of a GitHub release") {
			t.Fatalf("%s: err = %v", u, err)
		}
		f.nothing()
	}
}

func TestRecipeOptionsNeedRecipe(t *testing.T) {
	for _, args := range [][]string{
		{"--replace-config", "--instance", "x", "--repo", "/srv/x.git", "--apply", "/bin/true"},
		{"--gh", "/usr/local/bin/gh", "--instance", "x", "--repo", "/srv/x.git", "--apply", "/bin/true"},
		{"--instance", "x", "--recipe-instance", "y", "--repo", "/srv/x.git", "--apply", "/bin/true"},
	} {
		o, err := Parse(args)
		if err != nil {
			t.Fatal(err)
		}
		o.EtcDir = testgit.PrivateDir(t)
		if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "--recipe") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

func TestRecipeFailedSetupLeavesTheInstanceOff(t *testing.T) {
	fs := recipeFiles()
	fs["setup.sh"] = "#!/bin/sh\necho the setup failed here\nexit 3\n"
	f := newRecipeFx(t, fs)
	err := f.pair()
	if err == nil || !strings.Contains(err.Error(), "the instance is off") || !strings.Contains(f.out.String(), "the setup failed here") {
		t.Fatalf("err = %v\n%s", err, f.out.String())
	}
	if f.enabled() {
		t.Fatal("units were enabled")
	}
	if _, err := os.Lstat(recipe.RecordPath(f.etc, "config-repo")); err == nil {
		t.Fatal("the record was written")
	}
	if _, err := os.Lstat(f.o.instanceConfig("config-repo")); err == nil {
		t.Fatal("the config was written")
	}
}

func TestRecipeInstancePick(t *testing.T) {
	fs := recipeFiles()
	fs[recipe.ManifestName] = recipeManifest + "\n[[instance]]\nname = \"second\"\nconfig = \"example.toml.in\"\n"
	f := newRecipeFx(t, fs)
	// Two instances: a name that is in neither needs --recipe-instance.
	if err := Run(context.Background(), f.o); err == nil || !strings.Contains(err.Error(), "--recipe-instance") {
		t.Fatalf("err = %v", err)
	}
	f.nothing()
	f = newRecipeFx(t, fs, "--recipe-instance", "second")
	if err := f.pair(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	r, err := recipe.ReadInstanceRecord(recipe.RecordPath(f.etc, "config-repo"))
	if err != nil || r.Instance != "second" {
		t.Fatalf("record %+v, %v", r, err)
	}
	if _, err := os.Lstat(filepath.Join(f.o.Fetcher.Dir, "..", "setup.out")); err == nil {
		t.Fatal("the setup of the other instance ran")
	}
}

// keptConfig is a NAME.toml that is there before the bootstrap.
func (f *recipeFx) keptConfig(repo string) string {
	f.t.Helper()
	text := "mode = \"auto\"\nhost = \"old-host\"\n\n[repo]\nurl = \"" + repo + "\"\nbranch = \"main\"\n\n[commands]\napply = [\"/usr/local/lib/old/apply.sh\"]\ncheck_user = \"pullgate-check-old\"\n"
	if err := os.WriteFile(f.o.instanceConfig("config-repo"), []byte(text), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return text
}

func TestRecipeKeepsAConfigAndShowsTheDiff(t *testing.T) {
	f := newRecipeFx(t, recipeFiles())
	old := f.keptConfig(f.r.Bare)
	if err := f.pair(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	b, _ := os.ReadFile(f.o.instanceConfig("config-repo"))
	if string(b) != old {
		t.Fatalf("the config changed:\n%s", b)
	}
	out := f.out.String()
	for _, w := range []string{"  - mode = \"auto\"", "  + mode = \"ask\"", "  + host = \"example-host\"", "is kept. To write the recipe's config, run bootstrap again with --replace-config"} {
		if !strings.Contains(out, w) {
			t.Fatalf("output lacks %q:\n%s", w, out)
		}
	}
	if !slices.Contains(f.added, "pullgate-check-old") {
		t.Fatalf("users: %v", f.added)
	}
}

func TestRecipeReplaceConfigKeepsABackupAndTheRepo(t *testing.T) {
	f := newRecipeFx(t, recipeFiles(), "--replace-config")
	old := f.keptConfig(f.r.Bare)
	if err := f.pair(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	m := f.config()
	repo, _ := m["repo"].(map[string]any)
	cmds, _ := m["commands"].(map[string]any)
	if m["mode"] != "ask" || m["host"] != "example-host" || repo["url"] != f.r.Bare || cmds["check_user"] != "pullgate-check-old" {
		t.Fatalf("config: %v", m)
	}
	baks, _ := filepath.Glob(f.o.instanceConfig("config-repo") + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("backups: %v", baks)
	}
	if b, _ := os.ReadFile(baks[0]); string(b) != old {
		t.Fatalf("backup:\n%s", b)
	}
}

func TestRecipeKeptConfigWithAnotherRepoIsRefused(t *testing.T) {
	f := newRecipeFx(t, recipeFiles(), "--replace-config")
	old := f.keptConfig("/srv/other.git")
	if err := Run(context.Background(), f.o); err == nil || !strings.Contains(err.Error(), "A kept config keeps its repo") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(f.o.instanceConfig("config-repo")); string(b) != old {
		t.Fatal("the config changed")
	}
}

// paired writes a listener.toml and a bot token, as an earlier bootstrap
// did, with another instance and keys that bootstrap does not write.
func (f *fx) paired() {
	f.t.Helper()
	text := "owner_id = 4242\nchat_id = 4343\ninstances = [\"other\"]\napi_url = \"" + f.tg.URL + "\"\n\n[updates]\ngh = \"/usr/local/bin/gh\"\n"
	if err := os.WriteFile(filepath.Join(f.etc, "listener.toml"), []byte(text), 0o640); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.etc, "bot-token"), []byte(token+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func TestAddKeepsTheIDsAndTheOtherInstances(t *testing.T) {
	f := newRecipeFx(t, recipeFiles(), "--add")
	f.o.TokenStdin, f.o.APIURL = false, DefaultAPI
	f.o.Stdin = strings.NewReader("") // no token is read
	f.paired()
	if err := Run(context.Background(), f.o); err != nil {
		t.Fatalf("bootstrap --add: %v\n%s", err, f.out.String())
	}
	l, err := config.LoadListenerForUpdate(filepath.Join(f.etc, "listener.toml"), uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	if l.OwnerID != 4242 || l.ChatID != 4343 || !slices.Equal(l.Instances, []string{"other", "config-repo"}) || l.APIURL != f.tg.URL || l.Updates.GH != "/usr/local/bin/gh" {
		t.Fatalf("listener.toml: %+v", l)
	}
	if b, _ := os.ReadFile(filepath.Join(f.etc, "bot-token")); string(b) != token+"\n" {
		t.Fatal("the token changed")
	}
	if !strings.Contains(f.out.String(), "Kept from "+filepath.Join(f.etc, "listener.toml")+": owner_id 4242, chat_id 4343, the instances other") || strings.Contains(f.out.String(), "Pairing") {
		t.Fatalf("output:\n%s", f.out.String())
	}
	if _, err := os.Lstat(f.o.instanceConfig("config-repo")); err != nil || !f.enabled() {
		t.Fatalf("config %v, enabled %v", err, f.enabled())
	}
}

func TestAddRefusesABadListener(t *testing.T) {
	for name, edit := range map[string]func(f *recipeFx){
		"none":             func(*recipeFx) {},
		"others can write": func(f *recipeFx) { f.paired(); _ = os.Chmod(filepath.Join(f.etc, "listener.toml"), 0o666) },
		"no owner": func(f *recipeFx) {
			f.paired()
			_ = os.WriteFile(filepath.Join(f.etc, "listener.toml"), []byte("chat_id = 1\ninstances = [\"other\"]\n"), 0o640)
		},
		"unknown key": func(f *recipeFx) {
			f.paired()
			_ = os.WriteFile(filepath.Join(f.etc, "listener.toml"), []byte("owner_id = 1\nchat_id = 1\ninstances = [\"other\"]\nowner = 2\n"), 0o640)
		},
		"no token": func(f *recipeFx) { f.paired(); _ = os.Remove(filepath.Join(f.etc, "bot-token")) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newRecipeFx(t, recipeFiles(), "--add")
			f.o.TokenStdin, f.o.APIURL = false, DefaultAPI
			edit(f)
			before, _ := os.ReadFile(filepath.Join(f.etc, "listener.toml"))
			if err := Run(context.Background(), f.o); err == nil || !strings.Contains(err.Error(), "--add") {
				t.Fatalf("err = %v", err)
			}
			after, _ := os.ReadFile(filepath.Join(f.etc, "listener.toml"))
			if string(before) != string(after) {
				t.Fatal("listener.toml changed")
			}
			if _, err := os.Lstat(f.o.instanceConfig("config-repo")); err == nil {
				t.Fatal("a config was written")
			}
		})
	}
}

func TestAddTakesNoToken(t *testing.T) {
	f := newRecipeFx(t, recipeFiles(), "--add")
	f.paired()
	if err := Run(context.Background(), f.o); err == nil || !strings.Contains(err.Error(), "no --token-stdin") {
		t.Fatalf("err = %v", err)
	}
}

// TestContribExampleIsARecipe builds a tarball from
// contrib/examples/config-file and bootstraps from it.
func TestContribExampleIsARecipe(t *testing.T) {
	dir := filepath.Join("..", "..", "contrib", "examples", "config-file")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(b)
	}
	var m recipe.Manifest
	if _, err := toml.Decode(files[recipe.ManifestName], &m); err != nil {
		t.Fatal(err)
	}
	f := newRecipeFx(t, files)
	f.url = f.rel.Publish(m.Version, "config-repo_"+m.Version+"_recipe.tar.gz", recipetest.Tarball(t, files))
	f.o.Recipe = f.url
	if err := f.pair(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	c, err := config.Load(f.etc, "config-repo", uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	vd := filepath.Join(f.o.Fetcher.Dir, recipetest.Owner, recipetest.Repo, m.Version)
	if c.Mode != config.ModeAsk || c.OnFailure != config.OnFailureRollback || !slices.Equal(c.Commands.Apply, []string{vd + "/apply.sh"}) {
		t.Fatalf("config: %+v", c)
	}
	if err := c.CheckGateFiles(uint32(os.Geteuid())); err != nil {
		t.Fatalf("the programs of the recipe: %v", err)
	}
}

func TestDiff(t *testing.T) {
	got := Diff("a\nb\nc\n", "a\nx\nc\nd\n")
	want := "    a\n  - b\n  + x\n    c\n  + d"
	if got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
}
