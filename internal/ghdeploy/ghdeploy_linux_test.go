package ghdeploy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/code6x/pullgate/internal/ghdeploy/fakegh"
	"github.com/code6x/pullgate/internal/testgit"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type tfix struct {
	t   *testing.T
	dir string
	q   *Queue
	srv *fakegh.Server
	now time.Time
	log bytes.Buffer
	ref []string
}

func newTfix(t *testing.T) *tfix {
	f := &tfix{t: t, dir: filepath.Join(testgit.PrivateDir(t), "github"), srv: fakegh.New(t), now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	f.open()
	return f
}

func (f *tfix) open() {
	f.t.Helper()
	q, err := Open(f.dir, uint32(os.Geteuid()))
	if err != nil {
		f.t.Fatal(err)
	}
	f.q = q
}

func (f *tfix) sender() *Sender {
	return &Sender{
		Q: f.q, Log: slog.New(slog.NewTextHandler(&f.log, nil)), Now: func() time.Time { return f.now },
		Deadline: time.Now().Add(5 * time.Second), Instance: "config-repo",
		C:       &Client{API: f.srv.URL, Repository: "owner/config-repo", Environment: "example-host", Token: "test-token", HTTP: NewHTTPClient(nil)},
		Refused: func(text string) { f.ref = append(f.ref, text) },
	}
}

func (f *tfix) send() { f.sender().Send(context.Background()) }

func TestDescriptionsAreShort(t *testing.T) {
	for _, d := range Descriptions {
		if len(d) > 140 || d == "" {
			t.Fatalf("%q is %d characters", d, len(d))
		}
	}
	if s := RefusedText("config-repo", "owner/config-repo"); !strings.Contains(s, "Deployments: read and write") {
		t.Fatal(s)
	}
}

// Each answer of GitHub: sent, tried again, or dropped with its
// statuses, as DESIGN.md section 22 says.
func TestAnswers(t *testing.T) {
	cases := []struct {
		code    int
		status  bool // the code is for the status, not the create
		hdr     string
		kind    Kind
		refused bool
	}{
		{500, false, "", Retry, false}, {502, true, "", Retry, false}, {429, false, "", Retry, false},
		{401, false, "", Retry, true}, {403, false, "", Retry, true}, {403, false, "rate", Retry, false},
		{404, false, "", Retry, true}, {404, true, "", Drop, false},
		{409, false, "", Drop, false}, {422, false, "", Drop, false}, {422, true, "", Drop, false},
		{202, false, "", Drop, false}, {200, true, "", Drop, false}, {418, false, "", Drop, false},
		{301, false, "", Drop, false},
	}
	for _, c := range cases {
		f := newTfix(t)
		var hit atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			isStatus := strings.HasSuffix(r.URL.Path, "/statuses")
			if isStatus == c.status && hit.Add(1) == 1 {
				if c.hdr == "rate" {
					w.Header().Set("X-RateLimit-Remaining", "0")
				}
				if c.code == 301 {
					w.Header().Set("Location", "/moved")
				}
				w.WriteHeader(c.code)
				_, _ = io.WriteString(w, `{"message": "GITHUB-TEXT"}`)
				return
			}
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"id": 7}`)
		}))
		k := f.q.Start(RoleApply, shaA, "example-host", DescApplyStarted, f.now)
		f.q.Status(k, Success, DescApplied, f.now)
		k2 := f.q.Start(RoleApply, shaB, "example-host", DescApplyStarted, f.now)
		s := f.sender()
		s.C.API, s.C.HTTP = srv.URL, NewHTTPClient(nil)
		if c.status {
			f.q.Deployments[0].ID = 7
		}
		a := s.C.Post(context.Background(), f.q.Deployments[0], &Post{Create: !c.status, State: Success, Description: DescApplied})
		if a.Kind != c.kind || a.Refused != c.refused || a.Code != c.code || strings.Contains(a.Reason, "GITHUB-TEXT") {
			t.Fatalf("HTTP %d (status %v): %+v", c.code, c.status, a)
		}
		f.q.Deployments[0].ID = 0
		hit.Store(0)
		s.Send(context.Background())
		switch c.kind {
		case Retry:
			if len(f.q.Posts) == 0 || f.q.Last == nil || f.q.Last.OK || f.q.find(k).ID != 0 && !c.status {
				t.Fatalf("HTTP %d: a retry must keep the post and stop: %d posts, %d calls", c.code, len(f.q.Posts), hit.Load())
			}
		case Drop:
			// The rest of the deployment is dropped; the next one is sent.
			if len(f.q.Posts) != 0 || f.q.find(k2).ID != 7 || !f.q.find(k).Dropped {
				t.Fatalf("HTTP %d: a drop must drop the statuses of its deployment and go on: %+v", c.code, f.q.Posts)
			}
		}
		if c.code == 301 && !strings.Contains(f.log.String(), "the repo moved") {
			t.Fatalf("a redirect must say the repo moved: %+v", f.q.Last)
		}
		if strings.Contains(f.log.String(), "GITHUB-TEXT") || strings.Contains(f.log.String(), "test-token") {
			t.Fatal("the log shows the answer of GitHub or the token")
		}
		srv.Close()
	}
}

func TestStatusBeforeItsCreateWasSentFindsItAfterARestart(t *testing.T) {
	f := newTfix(t)
	down := true
	f.srv.Answer = func(fakegh.Call) (int, string) {
		if down {
			return 503, ""
		}
		return 0, ""
	}
	k := f.q.Start(RoleApply, shaA, "example-host", DescApplyStarted, f.now)
	f.q.Status(k, Success, DescApplied, f.now)
	if err := f.q.Save(); err != nil {
		t.Fatal(err)
	}
	f.send()
	if len(f.q.Posts) != 3 {
		t.Fatalf("all must wait: %d", len(f.q.Posts))
	}
	f.open() // a new process
	down = false
	f.send()
	want := []string{"create " + shaA, "in_progress " + shaA + " " + DescApplyStarted, "success " + shaA + " " + DescApplied}
	if got := f.srv.Steps(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", got)
	}
	f.open()
	if len(f.q.Posts) != 0 || f.q.Deployments[0].ID == 0 || f.q.Last == nil || !f.q.Last.OK {
		t.Fatalf("the queue on disk: %+v", f.q)
	}
}

func TestDroppedAfter24Hours(t *testing.T) {
	f := newTfix(t)
	f.srv.Answer = func(fakegh.Call) (int, string) { return 500, "" }
	k := f.q.Start(RoleApply, shaA, "example-host", DescApplyStarted, f.now)
	f.now = f.now.Add(23 * time.Hour)
	f.q.Status(k, Success, DescApplied, f.now)
	f.send()
	if len(f.q.Posts) != 3 {
		t.Fatal("kept before 24 hours")
	}
	f.now = f.now.Add(time.Hour)
	f.srv.Answer = nil
	k2 := f.q.Start(RoleApply, shaB, "example-host", DescApplyStarted, f.now)
	f.send()
	if len(f.q.Posts) != 0 || !f.q.find(k).Dropped || f.q.find(k2).ID == 0 {
		t.Fatalf("after 24 hours the create and its statuses are dropped: %+v", f.q.Posts)
	}
	if !strings.Contains(f.log.String(), "older than 24 hours") {
		t.Fatal("no log line")
	}
	for _, s := range f.srv.Steps() {
		if strings.Contains(s, shaA) {
			t.Fatalf("a dropped post was sent: %s", s)
		}
	}
}

func TestTokenRefusedOncePerDay(t *testing.T) {
	f := newTfix(t)
	f.srv.Answer = func(fakegh.Call) (int, string) { return 401, "" }
	f.q.Start(RoleApply, shaA, "example-host", DescApplyStarted, f.now)
	f.send()
	f.now = f.now.Add(time.Hour)
	f.send()
	if len(f.ref) != 1 || f.ref[0] != RefusedText("config-repo", "owner/config-repo") || strings.Count(f.log.String(), "is refused: make a new one") != 1 {
		t.Fatalf("once a day, fixed text: %q\n%s", f.ref, f.log.String())
	}
	f.now = f.now.Add(24 * time.Hour)
	f.q.Start(RoleApply, shaB, "example-host", DescApplyStarted, f.now)
	f.send()
	if len(f.ref) != 2 {
		t.Fatal("the next day again")
	}
}

func TestSendStopsAtTheDeadline(t *testing.T) {
	f := newTfix(t)
	f.srv.Hang(t)
	f.q.Start(RoleApply, shaA, "example-host", DescApplyStarted, f.now)
	s := f.sender()
	s.Deadline = time.Now().Add(300 * time.Millisecond)
	start := time.Now()
	s.Send(context.Background())
	s.Send(context.Background())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s", d)
	}
	if len(f.q.Posts) != 2 {
		t.Fatal("the posts must wait")
	}
}

func TestAnswerSizeLimit(t *testing.T) {
	f := newTfix(t)
	f.srv.Answer = func(fakegh.Call) (int, string) {
		return 201, `{"id": 5, "x": "` + strings.Repeat("a", maxAnswer) + `"}`
	}
	f.q.Start(RoleApply, shaA, "example-host", DescApplyStarted, f.now)
	f.send()
	if len(f.q.Posts) != 0 || !f.q.Deployments[0].Dropped || f.q.Deployments[0].ID != 0 {
		t.Fatalf("a too large answer is not read: %+v", f.q.Deployments[0])
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var moved atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			moved.Add(1)
			return
		}
		http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := &Client{API: srv.URL, Repository: "owner/config-repo", Token: "t", HTTP: NewHTTPClient(nil)}
	a := c.Post(context.Background(), &Deployment{SHA: shaA}, &Post{Create: true})
	if a.Kind != Drop || moved.Load() != 0 || !strings.Contains(a.Reason, "the repo moved") {
		t.Fatalf("%+v, %d", a, moved.Load())
	}
}

func tlsServer(t *testing.T, maxVersion uint16) (*httptest.Server, *x509.CertPool, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id": 9}`)
	}))
	srv.TLS = &tls.Config{MaxVersion: maxVersion, MinVersion: tls.VersionTLS10}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool, &hits
}

