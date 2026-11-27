package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code6x/pullgate/internal/ghdeploy"
	"github.com/code6x/pullgate/internal/ghdeploy/fakegh"
)

const ghToken = "github_pat_DOCTORTEST0123456789"

// withGitHub adds a [github] block, its token file and a fake GitHub.
func withGitHub(f *fix) *fakegh.Server {
	srv := fakegh.New(f.t)
	f.write("etc/"+name+".github-token", ghToken+"\n", 0o600)
	p := filepath.Join(f.base, "etc", name+".toml")
	b, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write("etc/"+name+".toml", string(b)+"\n[github]\nrepository = \"owner/config-repo\"\nenvironment = \"example-host\"\ntoken_file = \""+filepath.Join(f.base, "etc", name+".github-token")+"\"\napi_url = \""+srv.URL+"\"\n", 0o600)
	f.o.GitHubHTTP = ghdeploy.NewHTTPClient(nil)
	return srv
}

func TestGitHubChecks(t *testing.T) {
	f := newFix(t)
	srv := withGitHub(f)
	if n := f.run(); n != 0 {
		t.Fatalf("%d problems:\n%s", n, f.out.String())
	}
	out := f.out.String()
	for _, w := range []string{"[github] is valid", "the queue folder", "not made yet", "run pullgate doctor --github"} {
		if !strings.Contains(out, w) {
			t.Fatalf("no %q:\n%s", w, out)
		}
	}
	if len(srv.All()) != 0 {
		t.Fatal("without --github doctor calls no GitHub")
	}
	// The queue folder as the first run makes it.
	dir := ghdeploy.Dir(filepath.Join(f.o.StateDir, name))
	if _, err := ghdeploy.Open(dir, uint32(os.Geteuid())); err != nil {
		t.Fatal(err)
	}
	f.o.GitHub = true
	if n := f.run(); n != 0 {
		t.Fatalf("%d problems:\n%s", n, f.out.String())
	}
	out = f.out.String()
	if !strings.Contains(out, "the token can read the repo and its deployments. Write access is checked only by the first post") || !strings.Contains(out, "owned by root with mode 0700") {
		t.Fatal(out)
	}
	calls := srv.All()
	if len(calls) != 2 || calls[0].Path != "/repos/owner/config-repo" || calls[1].Path != "/repos/owner/config-repo/deployments?per_page=1" {
		t.Fatalf("calls: %+v", calls)
	}
	for _, c := range calls {
		if c.Method != "GET" || c.Auth != "Bearer "+ghToken {
			t.Fatalf("doctor must only GET: %+v", c)
		}
	}
	if strings.Contains(out, ghToken) {
		t.Fatal("the output shows the token")
	}
	// A token that GitHub refuses, and a queue folder with a wrong mode.
	srv.Answer = func(fakegh.Call) (int, string) { return 401, `{"message": "GITHUB-SAYS"}` }
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if n := f.run(); n != 2 {
		t.Fatalf("%d problems:\n%s", n, f.out.String())
	}
	out = f.out.String()
	if !strings.Contains(out, "HTTP 401: the token is refused") || !strings.Contains(out, "Deployments: read and write") || !strings.Contains(out, "mode -rwx------") || strings.Contains(out, "GITHUB-SAYS") || strings.Contains(out, ghToken) {
		t.Fatal(out)
	}
	for _, c := range srv.All() {
		if c.Method != "GET" {
			t.Fatalf("doctor must only GET: %+v", c)
		}
	}
}

func TestGitHubBadTokenFileIsAProblem(t *testing.T) {
	f := newFix(t)
	withGitHub(f)
	f.write("etc/"+name+".github-token", ghToken+"\n", 0o644)
	if n := f.run(); n == 0 || !strings.Contains(f.out.String(), "[github] token_file") || strings.Contains(f.out.String(), ghToken) {
		t.Fatalf("%d problems:\n%s", n, f.out.String())
	}
}

// /doctor from Telegram: no GitHub call, even with --github.
func TestGitHubPublicCallsNoGitHub(t *testing.T) {
	f := newFix(t)
	srv := withGitHub(f)
	f.o.GitHub, f.o.Public, f.o.Name = true, true, name
	f.run()
	if len(srv.All()) != 0 || strings.Contains(f.out.String(), ghToken) {
		t.Fatalf("calls %d:\n%s", len(srv.All()), f.out.String())
	}
}
