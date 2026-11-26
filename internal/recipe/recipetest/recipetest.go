// Package recipetest is a fake GitHub release of a recipe for tests: a
// release server on http://127.0.0.1:PORT and a fake gh, as a project
// that ships a recipe would use in its own CI (README "Recipes").
package recipetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/code6x/pullgate/internal/testgit"
)

// The fake release: example-org/example-recipe.
const (
	Owner = "example-org"
	Repo  = "example-recipe"
)

// Tarball packs files (path: body) into a .tar.gz. A path that ends in
// ".sh", or that is in exec, gets mode 0755.
func Tarball(t testing.TB, files map[string]string, exec ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		mode := int64(0o644)
		if strings.HasSuffix(n, ".sh") || contains(exec, n) {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: mode, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(files[n]))
	}
	_ = tw.Close()
	_ = zw.Close()
	return b.Bytes()
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Release is the fake release server and the fake gh.
type Release struct {
	mu    sync.Mutex
	files map[string][]byte
	srv   *httptest.Server
	GH    string // the fake gh: owned by the test user, in a private folder
}

// New starts a release server with no files, and writes the fake gh.
func New(t testing.TB) *Release {
	t.Helper()
	r := &Release{files: map[string][]byte{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		b, ok := r.files[q.URL.Path]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, q)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(r.srv.Close)
	base := testgit.PrivateDir(t)
	r.GH = filepath.Join(base, "gh")
	// It checks the flags as the real gh would, and prints the statement
	// with the name and the SHA-256 of the file.
	script := `#!/bin/sh
if [ "$1" = --version ]; then echo "gh version 2.100.0 (2026-01-01)"; exit 0; fi
[ "$1 $2" = "attestation verify" ] || exit 9
f=$3; shift 3
while [ $# -gt 0 ]; do
	case $1 in
	--repo) [ "$2" = "` + Owner + "/" + Repo + `" ] || { echo "repo mismatch" >&2; exit 1; }; shift 2 ;;
	--cert-identity) [ "$2" = "https://github.com/` + Owner + "/" + Repo + `/.github/workflows/release.yml@refs/heads/main" ] || { echo "identity mismatch" >&2; exit 1; }; shift 2 ;;
	--deny-self-hosted-runners) self=1; shift ;;
	--bundle|--format) shift 2 ;;
	*) shift ;;
	esac
done
[ "$self" = 1 ] || { echo "no --deny-self-hosted-runners" >&2; exit 1; }
s=$(sha256sum "$f" | cut -d' ' -f1)
printf '[{"verificationResult":{"statement":{"subject":[{"name":"%s","digest":{"sha256":"%s"}}]}}}]' "$(basename "$f")" "$s"
`
	if err := os.WriteFile(r.GH, []byte(script), 0o755); err != nil { //nolint:gosec // a test program
		t.Fatal(err)
	}
	return r
}

// Publish puts the tarball as file of the release vVERSION, with its
// SHA256SUMS and a bundle, and returns its URL.
func (r *Release) Publish(version, file string, tarball []byte) string {
	p := "/" + Owner + "/" + Repo + "/releases/download/v" + version + "/"
	s := sha256.Sum256(tarball)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[p+file] = tarball
	r.files[p+"SHA256SUMS"] = []byte(hex.EncodeToString(s[:]) + "  " + file + "\n")
	r.files[p+strings.TrimSuffix(file, ".tar.gz")+".sigstore.json"] = []byte("{}")
	return r.srv.URL + p + file
}

// Replace sets the file name of the release vVERSION to body.
func (r *Release) Replace(version, name, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files["/"+Owner+"/"+Repo+"/releases/download/v"+version+"/"+name] = []byte(body)
}
