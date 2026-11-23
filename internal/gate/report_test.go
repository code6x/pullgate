package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code6x/pullgate/internal/spool"
)

// Tests of code6x/pullgate#42: the report file of apply.

func replies(ms []spool.Message) string {
	var b strings.Builder
	for _, m := range ms {
		if m.Kind == spool.KindReply {
			b.WriteString(m.Text + "\n")
		}
	}
	return b.String()
}

func (f *fixture) askAndApply(subject, value string, more map[string]string) (Result, string) {
	f.t.Helper()
	f.commit(subject, value, more)
	if r := f.run(); r != Sent {
		f.t.Fatalf("run: %s", r)
	}
	f.tap(f.ask().Code, "apply")
	r := f.apply()
	return r, replies(f.outbox())
}

func TestApplyReportTwoLines(t *testing.T) {
	f := newFixture(t, ``, "")
	r, text := f.askAndApply("first", "one", map[string]string{"report": "skipped step x on purpose\nall else done <b>\n"})
	if r != Applied {
		t.Fatalf("apply: %s", r)
	}
	for _, want := range []string{"✅ Applied", "Report of apply:\n<code>skipped step x on purpose</code>\n<code>all else done &lt;b&gt;</code>"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "more lines") {
		t.Error("no count for 2 lines")
	}
	env := f.read("env")
	if !strings.Contains(env, "report="+f.env.Dir.Trees()) || !strings.Contains(env, "health report=\n") && !strings.HasSuffix(env, "health report=") {
		t.Fatalf("apply gets the report file and health does not:\n%s", env)
	}
}

func TestApplyReportFifteenLinesShowTen(t *testing.T) {
	f := newFixture(t, ``, "")
	var lines []string
	for i := 1; i <= 15; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	_, text := f.askAndApply("first", "one", map[string]string{"report": strings.Join(lines, "\n") + "\n"})
	if n := strings.Count(text, "<code>line "); n != 10 {
		t.Fatalf("%d lines shown, want 10:\n%s", n, text)
	}
	if !strings.Contains(text, "<code>line 10</code>") || strings.Contains(text, "line 11") || !strings.Contains(text, "… 5 more lines in the report") {
		t.Fatalf("the first 10 and a count:\n%s", text)
	}
}

func TestApplyReportInTheFailedMessage(t *testing.T) {
	f := newFixture(t, ``, "")
	r, text := f.askAndApply("first", "one", map[string]string{"report": "step 3 broke", "fail-apply": "1"})
	if r != Held {
		t.Fatalf("apply: %s", r)
	}
	if !strings.Contains(text, "❌") || !strings.Contains(text, "<code>step 3 broke</code>") {
		t.Fatalf("the failed message has the report:\n%s", text)
	}
}

func TestApplyWithoutReportIsUnchanged(t *testing.T) {
	f := newFixture(t, ``, "")
	_, text := f.askAndApply("first", "one", nil)
	if strings.Contains(text, "Report") || !strings.Contains(text, "✅ Applied") {
		t.Fatalf("no report, no report lines:\n%s", text)
	}
}

func TestApplyReportLinkIsNotFollowed(t *testing.T) {
	f := newFixture(t, ``, "")
	if err := os.WriteFile(filepath.Join(f.ctrl, "secret"), []byte("secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, text := f.askAndApply("first", "one", map[string]string{"report-link": "1"})
	if r != Applied {
		t.Fatalf("apply: %s", r)
	}
	if strings.Contains(text, "secret-value") || !strings.Contains(text, "could not be read") {
		t.Fatalf("a link as the report file is not read:\n%s", text)
	}
}

func TestApplyReportHardLinkIsNotRead(t *testing.T) {
	f := newFixture(t, ``, "")
	if err := os.WriteFile(filepath.Join(f.ctrl, "secret"), []byte("secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, text := f.askAndApply("first", "one", map[string]string{"report-hardlink": "1"})
	if r != Applied {
		t.Fatalf("apply: %s", r)
	}
	if strings.Contains(text, "secret-value") || !strings.Contains(text, "not the file that pullgate made") {
		t.Fatalf("a hard link as the report file is not read:\n%s", text)
	}
}

func TestApplyReportHostileText(t *testing.T) {
	f := newFixture(t, ``, "")
	_, text := f.askAndApply("first", "one", map[string]string{"report": "<a href=\"https://example.com\">x</a>\n\u202edm.EMDAER\n" + strings.Repeat("&", 5000) + "\n"})
	if strings.Contains(text, "<a href") || strings.ContainsRune(text, '\u202e') {
		t.Fatalf("report text is cleaned code:\n%s", text)
	}
	if len([]rune(text)) > 3000 {
		t.Fatalf("the report is cut: %d", len([]rune(text)))
	}
}
