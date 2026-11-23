// Package recipe fetches a recipe: a tarball of an instance's scripts and
// config template in a GitHub release, checked like a release of pullgate
// (DESIGN.md section 19). "pullgate recipe fetch" downloads it, checks its
// SHA-256 and its build attestation, and unpacks it into a folder that
// only root can write.
package recipe

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/safedir"
)

// Limits of the downloads and of the unpack, in bytes and files.
const (
	MaxSmall   = 1 << 20  // SHA256SUMS, the bundle
	MaxTarball = 64 << 20 // the tarball, as it is downloaded
	MaxFile    = 16 << 20 // one file in it
	MaxTotal   = 64 << 20 // all files in it
	MaxFiles   = 1000
)

// ManifestName is the manifest at the root of the tarball.
const ManifestName = "pullgate-recipe.toml"

// RecordName is the record of the unpack, in the version folder.
const RecordName = ".pullgate-recipe.json"

// GHMinVersion is the oldest gh that may check the attestation, as in
// install.sh.
const GHMinVersion = "2.93.0"

// DefaultWorkflow is the workflow that must have built the tarball.
const DefaultWorkflow = ".github/workflows/release.yml"

// Source is a checked recipe URL.
type Source struct {
	URL     string // as given
	Base    string // the release folder: https://github.com/OWNER/REPO/releases/download/vX.Y.Z
	Owner   string
	Repo    string
	Version string // X.Y.Z
	File    string // FILE.tar.gz
	Test    bool   // a test server on http://127.0.0.1:PORT
}

// Tag is vX.Y.Z.
func (s Source) Tag() string { return "v" + s.Version }

// Slug is OWNER/REPO.
func (s Source) Slug() string { return s.Owner + "/" + s.Repo }

var urlRE = regexp.MustCompile(`^(https://github\.com|http://127\.0\.0\.1:[0-9]{1,5})/([A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})/releases/download/v([0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9})/([A-Za-z0-9][A-Za-z0-9_.+-]{0,200}\.tar\.gz)$`)

// ParseURL checks a recipe URL: https://github.com/OWNER/REPO/releases/download/vX.Y.Z/FILE.tar.gz,
// or the same path on a test server http://127.0.0.1:PORT.
func ParseURL(u string) (Source, error) {
	m := urlRE.FindStringSubmatch(u)
	if m == nil || strings.Contains(u, "/../") || strings.HasPrefix(m[2], ".") || strings.HasPrefix(m[3], ".") {
		return Source{}, fmt.Errorf("the recipe URL %q is not a file of a GitHub release. Fix: give https://github.com/OWNER/REPO/releases/download/vX.Y.Z/FILE.tar.gz (no \"latest\", no other host)", u)
	}
	return Source{URL: u, Base: strings.TrimSuffix(u, "/"+m[5]), Owner: m[2], Repo: m[3], Version: m[4], File: m[5], Test: m[1] != "https://github.com"}, nil
}

// Instance is one instance of the manifest.
type Instance struct {
	Name     string   `toml:"name"`
	Config   string   `toml:"config"`
	Requires []string `toml:"requires"`
	Setup    []string `toml:"setup"`
}

// Manifest is pullgate-recipe.toml.
type Manifest struct {
	Format    int        `toml:"format"`
	Version   string     `toml:"version"` // X.Y.Z: must be the tag of the release (signed with the tarball)
	Instances []Instance `toml:"instance"`
}

var (
	progRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	versionRE  = regexp.MustCompile(`^[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}$`)
	workflowRE = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_-][A-Za-z0-9_.-]*\.ya?ml$`)
	relRE      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*(/[A-Za-z0-9_][A-Za-z0-9_.+-]*)*$`)
)

