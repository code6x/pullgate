package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/recipe"
	"github.com/code6x/pullgate/internal/recipe/recipetest"
)

// version is the recipe at version v: its template watches a file named
// after the version, so each version makes another config.
func version(v string) map[string]string {
	fs := recipeFiles()
	fs[recipe.ManifestName] = strings.Replace(recipeManifest, `"1.2.3"`, `"`+v+`"`, 1)
	fs["example.toml.in"] = recipeTemplate + "\n[watch]\npaths = [\"v" + v + "\"]\n"
	return fs
}

// updateFx is an instance set up from version 1.2.3, and the versions
// 1.2.2 and 1.2.4 to 1.2.6 published in the same release server.
type updateFx struct {
	*recipeFx
}

func newUpdateFx(t *testing.T) *updateFx {
	t.Helper()
	f := &updateFx{newRecipeFx(t, version("1.2.3"))}
	if err := f.pair(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, f.out.String())
	}
	for _, v := range []string{"1.2.2", "1.2.4", "1.2.5", "1.2.6"} {
		f.rel.Publish(v, "example_"+v+"_recipe.tar.gz", recipetest.Tarball(t, version(v)))
	}
	return f
}

func (f *updateFx) update(v string, use, reinstall bool) error {
	u := &Update{EtcDir: f.etc, Name: "config-repo", Version: v, Use: use, Reinstall: reinstall, Fetcher: f.o.Fetcher, Out: f.out, Now: func() time.Time { return time.Now().Add(time.Second) }}
	return u.Run(context.Background())
}

func (f *updateFx) record() *recipe.InstanceRecord {
	f.t.Helper()
	r, err := recipe.ReadInstanceRecord(recipe.RecordPath(f.etc, "config-repo"))
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *updateFx) dir(v string) string {
	return filepath.Join(f.o.Fetcher.Dir, recipetest.Owner, recipetest.Repo, v)
}

func (f *updateFx) text() string {
	b, _ := os.ReadFile(f.o.instanceConfig("config-repo"))
	return string(b)
}

func TestUpdateForwardOnly(t *testing.T) {
	f := newUpdateFx(t)
	before := f.text()
	// Older, or the same without --reinstall: refused, nothing written.
	for _, c := range []struct {
		v    string
		want string
	}{{"1.2.2", "older"}, {"1.2.3", "--reinstall"}} {
		if err := f.update(c.v, false, false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v", c.v, err)
		}
		if f.text() != before || f.record().Version != "1.2.3" {
			t.Fatalf("%s: the config or the record changed", c.v)
		}
		if _, err := os.Lstat(f.dir("1.2.2")); err == nil {
			t.Fatal("the older version was unpacked")
		}
	}
	if err := f.update("1.2.3", false, true); err != nil {
		t.Fatalf("--reinstall: %v", err)
	}
	// Bootstrap with an older recipe is refused too.
	g := newRecipeFx(t, version("1.2.2"))
	g.o.EtcDir, g.o.Fetcher.Dir = f.etc, f.o.Fetcher.Dir
	g.o.Recipe = strings.Replace(f.url, "1.2.3", "1.2.2", 2)
	g.o.Fetcher.GH = f.rel.GH
	g.o.Instances[0].URL = f.r.Bare
	g.o.Setup.StateDir, g.o.Setup.SpoolDir = f.o.Setup.StateDir, f.o.Setup.SpoolDir
	if err := Run(context.Background(), g.o); err == nil || !strings.Contains(err.Error(), "forward only") {
		t.Fatalf("bootstrap older: err = %v", err)
	}
}

