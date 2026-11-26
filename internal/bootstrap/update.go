package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/recipe"
)

// Update is "pullgate recipe update" and "pullgate recipe use"
// (DESIGN.md section 21): an instance that was set up from a recipe moves
// to a newer version of it, forward only.
type Update struct {
	EtcDir    string
	Name      string
	Version   string // --version X.Y.Z (update), or the version of use
	URL       string // --url URL (update): instead of --version
	Reinstall bool   // update: the same version again
	Use       bool   // use: a version that recipe fetch unpacked; no download
	Fetcher   *recipe.Fetcher
	Out       io.Writer
	Now       func() time.Time
}

// UpdateUsage is the help text of the two commands.
const UpdateUsage = `pullgate recipe update NAME (--version X.Y.Z | --url URL) [--reinstall] [--gh PATH] [--dir DIR]
pullgate recipe use NAME X.Y.Z [--dir DIR]`

// Run moves the instance to the new version: the same checks as the
// first install, NAME.toml written again from the new template with the
// same local values (and a backup), and a new record. The folder of the
// version before is kept; older folders that no record names are removed.
func (u *Update) Run(ctx context.Context) error {
	if !config.ValidName(u.Name) {
		return fmt.Errorf("%q is not an instance name", u.Name)
	}
	rp := recipe.RecordPath(u.EtcDir, u.Name)
	rec, err := recipe.ReadInstanceRecord(rp)
	if err != nil {
		return fmt.Errorf("instance %s has no good recipe record (%w). Fix: set it up with pullgate bootstrap --recipe URL first", u.Name, err)
	}
	cur, err := recipe.ParseURL(rec.URL)
	if err != nil || cur.Slug() != rec.Repo {
		return fmt.Errorf("%s: the url %q is not the recipe of the record. Fix: set the instance up again with pullgate bootstrap --recipe URL", rp, rec.URL)
	}
	newURL := u.URL
	if u.Use || newURL == "" {
		if u.URL != "" && u.Use {
			return errors.New("recipe use takes a version, not --url")
		}
		// The same release file name, with the new version in it.
		newURL = strings.Replace(cur.Base, "/v"+cur.Version, "/v"+u.Version, 1) + "/" + strings.ReplaceAll(cur.File, cur.Version, u.Version)
	}
	src, err := recipe.ParseURL(newURL)
	if err != nil {
		return err
	}
	if src.Slug() != rec.Repo {
		return fmt.Errorf("%s is a recipe of %s, but instance %s runs one of %s. Fix: to change the project, set it up again with pullgate bootstrap --recipe URL --replace-config", newURL, src.Slug(), u.Name, rec.Repo)
	}
	switch {
	case recipe.Older(src.Version, rec.Version):
		return fmt.Errorf("instance %s runs version %s of %s; %s is older. Updates go forward only. Nothing was written. Fix: give a newer version", u.Name, rec.Version, rec.Repo, src.Version)
	case src.Version == rec.Version && u.Use:
		say(u.Out, "Instance %s uses version %s of %s already. Nothing to do.", u.Name, rec.Version, rec.Repo)
		return nil
	case src.Version == rec.Version && !u.Reinstall:
		return fmt.Errorf("instance %s runs version %s of %s already. Nothing was written. Fix: give a newer version, or --reinstall to install the same one again", u.Name, rec.Version, rec.Repo)
	}
	c, err := config.Load(u.EtcDir, u.Name, u.Fetcher.UID)
	if err != nil {
		return fmt.Errorf("instance %s: %w", u.Name, err)
	}
	f := *u.Fetcher
	f.Bundle, f.Workflow = rec.Bundle, rec.SignerWorkflow
	o := &Options{
		EtcDir: u.EtcDir, Out: u.Out, Now: u.Now, Fetcher: &f, Host: rec.Host, Recipe: newURL,
		ReplaceConfig: true, keepMode: true,
		Instances: []Instance{{Name: u.Name, URL: c.Repo.URL, Branch: c.Repo.Branch, From: rec.Instance}},
	}
	if err := o.locals(); err != nil {
		return err
	}
	var rr *recipeRun
	if u.Use {
		rr, err = o.openRecipe(src)
	} else {
		rr, err = o.fetchRecipe(ctx)
	}
	if err != nil {
		return err
	}
	if _, err := o.recipeConfig(u.Name, rr); err != nil {
		return err
	}
	if err := o.writeRecord(u.Name, rr); err != nil {
		return err
	}
	say(u.Out, "Instance %s: now on version %s of %s (it counts from the next run). The folder of %s is kept.", u.Name, src.Version, rec.Repo, rec.Version)
	u.prune(src, rec.Version)
	return nil
}

func say(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }

// openRecipe is fetchRecipe for "recipe use": the version folder that
// recipe fetch unpacked, checked again, with no download.
func (o *Options) openRecipe(src recipe.Source) (*recipeRun, error) {
	rr := &recipeRun{src: src, dir: filepath.Join(o.Fetcher.Dir, src.Owner, src.Repo, src.Version), picks: map[string]*recipe.Instance{}, configs: map[string][]byte{}}
	if err := checkValue("@RECIPE_DIR@", rr.dir); err != nil {
		return nil, err
	}
	f := *o.Fetcher
	f.Check = o.checkTemplates(rr)
	res, err := f.Open(src.Owner, src.Repo, src.Version)
	if err != nil {
		return nil, err
	}
	rr.res, rr.src = res, res.Source
	return rr, nil
}

// prune removes the version folders of the recipe that are older than
// the version before, and that nothing names: no record (by its repo and
// version, or its folder), and no config of the config folder. When a
// record cannot be read, it removes nothing.
func (u *Update) prune(src recipe.Source, before string) {
	repoDir := filepath.Join(u.Fetcher.Dir, src.Owner, src.Repo)
	used := map[string]bool{}
	recs, _ := filepath.Glob(filepath.Join(u.EtcDir, "*.recipe"))
	for _, p := range recs {
		r, err := recipe.ReadInstanceRecord(p)
		if err != nil {
			say(u.Out, "Note: no old folder is removed, because %s cannot be read: %v", p, err)
			return
		}
		used[filepath.Clean(r.Dir)] = true
		if r.Repo == src.Slug() {
			used[filepath.Join(repoDir, r.Version)] = true
		}
	}
	var configs []string
	tomls, _ := filepath.Glob(filepath.Join(u.EtcDir, "*.toml"))
	for _, p := range tomls {
		b, err := os.ReadFile(p) //nolint:gosec // root's own config folder
		if err != nil {
			say(u.Out, "Note: no old folder is removed, because %s cannot be read: %v", p, err)
			return
		}
		configs = append(configs, string(b))
	}
	ents, err := os.ReadDir(repoDir)
	if err != nil {
		return
	}
	for _, e := range ents {
		v := e.Name()
		p := filepath.Join(repoDir, v)
		if !e.IsDir() || v == src.Version || v == before || used[p] || !recipe.ValidVersion(v) || !recipe.Older(v, before) {
			continue
		}
		named := false
		for _, c := range configs {
			named = named || strings.Contains(c, p+"/")
		}
		if named {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			say(u.Out, "Note: could not remove the old folder %s: %v", p, err)
			continue
		}
		say(u.Out, "Removed the old folder %s.", p)
	}
}