// ParseManifest reads and checks the manifest. An unknown key is an
// error.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	md, err := toml.Decode(string(b), &m)
	if err != nil {
		return nil, fmt.Errorf("%s: %w. Fix: correct the recipe", ManifestName, err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		return nil, fmt.Errorf("%s: unknown key %q. Fix: the recipe is for a newer pullgate, or wrong", ManifestName, u[0].String())
	}
	if m.Format != 1 {
		return nil, fmt.Errorf("%s: format = %d; this pullgate reads format 1. Fix: update pullgate, or use a recipe of format 1", ManifestName, m.Format)
	}
	if !versionRE.MatchString(m.Version) {
		return nil, fmt.Errorf("%s: version %q is not X.Y.Z. Fix: the recipe must say its own version", ManifestName, m.Version)
	}
	if len(m.Instances) == 0 {
		return nil, fmt.Errorf("%s: no [[instance]]. Fix: the recipe must name at least one instance", ManifestName)
	}
	seen := map[string]bool{}
	for _, in := range m.Instances {
		switch {
		case !config.ValidName(in.Name) || seen[in.Name]:
			return nil, fmt.Errorf("%s: instance name %q is not valid or given twice", ManifestName, in.Name)
		case !relRE.MatchString(in.Config) || strings.Contains(in.Config, ".."):
			return nil, fmt.Errorf("%s: instance %s: config %q is not a path in the recipe", ManifestName, in.Name, in.Config)
		case len(in.Setup) > 0 && (!relRE.MatchString(in.Setup[0]) || strings.Contains(in.Setup[0], "..")):
			return nil, fmt.Errorf("%s: instance %s: setup %q is not a program in the recipe", ManifestName, in.Name, in.Setup[0])
		}
		for _, r := range in.Requires {
			if !progRE.MatchString(r) {
				return nil, fmt.Errorf("%s: instance %s: requires %q is not a program name", ManifestName, in.Name, r)
			}
		}
		seen[in.Name] = true
	}
	return &m, nil
}

// LookPath finds a program of requires; tests give a fake.
type LookPath func(name string) (string, error)

// SearchPath is the PATH where requires must be found.
const SearchPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// DefaultLookPath looks in SearchPath only.
func DefaultLookPath(name string) (string, error) {
	for _, d := range filepath.SplitList(SearchPath) {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not in %s", name, SearchPath)
}

// CheckRequires checks the programs that the instances need.
func (m *Manifest) CheckRequires(look LookPath) error {
	var missing []string
	for _, in := range m.Instances {
		for _, r := range in.Requires {
			if _, err := look(r); err != nil && !slices.Contains(missing, r) {
				missing = append(missing, r)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the recipe needs %s, which is not installed (looked in %s). Nothing was written. Fix: install it with the package manager of the host, then run this again", strings.Join(missing, ", "), SearchPath)
	}
	return nil
}

// Fetcher downloads, checks and unpacks a recipe.
type Fetcher struct {
	Dir      string // the recipes folder: /usr/local/lib/pullgate/recipes
	UID      uint32 // root (the test user in tests)
	GH       string // the gh program
	Bundle   string // the bundle name; "" is FILE with .tar.gz replaced by .sigstore.json
	Workflow string // the signer workflow; "" is DefaultWorkflow
	Look     LookPath
	Out      io.Writer
	Client   *http.Client
	// GHTimeout caps gh attestation verify (it gets the Sigstore trusted
	// root online); 0 is 5 minutes.
	GHTimeout time.Duration
}

// Result is a fetched recipe.
type Result struct {
	Source   Source
	SHA256   string
	Dir      string // the version folder
	Manifest *Manifest
}

func (f *Fetcher) say(format string, a ...any) { _, _ = fmt.Fprintf(f.Out, format+"\n", a...) }

func (f *Fetcher) client(test bool) *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{
		Timeout:   10 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, Proxy: nil},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			// GitHub redirects a release file to its storage: HTTPS only.
			local := test && r.URL.Scheme == "http" && r.URL.Hostname() == "127.0.0.1"
			if len(via) > 5 || (r.URL.Scheme != "https" && !local) {
				return errors.New("a redirect to a URL that is not HTTPS")
			}
			return nil
		},
	}
}

// get downloads url, at most max bytes; an HTTP error is a failure.
func (f *Fetcher) get(ctx context.Context, test bool, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client(test).Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w. Fix: check the network and the URL", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d. Fix: check that the release has this file", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("download %s: larger than %d bytes. Fix: the recipe is too large for pullgate", url, max)
	}
	return b, nil
}

// SumFor finds the one line of file in SHA256SUMS and returns its SHA-256.
func SumFor(sums []byte, file string) (string, error) {
	var found []string
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == file {
			found = append(found, strings.ToLower(f[0]))
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("SHA256SUMS has %d lines for %s, not 1. Nothing was written. Fix: do not use this release; report it to its project", len(found), file)
	}
	if len(found[0]) != 64 {
		return "", fmt.Errorf("SHA256SUMS: the line for %s holds no SHA-256", file)
	}
	return found[0], nil
}