func TestUpdateWritesTheConfigAgainWithTheSameValues(t *testing.T) {
	f := newUpdateFx(t)
	before := f.text()
	// The owner set the mode: an update keeps it.
	if err := os.WriteFile(f.o.instanceConfig("config-repo"), []byte(strings.Replace(before, `mode = "ask"`, `mode = "off"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.update("1.2.4", false, false); err != nil {
		t.Fatalf("update: %v\n%s", err, f.out.String())
	}
	want := strings.ReplaceAll(strings.Replace(before, `mode = "ask"`, `mode = "off"`, 1), "1.2.3", "1.2.4")
	if got := f.text(); got != want {
		t.Fatalf("config:\n%s\nwant:\n%s", got, want)
	}
	baks, _ := filepath.Glob(f.o.instanceConfig("config-repo") + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("backups: %v", baks)
	}
	r := f.record()
	if r.Version != "1.2.4" || r.Dir != f.dir("1.2.4") || r.Host != "example-host" || r.Instance != "example" {
		t.Fatalf("record: %+v", r)
	}
	// The folder of the version before is kept until the next update.
	if _, err := os.Lstat(f.dir("1.2.3")); err != nil {
		t.Fatal("the folder of 1.2.3 was removed")
	}
	if err := f.update("1.2.5", false, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := os.Lstat(f.dir("1.2.3")); err == nil {
		t.Fatal("the folder of 1.2.3 is still there after two updates")
	}
	if _, err := os.Lstat(f.dir("1.2.4")); err != nil {
		t.Fatal("the folder of 1.2.4 was removed")
	}
}

func TestUpdateHasTheChecksOfTheFirstInstall(t *testing.T) {
	f := newUpdateFx(t)
	before := f.text()
	// A wrong SHA-256 in the release of 1.2.4.
	f.rel.Publish("1.2.4", "example_1.2.4_recipe.tar.gz", recipetest.Tarball(t, version("1.2.4")))
	f.rel.Replace("1.2.4", "SHA256SUMS", strings.Repeat("0", 64)+"  example_1.2.4_recipe.tar.gz\n")
	if err := f.update("1.2.4", false, false); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("err = %v", err)
	}
	// A template of 1.2.5 that sets the mode.
	bad := version("1.2.5")
	bad["example.toml.in"] = "mode = \"auto\"\n" + bad["example.toml.in"]
	f.rel.Publish("1.2.5", "example_1.2.5_recipe.tar.gz", recipetest.Tarball(t, bad))
	if err := f.update("1.2.5", false, false); err == nil || !strings.Contains(err.Error(), "sets mode") {
		t.Fatalf("err = %v", err)
	}
	if f.text() != before || f.record().Version != "1.2.3" {
		t.Fatal("the config or the record changed")
	}
	for _, v := range []string{"1.2.4", "1.2.5"} {
		if _, err := os.Lstat(f.dir(v)); err == nil {
			t.Fatalf("%s was unpacked", v)
		}
	}
}

func TestUseSwitchesToAFetchedVersion(t *testing.T) {
	f := newUpdateFx(t)
	before := f.text()
	if err := f.update("1.2.4", true, false); err == nil || !strings.Contains(err.Error(), "was not fetched") {
		t.Fatalf("use of a version not fetched: %v", err)
	}
	if _, err := f.o.Fetcher.Fetch(context.Background(), strings.Replace(f.url, "1.2.3", "1.2.4", 2)); err != nil {
		t.Fatal(err)
	}
	if f.text() != before {
		t.Fatal("recipe fetch changed the config")
	}
	// A file of the unpacked version changed: refused.
	apply := filepath.Join(f.dir("1.2.4"), "apply.sh")
	good, _ := os.ReadFile(apply)
	if err := os.WriteFile(apply, []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.update("1.2.4", true, false); err == nil || !strings.Contains(err.Error(), "changed after the unpack") {
		t.Fatalf("use of a changed folder: %v", err)
	}
	if err := os.WriteFile(apply, good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.update("1.2.4", true, false); err != nil {
		t.Fatalf("use: %v\n%s", err, f.out.String())
	}
	if !strings.Contains(f.text(), "v1.2.4") || f.record().Version != "1.2.4" {
		t.Fatalf("config:\n%s", f.text())
	}
	// The same version again is nothing to do; an older one is refused.
	if err := f.update("1.2.4", true, false); err != nil {
		t.Fatalf("use again: %v", err)
	}
	if err := f.update("1.2.3", true, false); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("use older: %v", err)
	}
}

func TestUpdateNeedsARecord(t *testing.T) {
	f := newRecipeFx(t, recipeFiles())
	u := &Update{EtcDir: f.etc, Name: "config-repo", Version: "1.2.4", Fetcher: f.o.Fetcher, Out: f.out, Now: time.Now}
	if err := u.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "bootstrap --recipe") {
		t.Fatalf("err = %v", err)
	}
}

func TestUseRefusesAFolderWithABadRecord(t *testing.T) {
	f := newUpdateFx(t)
	if _, err := f.o.Fetcher.Fetch(context.Background(), strings.Replace(f.url, "1.2.3", "1.2.4", 2)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(f.dir("1.2.4"), recipe.RecordName)
	good, _ := os.ReadFile(p)
	for _, c := range []struct{ body, want string }{
		{strings.Replace(string(good), "v1.2.4/example_1.2.4", "v1.2.5/example_1.2.5", 1), "names another recipe"},
		{strings.Replace(string(good), `"url"`, `"old"`, 1), "no good record"},
		{strings.Replace(string(good), `"sha256"`, `"signer_workflow": ".github/workflows/other.yml", "sha256"`, 1), "--signer-workflow"},
	} {
		if err := os.WriteFile(p, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		before := f.text()
		if err := f.update("1.2.4", true, false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("err = %v, want %q", err, c.want)
		}
		if f.text() != before || f.record().Version != "1.2.3" {
			t.Fatal("the config or the record changed")
		}
	}
}

func TestUpdateRefusesAnotherProject(t *testing.T) {
	f := newUpdateFx(t)
	u := &Update{EtcDir: f.etc, Name: "config-repo", URL: strings.Replace(strings.Replace(f.url, "1.2.3", "1.2.4", 2), recipetest.Owner, "other-org", 1), Fetcher: f.o.Fetcher, Out: f.out, Now: time.Now}
	if err := u.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "is a recipe of other-org") {
		t.Fatalf("err = %v", err)
	}
	if _, err := recipe.ParseURL(strings.Replace(f.url, "v1.2.3", "v01.2.3", 1)); err == nil {
		t.Fatal("a version with a leading zero passed")
	}
}

func TestPruneKeepsAFolderInUse(t *testing.T) {
	f := newUpdateFx(t)
	// Another instance runs 1.2.3 of the same recipe.
	other := recipe.InstanceRecord{Repo: recipetest.Owner + "/" + recipetest.Repo, Version: "1.2.3", Dir: "/elsewhere/1.2.3"}
	if err := os.WriteFile(recipe.RecordPath(f.etc, "other"), other.Encode(), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1.2.4", "1.2.5", "1.2.6"} {
		if err := f.update(v, false, false); err != nil {
			t.Fatalf("update %s: %v", v, err)
		}
	}
	if _, err := os.Lstat(f.dir("1.2.3")); err != nil {
		t.Fatal("the folder that another record names was removed")
	}
	if _, err := os.Lstat(f.dir("1.2.4")); err == nil {
		t.Fatal("the folder of 1.2.4 is still there")
	}
}
