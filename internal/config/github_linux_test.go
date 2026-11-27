package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code6x/pullgate/internal/testgit"
)

func ghConfig(url, block string) string {
	return strings.Replace(good, "ssh://git@git.example.com/owner/config-repo.git", url, 1) + block
}

const ghBlock = `
[github]
repository = "owner/config-repo"
environment = "example-host"
token_file = "/etc/pullgate/x.github-token"
`

func TestGitHubBlock(t *testing.T) {
	c, err := Load(write(t, good, 0o600), "x", uid())
	if err != nil || c.GitHub != nil {
		t.Fatalf("no block: %+v %v", c.GitHub, err)
	}
	c, err = Load(write(t, good+ghBlock, 0o600), "x", uid())
	if err != nil || c.GitHub == nil || c.GitHub.API() != DefaultGitHubAPI || c.GitHub.APIURL != "" || c.GitHub.Environment != "example-host" {
		t.Fatalf("block: %+v %v", c.GitHub, err)
	}
}

// Each form of a GitHub url matches without case and without .git; a
// url of another host is not compared.
func TestGitHubRepoMatch(t *testing.T) {
	for _, u := range []string{
		"git@github.com:owner/config-repo.git", "git@github.com:Owner/Config-Repo", "git@GitHub.com:owner/config-repo",
		"ssh://git@github.com/owner/config-repo.git", "ssh://git@github.com/OWNER/config-repo",
		"ssh://git@ssh.github.com:443/owner/config-repo.git", "ssh://git@ssh.github.com:443/owner/config-repo",
		"ssh://git@git.example.com/other/repo.git",
	} {
		if _, err := Load(write(t, ghConfig(u, ghBlock), 0o600), "x", uid()); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{
		"git@github.com:owner/other-repo.git", "git@github.com:other/config-repo",
		"ssh://git@github.com/owner/config-repo2.git", "ssh://git@ssh.github.com:443/someone/config-repo",
		"ssh://git@github.com/owner/config-repo/extra",
	} {
		_, err := Load(write(t, ghConfig(u, ghBlock), 0o600), "x", uid())
		if err == nil || !strings.Contains(err.Error(), "[github] repository") || !strings.Contains(err.Error(), "Fix:") {
			t.Errorf("%s: a mismatch must be a config error: %v", u, err)
		}
	}
}

func TestGitHubBlockRefused(t *testing.T) {
	rep := func(a, b string) string { return good + strings.Replace(ghBlock, a, b, 1) }
	cases := map[string]string{
		"no repository":     rep(`repository = "owner/config-repo"`, ""),
		"bad repository":    rep("owner/config-repo", "owner/config-repo/x"),
		"repository .git":   rep("owner/config-repo", "owner/config-repo.git"),
		"no environment":    rep(`environment = "example-host"`, ""),
		"bad environment":   rep("example-host", "Example Host"),
		"no token_file":     rep(`token_file = "/etc/pullgate/x.github-token"`, ""),
		"relative token":    rep("/etc/pullgate/x.github-token", "x.github-token"),
		"unclean token":     rep("/etc/pullgate/x.github-token", "/etc/pullgate/../x"),
		"http api":          good + ghBlock + `api_url = "http://api.github.example"` + "\n",
		"user info api":     good + ghBlock + `api_url = "https://u@api.github.com"` + "\n",
		"unknown key":       good + ghBlock + `token = "x"` + "\n",
		"key in upper case": good + strings.Replace(ghBlock, "repository", "Repository", 1),
	}
	for n, body := range cases {
		_, err := Load(write(t, body, 0o600), "x", uid())
		if err == nil {
			t.Errorf("%s: no error", n)
			continue
		}
		if strings.Contains(n, "api") && !strings.Contains(err.Error(), DefaultGitHubAPI) {
			t.Errorf("%s: the fix must name %s: %v", n, DefaultGitHubAPI, err)
		}
	}
	if _, err := Load(write(t, good+ghBlock+`api_url = "http://127.0.0.1:8080"`+"\n", 0o600), "x", uid()); err != nil {
		t.Fatalf("a test server on 127.0.0.1 is allowed: %v", err)
	}
}

func tokenConfig(t *testing.T, dir string) *Instance {
	t.Helper()
	return &Instance{Name: "x", Path: "x.toml", GitHub: &GitHub{Repository: "owner/config-repo", Environment: "example-host", TokenFile: filepath.Join(dir, "x.github-token")}}
}

func TestGitHubToken(t *testing.T) {
	d := testgit.PrivateDir(t)
	c := tokenConfig(t, d)
	p := c.GitHub.TokenFile
	put := func(body string, mode os.FileMode) {
		t.Helper()
		_ = os.Remove(p)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	const tok = "github_pat_TESTONLY0123456789"
	for _, body := range []string{tok, tok + "\n"} {
		put(body, 0o600)
		got, err := c.ReadGitHubToken(uid())
		if err != nil || got != tok {
			t.Fatalf("%q: %v", body, err)
		}
	}
	bad := map[string]func(){
		"two newlines":   func() { put(tok+"\n\n", 0o600) },
		"a space":        func() { put("github_pat_ TESTONLY", 0o600) },
		"a tab":          func() { put(tok+"\t", 0o600) },
		"cr":             func() { put(tok+"\r\n", 0o600) },
		"two lines":      func() { put(tok+"\nsecond", 0o600) },
		"empty":          func() { put("", 0o600) },
		"only a newline": func() { put("\n", 0o600) },
		"too big":        func() { put(strings.Repeat("a", MaxGitHubToken+1), 0o600) },
		"group can read": func() { put(tok, 0o640) },
		"others read":    func() { put(tok, 0o604) },
		"group write":    func() { put(tok, 0o620) },
		"others write":   func() { put(tok, 0o602) },
		"a symlink": func() {
			put(tok, 0o600)
			_ = os.Rename(p, p+".real")
			if err := os.Symlink(p+".real", p); err != nil {
				t.Fatal(err)
			}
		},
		"missing": func() { _ = os.Remove(p) },
	}
	for n, setup := range bad {
		setup()
		_, err := c.ReadGitHubToken(uid())
		if err == nil {
			t.Errorf("%s: no error", n)
			continue
		}
		if strings.Contains(err.Error(), "TESTONLY") || !strings.Contains(err.Error(), "[github] token_file") {
			t.Errorf("%s: the error must name the key and never show the token: %v", n, err)
		}
		_ = os.Remove(p + ".real")
	}
	put(tok, 0o600)
	if _, err := c.ReadGitHubToken(uid() + 1); err == nil {
		t.Error("a token file of another owner (not root) must be refused")
	}
	// A folder that others can write is not safe.
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(d, 0o700) }()
	if _, err := c.ReadGitHubToken(uid()); err == nil {
		t.Error("a token file in a folder that others can write must be refused")
	}
}
