package gate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/code6x/pullgate/internal/message"
	"github.com/code6x/pullgate/internal/spool"
)

var codeOrPre = regexp.MustCompile(`(?s)<code>.*?</code>|<pre>.*?</pre>`)

// repoMarkers are parts of the hostile values below. None may be seen
// outside <code> and <pre> in any message (code6x/pullgate#29).
var repoMarkers = []string{"README.md", "example.com", "example.org", "a href", "@username", "#tag", "/command"}

const hostileLine = `README.md login.example.com https://example.com/x docs.example.org/setup <a href="https://example.com">Apply</a> @username #tag /command`

func checkRepoText(t *testing.T, ms []spool.Message) {
	t.Helper()
	for _, m := range ms {
		if message.Len(m.Text) > 4096 {
			t.Errorf("%s message too long: %d", m.Kind, message.Len(m.Text))
		}
		out := codeOrPre.ReplaceAllString(m.Text, "")
		for _, k := range repoMarkers {
			if strings.Contains(out, k) {
				t.Errorf("%s message holds %q outside <code>/<pre>:\n%s", m.Kind, k, m.Text)
			}
		}
	}
}

// TestHostileCommitTextIsOnlyCode runs a commit with hostile file names, a
// hostile subject and hostile command output through a failed check, a
// question, and a failed apply, and checks every message.
func TestHostileCommitTextIsOnlyCode(t *testing.T) {
	f := newFixture(t, ``, "")
	files := map[string]string{"README.md": "x", "login.example.com": "x", "policy/@username.toml": "x", "#tag": "x"}
	// 1. A check that fails with hostile output.
	f.script("check.sh", "echo '"+hostileLine+"'\necho '"+hostileLine+"' >&2\nexit 1\n")
	f.commit("first "+hostileLine, "one", files)
	if r := f.run(); r != CheckFailed {
		t.Fatalf("run: %s", r)
	}
	ms := f.outbox()
	if !contains(ms, "<pre>README.md login.example.com") {
		t.Fatalf("the notice must show the check output: %+v", ms)
	}
	checkRepoText(t, ms)
	// 2. A question with a hostile summary.
	f.script("check.sh", "true\n")
	f.script("summary.sh", "echo '"+hostileLine+"'\n")
	f.commit("second "+hostileLine, "two", map[string]string{"fail-apply": "x", "docs.example.org": "x"})
	if r := f.run(); r != Asked {
		t.Fatalf("run: %s", r)
	}
	ms = f.outbox()
	checkRepoText(t, ms)
	var q spool.Message
	for _, m := range ms {
		if m.Kind == spool.KindAsk {
			q = m
		}
	}
	for _, want := range []string{"<code>README.md</code>", "<code>login.example.com</code>", "<code>second " + strings.ReplaceAll(strings.ReplaceAll(hostileLine, "<", "&lt;"), ">", "&gt;")[:60]} {
		if !strings.Contains(q.Text, want) {
			t.Errorf("missing %q in the question:\n%s", want, q.Text)
		}
	}
	// 3. A failed apply whose command prints hostile text.
	f.script("apply.sh", "echo '"+hostileLine+"'\necho '"+hostileLine+"' >&2\nexit 2\n")
	f.tap(q.Code, "apply")
	if r := f.apply(); r != Held {
		t.Fatalf("apply: %s", r)
	}
	ms = f.outbox()
	if !contains(ms, "failed") {
		t.Fatalf("no failure report: %+v", ms)
	}
	checkRepoText(t, ms)
}

// TestHostileSymlinkRefusalIsOnlyCode: the refused notice names a path
// from the commit; it is inside <code> too.
func TestHostileSymlinkRefusalIsOnlyCode(t *testing.T) {
	f := newFixture(t, ``, "[limits]\nrefuse_symlinks = true\n")
	name := "login.example.com @username #tag"
	if err := os.Symlink("../../x", filepath.Join(f.r.Work, name)); err != nil {
		t.Fatal(err)
	}
	f.commit("link", "one", nil)
	if r := f.run(); r != CheckFailed {
		t.Fatalf("run: %s", r)
	}
	ms := f.outbox()
	if !contains(ms, "login.example.com") {
		t.Fatalf("the notice must name the path: %+v", ms)
	}
	checkRepoText(t, ms)
}

// TestGateBuildsNoRepoTextItself: the gate code makes no HTML from text by
// itself; it uses message.Code, message.SHA and message.Output, which put
// the text in <code> or <pre> (code6x/pullgate#29).
func TestGateBuildsNoRepoTextItself(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		// The listener's own messages hold only instance names from its
		// config, never repo text; root's messages are built here.
		for _, bad := range []string{"html.EscapeString", "<pre>", "\"<code>\"", "\"<code>\" +", "Sprintf(\"<code>"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %q: put repo text in a message only with message.Code, message.SHA or message.Output", n, bad)
			}
		}
	}
}

// TestNoticesShowShortSHAs: notices and replies show 12 characters of a
// SHA, and an apply still installs the full SHA (code6x/pullgate#30).
func TestNoticesShowShortSHAs(t *testing.T) {
	f := newFixture(t, ``, "")
	a := f.commit("first", "one", nil)
	f.run()
	q := f.ask()
	if strings.Count(q.Text, a) != 1 || !strings.Contains(q.Text, "\nFull: <code>"+a+"</code>") {
		t.Fatalf("the question shows the full SHA once:\n%s", q.Text)
	}
	f.tap(q.Code, "apply")
	if r := f.apply(); r != Applied || f.state().Applied != a || !strings.Contains(f.read("log"), "apply "+a) {
		t.Fatalf("the apply must use the full SHA: %s %q", r, f.read("log"))
	}
	ms := f.outbox()
	if contains(ms, a) || !contains(ms, "<code>"+message.Short(a)+"</code>") {
		t.Fatalf("the report must show only the short SHA: %+v", ms)
	}
}