func TestOldTLSIsRefused(t *testing.T) {
	srv, pool, hits := tlsServer(t, tls.VersionTLS11)
	c := &Client{API: srv.URL, Repository: "owner/config-repo", Token: "t", HTTP: NewHTTPClient(pool)}
	if a := c.Post(context.Background(), &Deployment{SHA: shaA}, &Post{Create: true}); a.Kind != Retry || hits.Load() != 0 {
		t.Fatalf("TLS 1.1 must fail: %+v", a)
	}
	srv2, pool2, hits2 := tlsServer(t, tls.VersionTLS12)
	c = &Client{API: srv2.URL, Repository: "owner/config-repo", Token: "t", HTTP: NewHTTPClient(pool2)}
	if a := c.Post(context.Background(), &Deployment{SHA: shaA}, &Post{Create: true}); a.Kind != Sent || hits2.Load() != 1 {
		t.Fatalf("TLS 1.2 must work: %+v", a)
	}
}

func TestProxyIsIgnored(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		w.WriteHeader(502)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	srv, pool, hits := tlsServer(t, tls.VersionTLS13)
	c := &Client{API: srv.URL, Repository: "owner/config-repo", Token: "t", HTTP: NewHTTPClient(pool)}
	if a := c.Post(context.Background(), &Deployment{SHA: shaA}, &Post{Create: true}); a.Kind != Sent || hits.Load() != 1 || proxied.Load() != 0 {
		t.Fatalf("the proxy must not be used: %+v, proxy %d", a, proxied.Load())
	}
}