// Fetch downloads and checks the recipe at u, and unpacks it. It writes
// nothing unless every check passes.
func (f *Fetcher) Fetch(ctx context.Context, u string) (*Result, error) {
	src, err := ParseURL(u)
	if err != nil {
		return nil, err
	}
	if src.Test {
		f.say("WARNING: the release server is %s, not https://github.com. Use it only for tests.", strings.SplitN(src.URL, "/"+src.Owner+"/", 2)[0])
	}
	bundle := f.Bundle
	if bundle == "" {
		bundle = strings.TrimSuffix(src.File, ".tar.gz") + ".sigstore.json"
	}
	if !progRE.MatchString(bundle) || !strings.HasSuffix(bundle, ".sigstore.json") {
		return nil, fmt.Errorf("--bundle %q is not a file name that ends in .sigstore.json", bundle)
	}
	wf := f.Workflow
	if wf == "" {
		wf = DefaultWorkflow
	}
	if !workflowRE.MatchString(wf) {
		return nil, fmt.Errorf("--signer-workflow %q is not a workflow file. Fix: give .github/workflows/NAME.yml", wf)
	}
	if err := f.checkGH(ctx); err != nil {
		return nil, err
	}
	tarball, err := f.get(ctx, src.Test, src.URL, MaxTarball)
	if err != nil {
		return nil, err
	}
	sums, err := f.get(ctx, src.Test, src.Base+"/SHA256SUMS", MaxSmall)
	if err != nil {
		return nil, err
	}
	b, err := f.get(ctx, src.Test, src.Base+"/"+bundle, MaxSmall)
	if err != nil {
		return nil, err
	}
	want, err := SumFor(sums, src.File)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(tarball)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return nil, fmt.Errorf("the SHA-256 of %s is %s, but SHA256SUMS says %s. Nothing was written. Fix: download again; if it stays, do not use this release", src.File, got, want)
	}
	identity := "https://github.com/" + src.Slug() + "/" + wf + "@refs/heads/main"
	if err := f.verify(ctx, src, tarball, b, got, identity); err != nil {
		return nil, err
	}
	f.say("Recipe: %s %s, file %s", src.Slug(), src.Tag(), src.File)
	f.say("SHA-256: %s", got)
	f.say("Signed by: %s (a GitHub-hosted runner)", identity)
	dir, m, err := f.unpack(src, tarball, got)
	if err != nil {
		return nil, err
	}
	f.say("Unpacked to %s", dir)
	return &Result{Source: src, SHA256: got, Dir: dir, Manifest: m}, nil
}

var ghVersionRE = regexp.MustCompile(`^gh version ([0-9]+)\.([0-9]+)\.([0-9]+)[ \n]`)

// checkGH checks that gh is a root-owned program that only root can
// change, and new enough.
func (f *Fetcher) checkGH(ctx context.Context) error {
	if !filepath.IsAbs(f.GH) {
		return fmt.Errorf("--gh %q is not an absolute path", f.GH)
	}
	if err := safedir.CheckFile(f.GH, f.UID, false); err != nil {
		return fmt.Errorf("--gh: %w", err)
	}
	if err := safedir.CheckParents(filepath.Dir(f.GH), f.UID); err != nil {
		return fmt.Errorf("--gh: %w", err)
	}
	out, err := f.gh(ctx, time.Minute, "", "--version")
	if err != nil {
		return fmt.Errorf("%s --version: %w. Fix: install gh %s or newer, or give it with --gh", f.GH, err, GHMinVersion)
	}
	m := ghVersionRE.FindStringSubmatch(string(out) + " ")
	if m == nil || !atLeast(m[1:], strings.Split(GHMinVersion, ".")) {
		return fmt.Errorf("%s is not gh %s or newer. Fix: update gh, or give another with --gh", f.GH, GHMinVersion)
	}
	return nil
}

func atLeast(have, want []string) bool {
	for i := range 3 {
		h, _ := strconv.Atoi(have[i])
		w, _ := strconv.Atoi(want[i])
		if h != w {
			return h > w
		}
	}
	return true
}

// gh runs gh with no token, an empty home and an empty config, at most
// max, in dir.
func (f *Fetcher) gh(ctx context.Context, max time.Duration, dir string, args ...string) ([]byte, error) {
	home, err := os.MkdirTemp("", "pullgate-gh-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(home) }()
	ctx, cancel := context.WithTimeout(ctx, max)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.GH, args...) //nolint:gosec // gh, checked in checkGH
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "HOME=" + home, "GH_CONFIG_DIR=" + home, "GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1", "NO_COLOR=1"}
	cmd.Dir = home
	if dir != "" {
		cmd.Dir = dir
	}
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return out, nil
}

