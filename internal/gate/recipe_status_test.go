package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code6x/pullgate/internal/recipe"
)

func TestStatusShowsTheRecipe(t *testing.T) {
	f := newFixture(t, ``, "")
	if s := f.status(); strings.Contains(s, "recipe:") {
		t.Fatalf("a recipe line with no record:\n%s", s)
	}
	p := recipe.RecordPath(filepath.Dir(f.env.Cfg.Path), f.env.Cfg.Name)
	r := recipe.InstanceRecord{Repo: "example-org/example-recipe", Version: "1.2.3", File: "example_1.2.3_recipe.tar.gz", Instance: "example", Dir: "/usr/local/lib/pullgate/recipes/example-org/example-recipe/1.2.3"}
	if err := os.WriteFile(p, r.Encode(), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := f.status(); !strings.Contains(s, "recipe:      example-org/example-recipe v1.2.3 (example_1.2.3_recipe.tar.gz, instance example, in /usr/local/lib/pullgate/recipes/example-org/example-recipe/1.2.3)") {
		t.Fatalf("status:\n%s", s)
	}
	if err := os.WriteFile(p, []byte("version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := f.status(); !strings.Contains(s, "cannot be read") {
		t.Fatalf("status of a bad record:\n%s", s)
	}
}