func TestQueueModesUnderUmask022(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	base := testgit.PrivateDir(t)
	dir := filepath.Join(base, "github")
	if err := os.Mkdir(dir, 0o755); err != nil { // made before with a wide mode
		t.Fatal(err)
	}
	q, err := Open(dir, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	q.Start(RoleApply, shaA, "example-host", DescApplyStarted, time.Now())
	if err := q.Save(); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{dir: 0o700, File(dir): 0o600} {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %o", p, fi.Mode(), err, want)
		}
	}
	if err := CheckDir(dir, uint32(os.Geteuid())); err != nil {
		t.Fatal(err)
	}
	// Read back: a wrong mode is refused.
	if err := os.Chmod(File(dir), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, uint32(os.Geteuid())); err == nil {
		t.Fatal("a queue file that others can read must be refused")
	}
	if err := os.Chmod(File(dir), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(dir, uint32(os.Geteuid())); err == nil {
		t.Fatal("a queue folder with mode 0755 must be a problem")
	}
	// A link in place of the file is refused.
	_ = os.Remove(File(dir))
	if err := os.Symlink("/etc/passwd", File(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, uint32(os.Geteuid())); err == nil {
		t.Fatal("a link must be refused")
	}
}

func TestCreateAndStatusBodies(t *testing.T) {
	f := newTfix(t)
	k := f.q.Start(RoleApply, shaA, "example-host", DescApplyStarted, f.now)
	f.q.Status(k, Failure, DescHeld, f.now)
	f.send()
	calls := f.srv.Calls()
	if len(calls) != 3 {
		t.Fatalf("%d calls", len(calls))
	}
	cr := calls[0]
	if cr.Method != "POST" || cr.Path != "/repos/owner/config-repo/deployments" || cr.Auth != "Bearer test-token" {
		t.Fatalf("%+v", cr)
	}
	want := map[string]any{"ref": shaA, "task": "deploy", "auto_merge": false, "required_contexts": []any{}, "environment": "example-host",
		"production_environment": false, "transient_environment": false, "description": "pullgate apply"}
	if len(cr.Body) != len(want) {
		t.Fatalf("body %v", cr.Body)
	}
	for k, v := range want {
		if got, ok := cr.Body[k]; !ok || !equal(got, v) {
			t.Fatalf("%s: %v, want %v", k, got, v)
		}
	}
	if _, ok := cr.Body["payload"]; ok {
		t.Fatal("no payload")
	}
	st := calls[2]
	if !strings.HasPrefix(st.Path, "/repos/owner/config-repo/deployments/") || !strings.HasSuffix(st.Path, "/statuses") || st.Body["state"] != "failure" || st.Body["description"] != DescHeld || len(st.Body) != 2 {
		t.Fatalf("%+v", st)
	}
}

func equal(a, b any) bool {
	if x, ok := a.([]any); ok {
		y, ok := b.([]any)
		return ok && len(x) == 0 && len(y) == 0
	}
	return a == b
}

func TestNewestSeesADroppedAttempt(t *testing.T) {
	f := newTfix(t)
	f.q.Start(RoleApply, shaA, "example-host", DescApplyStarted, f.now)
	f.q.Status(1, Failure, DescHeld, f.now)
	k := f.q.Start(RoleApply, shaA, "example-host", DescApplyStarted, f.now)
	f.q.drop(f.q.find(k))
	if d := f.q.Newest(RoleApply, shaA, false); d == nil || d.Key != k {
		t.Fatalf("the newest attempt, also a dropped one: %+v", d)
	}
}

func TestQueuedDeploymentKeepsItsEnvironment(t *testing.T) {
	f := newTfix(t)
	f.q.Start(RoleApply, shaA, "old-env", DescApplyStarted, f.now)
	f.send() // the client has example-host
	if c := f.srv.Calls(); len(c) == 0 || c[0].Body["environment"] != "old-env" {
		t.Fatalf("%+v", c)
	}
}

func TestQueueIsBounded(t *testing.T) {
	f := newTfix(t)
	for i := 0; i < maxPosts; i++ {
		f.q.Start(RoleApply, shaA, "example-host", DescApplyStarted, f.now)
	}
	if len(f.q.Posts) > maxPosts+1 {
		t.Fatalf("%d posts", len(f.q.Posts))
	}
	if d := f.q.find(1); d != nil && !d.Dropped {
		t.Fatal("the oldest deployment must be dropped first")
	}
	if err := f.q.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(f.dir, uint32(os.Geteuid())); err != nil {
		t.Fatalf("a full queue must still be read: %v", err)
	}
}

// A 201 whose answer is cut off is tried again, not dropped.
func TestCutAnswerOfACreateIsTriedAgain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":`)
	}))
	defer srv.Close()
	c := &Client{API: srv.URL, Repository: "owner/config-repo", Token: "t", HTTP: NewHTTPClient(nil)}
	if a := c.Post(context.Background(), &Deployment{SHA: shaA}, &Post{Create: true}); a.Kind != Retry {
		t.Fatalf("%+v", a)
	}
}