// verify runs gh attestation verify on the tarball, with the bundle, and
// checks that the signed statement names the file with its SHA-256.
func (f *Fetcher) verify(ctx context.Context, src Source, tarball, bundle []byte, sum, identity string) error {
	tmp, err := os.MkdirTemp("", "pullgate-recipe-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	tf, bf := filepath.Join(tmp, src.File), filepath.Join(tmp, "bundle.sigstore.json")
	if err := os.WriteFile(tf, tarball, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(bf, bundle, 0o600); err != nil {
		return err
	}
	max := f.GHTimeout
	if max == 0 {
		max = 5 * time.Minute
	}
	out, err := f.gh(ctx, max, tmp, "attestation", "verify", tf, "--bundle", bf, "--repo", src.Slug(),
		"--cert-identity", identity, "--deny-self-hosted-runners", "--format", "json")
	if err != nil {
		return fmt.Errorf("gh attestation verify failed, so it is not proven that %s built %s: %w. Nothing was written. Fix: after a network error, try again; else do not use this release", identity, src.File, err)
	}
	var res []struct {
		VerificationResult struct {
			Statement struct {
				Subject []struct {
					Name   string            `json:"name"`
					Digest map[string]string `json:"digest"`
				} `json:"subject"`
			} `json:"statement"`
		} `json:"verificationResult"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return fmt.Errorf("gh attestation verify: cannot read its JSON output: %w", err)
	}
	for _, r := range res {
		for _, s := range r.VerificationResult.Statement.Subject {
			if s.Name == src.File && strings.EqualFold(s.Digest["sha256"], sum) {
				return nil
			}
		}
	}
	return fmt.Errorf("the attestation does not name %s with its SHA-256 %s. Nothing was written. Fix: do not use this release; report it to its project", src.File, sum)
}

// record is the record of an unpack: the tarball and each file.
type record struct {
	SHA256 string            `json:"sha256"`
	Files  map[string]string `json:"files"` // path: SHA-256
}

// unpack unpacks the tarball into Dir/OWNER/REPO/X.Y.Z: in a new
// temporary folder next to it, then a rename. A version folder that is
// there already is used only if only root can write it and it matches its
// record.
func (f *Fetcher) unpack(src Source, tarball []byte, sum string) (string, *Manifest, error) {
	repoDir := filepath.Join(f.Dir, src.Owner, src.Repo)
	if err := f.mkdirs(repoDir); err != nil {
		return "", nil, err
	}
	final := filepath.Join(repoDir, src.Version)
	if _, err := os.Lstat(final); err == nil {
		m, err := f.checkExisting(final, sum)
		if err != nil {
			return "", nil, err
		}
		return final, m, nil
	}
	tmp, err := os.MkdirTemp(repoDir, ".tmp-")
	if err != nil {
		return "", nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()
	rec := record{SHA256: sum, Files: map[string]string{}}
	if err := extract(tarball, tmp, rec.Files); err != nil {
		return "", nil, err
	}
	m, err := readManifest(tmp, src.Version)
	if err != nil {
		return "", nil, err
	}
	if err := m.CheckRequires(f.look()); err != nil {
		return "", nil, err
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, RecordName), b, 0o644); err != nil { //nolint:gosec // the record is not secret, like the recipe files
		return "", nil, err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", nil, err
	}
	ok = true
	return final, m, nil
}

func (f *Fetcher) look() LookPath {
	if f.Look != nil {
		return f.Look
	}
	return DefaultLookPath
}

// mkdirs makes the folders up to dir, owned by root, mode 0755, and checks
// that only root can write them.
func (f *Fetcher) mkdirs(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("make %s: %w", dir, err)
	}
	if err := safedir.CheckParents(dir, f.UID); err != nil {
		return fmt.Errorf("the recipes folder is not safe: %w", err)
	}
	return nil
}

// readManifest reads the manifest of the recipe in dir, whose version
// must be version: the version is signed with the tarball, the tag of
// the URL is not.
func readManifest(dir, version string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestName)) //nolint:gosec // in the new recipe folder
	if err != nil {
		return nil, fmt.Errorf("the recipe has no %s at its root. Fix: this file is not a pullgate recipe", ManifestName)
	}
	m, err := ParseManifest(b)
	if err != nil {
		return nil, withFix(err)
	}
	if m.Version != version {
		return nil, fmt.Errorf("%s says version %s, but the release is v%s: a tarball of another release. Nothing was written. Fix: do not use this release; report it to its project", ManifestName, m.Version, version)
	}
	for _, in := range m.Instances {
		paths := []string{in.Config}
		if len(in.Setup) > 0 {
			paths = append(paths, in.Setup[0])
		}
		for _, p := range paths {
			if fi, err := os.Lstat(filepath.Join(dir, p)); err != nil || !fi.Mode().IsRegular() {
				return nil, withFix(fmt.Errorf("%s: instance %s names %s, which is not a file in the recipe", ManifestName, in.Name, p))
			}
		}
	}
	return m, nil
}

// extract unpacks only plain files and folders, with the limits, into dir,
// and puts the SHA-256 of each file in files.
func extract(tarball []byte, dir string, files map[string]string) error {
	zr, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return fmt.Errorf("the recipe is not a .tar.gz: %w", err)
	}
	tr := tar.NewReader(zr)
	var total int64
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read the recipe: %w", err)
		}
		n++
		name := strings.TrimPrefix(h.Name, "./")
		name = strings.TrimSuffix(name, "/")
		bad := func(why string) error {
			return fmt.Errorf("the recipe holds %q: %s. Nothing was written. Fix: do not use this recipe; report it to its project", h.Name, why)
		}
		switch {
		case n > MaxFiles:
			return bad(fmt.Sprintf("more than %d entries", MaxFiles))
		case name == "" || name == ".":
			if h.Typeflag == tar.TypeDir {
				continue
			}
			return bad("no name")
		case strings.HasPrefix(h.Name, "/"):
			return bad("an absolute path")
		case slices.Contains(strings.Split(name, "/"), ".."):
			return bad("a path with ..")
		case path.Clean(name) != name || !relRE.MatchString(name):
			return bad("a name that pullgate does not allow (A-Za-z0-9_.+- and /)")
		case name == RecordName:
			return bad("the name of pullgate's record")
		case h.Mode&(04000|02000|01000) != 0:
			return bad("a setuid, setgid or sticky bit")
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if h.Size > MaxFile {
				return bad(fmt.Sprintf("a file over %d bytes", MaxFile))
			}
			total += h.Size
			if total > MaxTotal {
				return bad(fmt.Sprintf("more than %d bytes in all", MaxTotal))
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			mode := fs.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			w, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // a checked name in the new folder
			if err != nil {
				return bad("a file given twice, or below a file")
			}
			hs := sha256.New()
			_, err = io.Copy(io.MultiWriter(w, hs), io.LimitReader(tr, MaxFile+1))
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			files[name] = hex.EncodeToString(hs.Sum(nil))
		default:
			return bad("not a plain file or folder (a link, a device, or another type)")
		}
	}
	return nil
}

// checkExisting checks a version folder that is there already: only root
// can write it, and its files are the ones of its record, for this
// tarball.
func (f *Fetcher) checkExisting(dir, sum string) (*Manifest, error) {
	fix := ". Fix: move " + dir + " away, then run this again"
	if err := safedir.CheckTree(dir, f.UID); err != nil {
		return nil, fmt.Errorf("%s is there already and is not safe: %w%s", dir, err, fix)
	}
	b, err := os.ReadFile(filepath.Join(dir, RecordName)) //nolint:gosec // in the checked folder
	if err != nil {
		return nil, fmt.Errorf("%s is there already, with no record of its unpack%s", dir, fix)
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil || rec.SHA256 != sum {
		return nil, fmt.Errorf("%s is there already, from another tarball%s", dir, fix)
	}
	seen := 0
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." || rel == RecordName {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != f.UID || fi.Mode().Perm()&0o022 != 0 || fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
			return fmt.Errorf("%s is not owned by root, or others can write it", p)
		}
		if fi.IsDir() {
			return nil
		}
		want, ok := rec.Files[rel]
		if !ok || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a file of the record", p)
		}
		c, err := os.ReadFile(p) //nolint:gosec // in the checked folder
		if err != nil {
			return err
		}
		s := sha256.Sum256(c)
		if hex.EncodeToString(s[:]) != want {
			return fmt.Errorf("%s changed after the unpack", p)
		}
		seen++
		return nil
	})
	if err == nil && seen != len(rec.Files) {
		err = errors.New("a file of the record is missing")
	}
	if err != nil {
		return nil, fmt.Errorf("%s is there already, but does not match its record: %w%s", dir, err, fix)
	}
	return readManifest(dir, filepath.Base(dir))
}

// withFix adds the fix to an error of the recipe that has none.
func withFix(err error) error {
	if strings.Contains(err.Error(), "Fix") {
		return err
	}
	return fmt.Errorf("%w. Nothing was written. Fix: do not use this recipe; report it to its project", err)
}
