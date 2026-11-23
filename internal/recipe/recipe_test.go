package recipe

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/code6x/pullgate/internal/testgit"
)

func TestMain(m *testing.M) {
	syscall.Umask(0o022)
	os.Exit(m.Run())
}

const manifest = `format = 1
version = "1.2.3"

[[instance]]
name = "example"
config = "example.toml.in"
requires = ["sh"]
`

type entry struct {
	name string
	body string
	mode int64
	typ  byte
	link string
}

func tarball(t *testing.T, es []entry) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, e := range es {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Mode: mode, Typeflag: typ, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = zw.Close()
	return b.Bytes()
}

func good() []entry {
	return []entry{{name: ManifestName, body: manifest}, {name: "example.toml.in", body: "mode = \"ask\"\n"}, {name: "bin/apply.sh", body: "#!/bin/sh\n", mode: 0o755}}
}

// rel is a fake GitHub release: the files, a fake gh, and the fetcher.
type rel struct {
	t     *testing.T
	files map[string][]byte
	srv   *httptest.Server
	f     *Fetcher
	ghLog string
	out   bytes.Buffer
}

const file = "example_1.2.3_recipe.tar.gz"

func newRel(t *testing.T, tb []byte) *rel {
	t.Helper()
	r := &rel{t: t, files: map[string][]byte{}}
	r.set(tb)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		b, ok := r.files[q.URL.Path]
		if !ok {
			http.NotFound(w, q)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(r.srv.Close)
	base := testgit.PrivateDir(t)
	r.ghLog = filepath.Join(base, "gh.log")
	gh := filepath.Join(base, "gh")
	// The fake gh: --version, and attestation verify, which checks the
	// flags as the real one would, and prints the statement.
	script := `#!/bin/sh
echo "$@" >> ` + r.ghLog + `
if [ "$1" = --version ]; then echo "gh version 2.100.0 (2026-01-01)"; exit 0; fi
[ "$1 $2" = "attestation verify" ] || exit 9
want_repo=example-org/example-recipe
want_id=https://github.com/example-org/example-recipe/.github/workflows/release.yml@refs/heads/main
f=$3; shift 3
while [ $# -gt 0 ]; do
	case $1 in
	--repo) [ "$2" = "$want_repo" ] || { echo "repo mismatch" >&2; exit 1; }; shift 2 ;;
	--cert-identity) [ "$2" = "$want_id" ] || { echo "identity mismatch" >&2; exit 1; }; shift 2 ;;
	--bundle|--format) shift 2 ;;
	--deny-self-hosted-runners) self=1; shift ;;
	*) shift ;;
	esac
done
[ "$self" = 1 ] || { echo "no --deny-self-hosted-runners" >&2; exit 1; }
[ -f ` + base + `/selfhosted ] && { echo "self-hosted runner" >&2; exit 1; }
if [ -f ` + base + `/body ]; then cat ` + base + `/body; exit 0; fi
s=$(sha256sum "$f" | cut -d' ' -f1)
printf '[{"verificationResult":{"statement":{"subject":[{"name":"%s","digest":{"sha256":"%s"}}]}}}]' "$(basename "$f")" "$s"
`
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(testgit.PrivateDir(t), "recipes")
	r.f = &Fetcher{Dir: dir, UID: uint32(os.Geteuid()), GH: gh, Out: &r.out,
		Look: func(n string) (string, error) {
			if n == "sh" {
				return "/bin/sh", nil
			}
			return "", errors.New("not found")
		}}
	return r
}

func (r *rel) path(n string) string {
	return "/example-org/example-recipe/releases/download/v1.2.3/" + n
}

func (r *rel) set(tb []byte) {
	s := sha256.Sum256(tb)
	r.files[r.path(file)] = tb
	r.files[r.path("SHA256SUMS")] = []byte(hex.EncodeToString(s[:]) + "  " + file + "\n0000000000000000000000000000000000000000000000000000000000000000  other.tar.gz\n")
	r.files[r.path("example_1.2.3_recipe.sigstore.json")] = []byte("{}")
}

func (r *rel) url() string { return r.srv.URL + r.path(file) }

func (r *rel) fetch() (*Result, error) {
	return r.f.Fetch(context.Background(), r.url())
}

// nothing checks that no version folder was written.
func (r *rel) nothing() {
	r.t.Helper()
	ents, _ := os.ReadDir(filepath.Join(r.f.Dir, "example-org", "example-recipe"))
	if len(ents) != 0 {
		r.t.Fatalf("written: %v", ents)
	}
}

func TestFetchGood(t *testing.T) {
	r := newRel(t, tarball(t, good()))
	res, err := r.fetch()
	if err != nil {
		t.Fatalf("fetch: %v\n%s", err, r.out.String())
	}
	if res.Dir != filepath.Join(r.f.Dir, "example-org", "example-recipe", "1.2.3") || res.Manifest.Instances[0].Name != "example" {
		t.Fatalf("result: %+v", res)
	}
	fi, err := os.Stat(filepath.Join(res.Dir, "bin/apply.sh"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("apply.sh: %v %v", fi, err)
	}
	out := r.out.String()
	for _, w := range []string{"WARNING: the release server", "Recipe: example-org/example-recipe v1.2.3", "SHA-256: " + res.SHA256, "Signed by: https://github.com/example-org/example-recipe/.github/workflows/release.yml@refs/heads/main"} {
		if !strings.Contains(out, w) {
			t.Fatalf("no %q:\n%s", w, out)
		}
	}
	// Again: the folder is there and matches its record.
	if _, err := r.fetch(); err != nil {
		t.Fatalf("fetch again: %v", err)
	}
	// A changed file in it is refused.
	if err := os.WriteFile(filepath.Join(res.Dir, "bin/apply.sh"), []byte("#!/bin/sh\nevil\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetch(); err == nil || !strings.Contains(err.Error(), "does not match its record") {
		t.Fatalf("a changed folder: %v", err)
	}
}

func TestURLs(t *testing.T) {
	for _, u := range []string{
		"http://github.com/o/r/releases/download/v1.2.3/f.tar.gz",
		"https://example.com/o/r/releases/download/v1.2.3/f.tar.gz",
		"https://github.com/o/r/releases/latest/download/f.tar.gz",
		"https://github.com/o/r/releases/download/1.2.3/f.tar.gz",
		"https://github.com/o/r/releases/download/v1.2.3/f.zip",
		"https://github.com/o/../releases/download/v1.2.3/f.tar.gz",
		"https://github.com/o/r/releases/download/v1.2.3/f.tar.gz?x=1",
		"http://127.0.0.2:80/o/r/releases/download/v1.2.3/f.tar.gz",
		"http://127.0.0.1:80@example.com/o/r/releases/download/v1.2.3/f.tar.gz",
	} {
		if _, err := ParseURL(u); err == nil {
			t.Errorf("ParseURL(%q) passed", u)
		}
	}
	s, err := ParseURL("https://github.com/example-org/example-recipe/releases/download/v1.2.3/" + file)
	if err != nil || s.Test || s.Slug() != "example-org/example-recipe" || s.Version != "1.2.3" || s.File != file {
		t.Fatalf("%+v %v", s, err)
	}
}

// Each deny path writes nothing.
func TestDeny(t *testing.T) {
	big := strings.Repeat("x", MaxFile+1)
	for _, c := range []struct {
		name string
		tb   []entry
		set  func(r *rel)
		want string
	}{
		{"wrong SHA-256", nil, func(r *rel) {
			r.files[r.path("SHA256SUMS")] = []byte(strings.Repeat("ab", 32) + "  " + file + "\n")
		}, "but SHA256SUMS says"},
		{"no line in SHA256SUMS", nil, func(r *rel) { r.files[r.path("SHA256SUMS")] = []byte("") }, "has 0 lines"},
		{"two lines in SHA256SUMS", nil, func(r *rel) {
			b := r.files[r.path("SHA256SUMS")]
			r.files[r.path("SHA256SUMS")] = append(b, b...)
		}, "has 2 lines"},
		{"no bundle", nil, func(r *rel) { delete(r.files, r.path("example_1.2.3_recipe.sigstore.json")) }, "HTTP 404"},
		{"attestation of another repo", nil, func(r *rel) {
			r.f.GH = r.rewriteGH("want_repo=example-org/example-recipe", "want_repo=other/repo")
		}, "gh attestation verify failed"},
		{"attestation of another workflow", nil, func(r *rel) { r.f.Workflow = ".github/workflows/other.yml" }, "gh attestation verify failed"},
		{"attestation of another branch", nil, func(r *rel) {
			r.f.GH = r.rewriteGH("@refs/heads/main", "@refs/heads/dev")
		}, "gh attestation verify failed"},
		{"self-hosted runner", nil, func(r *rel) { r.write("selfhosted", "") }, "gh attestation verify failed"},
		{"statement does not name the file", nil, func(r *rel) {
			r.write("body", `[{"verificationResult":{"statement":{"subject":[{"name":"other.tar.gz","digest":{"sha256":"00"}}]}}}]`)
		}, "does not name"},
		{"a symlink", append(good(), entry{name: "link", typ: tar.TypeSymlink, link: "/etc/shadow"}), nil, "not a plain file"},
		{"a hard link", append(good(), entry{name: "hard", typ: tar.TypeLink, link: "bin/apply.sh"}), nil, "not a plain file"},
		{"a device", append(good(), entry{name: "dev", typ: tar.TypeChar}), nil, "not a plain file"},
		{"..", append(good(), entry{name: "../evil", body: "x"}), nil, "a path with .."},
		{"absolute path", append(good(), entry{name: "/etc/evil", body: "x"}), nil, "an absolute path"},
		{"setuid", append(good(), entry{name: "suid", body: "x", mode: 0o4755}), nil, "setuid"},
		{"too large", append(good(), entry{name: "big", body: big}), nil, "a file over"},
		{"unknown manifest key", []entry{{name: ManifestName, body: manifest + "evil = 1\n"}, {name: "example.toml.in", body: ""}}, nil, "unknown key"},
		{"a tarball of another version", []entry{{name: ManifestName, body: strings.Replace(manifest, "1.2.3", "1.0.0", 1)}, {name: "example.toml.in", body: ""}}, nil, "a tarball of another release"},
		{"no manifest", []entry{{name: "example.toml.in", body: ""}}, nil, "has no " + ManifestName},
		{"config not in the recipe", []entry{{name: ManifestName, body: manifest}}, nil, "not a file in the recipe"},
		{"missing requires", []entry{{name: ManifestName, body: strings.Replace(manifest, `["sh"]`, `["sh", "ansible-playbook"]`, 1)}, {name: "example.toml.in", body: ""}}, nil, "needs ansible-playbook"},
		{"gh too old", nil, func(r *rel) { r.f.GH = r.rewriteGH("2.100.0", "2.90.0") }, "is not gh 2.93.0 or newer"},
		{"gh writable by others", nil, func(r *rel) { _ = os.Chmod(r.f.GH, 0o777) }, "--gh"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tb := c.tb
			if tb == nil {
				tb = good()
			}
			r := newRel(t, tarball(t, tb))
			if c.set != nil {
				c.set(r)
			}
			_, err := r.fetch()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("fetch: %v, want %q", err, c.want)
			}
			if !strings.Contains(err.Error(), "Fix") {
				t.Fatalf("no fix in the error: %v", err)
			}
			r.nothing()
		})
	}
}

func (r *rel) write(n, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(r.ghLog), n), []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// rewriteGH writes a copy of the fake gh with old replaced by new.
func (r *rel) rewriteGH(old, new string) string {
	r.t.Helper()
	b, err := os.ReadFile(r.f.GH)
	if err != nil {
		r.t.Fatal(err)
	}
	p := r.f.GH + "2"
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o755); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func TestManifest(t *testing.T) {
	for _, bad := range []string{
		"format = 2\nversion = \"1.0.0\"\n[[instance]]\nname = \"a\"\nconfig = \"a.toml.in\"\n",
		"format = 1\nversion = \"1.0.0\"\n",
		"format = 1\n[[instance]]\nname = \"a\"\nconfig = \"a\"\n",
		"format = 1\nversion = \"1.0.0\"\n[[instance]]\nname = \"A\"\nconfig = \"a.toml.in\"\n",
		"format = 1\nversion = \"1.0.0\"\n[[instance]]\nname = \"a\"\nconfig = \"../a.toml.in\"\n",
		"format = 1\nversion = \"1.0.0\"\n[[instance]]\nname = \"a\"\nconfig = \"a\"\nsetup = [\"/bin/sh\"]\n",
		"format = 1\nversion = \"1.0.0\"\n[[instance]]\nname = \"a\"\nconfig = \"a\"\nrequires = [\"a b\"]\n",
		"format = 1\nversion = \"1.0.0\"\n[[instance]]\nname = \"a\"\nconfig = \"a\"\n[[instance]]\nname = \"a\"\nconfig = \"b\"\n",
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Errorf("ParseManifest passed:\n%s", bad)
		}
	}
	if m, err := ParseManifest([]byte(manifest)); err != nil || len(m.Instances) != 1 {
		t.Fatal(m, err)
	}
	_ = fmt.Sprint()
}
