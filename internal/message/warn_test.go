package message

import (
	"fmt"
	"strings"
	"testing"
)

// Tests of code6x/pullgate#42: WARN: lines of the summary in the head.

func headOf(text string) string {
	i := strings.Index(text, "\nCommits (")
	if i < 0 {
		return text
	}
	return text[:i]
}

func TestSummaryWarningsStayInTheHeadOfALongQuestion(t *testing.T) {
	q := baseQuestion()
	for i := 0; i < 500; i++ {
		q.Files = append(q.Files, fmt.Sprintf("dir/file-%04d.txt", i))
	}
	q.TotalCommits = 1
	q.Subjects = []string{"many files"}
	q.Summary = "line one\nWARN: sudoers changed\nline two\nWARN: login group changed\n" + strings.Repeat("more summary\n", 200)
	text := q.Text()
	if Len(text) > Limit {
		t.Fatalf("too long: %d", Len(text))
	}
	head := headOf(text)
	for _, w := range []string{"⚠ <code>sudoers changed</code>", "⚠ <code>login group changed</code>"} {
		if !strings.Contains(head, w) {
			t.Errorf("missing %q in the head:\n%s", w, head)
		}
	}
	if strings.Count(text, "sudoers changed") != 1 || strings.Contains(text, "WARN:") {
		t.Error("a warning line is taken out of the summary block")
	}
	if !strings.Contains(text, "more lines left out") || strings.Contains(text, "dir/file-0499.txt") {
		t.Error("the file list must be cut")
	}
	if !strings.HasSuffix(text, "Full: <code>"+q.SHA+"</code>") {
		t.Error("the Full: line stays last")
	}
}

func TestSummaryWarningsComeAfterWatchedPaths(t *testing.T) {
	q := baseQuestion()
	q.Files = []string{"policy/a.toml"}
	q.Symlinks = []Symlink{{Path: "link", Target: "/etc"}}
	q.Summary = "WARN: x\n"
	text := q.Text()
	w, s, l := strings.Index(text, "⚠ Watched"), strings.Index(text, "⚠ <code>x</code>"), strings.Index(text, "⚠ Symlink")
	if w < 0 || s < w || l < s {
		t.Fatalf("order: watched, summary warnings, symlinks:\n%s", text)
	}
	if strings.Contains(text, "Summary:") {
		t.Error("a summary of only warnings has no summary block")
	}
}

func TestSummaryWarningIsOnlyCleanedCode(t *testing.T) {
	q := baseQuestion()
	q.Files = []string{"a"}
	for _, h := range hostile {
		q.Summary += WarnPrefix + h + "\n"
	}
	text := q.Text()
	out := outsideCode(text)
	for _, m := range markers {
		if strings.Contains(out, m) {
			t.Errorf("%q is outside <code>:\n%s", m, out)
		}
	}
	if strings.ContainsRune(text, '\u202e') || strings.Contains(text, "<a href") {
		t.Error("a right-to-left mark or HTML must not reach the message")
	}
	if !strings.Contains(headOf(text), "⚠ <code>&lt;a href=&#34;https://example.com&#34;&gt;Apply&lt;/a&gt;</code>") {
		t.Errorf("the HTML warning is shown escaped, as code:\n%s", headOf(text))
	}
	if !strings.Contains(headOf(text), "⚠ <code>?dm.EMDAER</code>") {
		t.Error("the right-to-left mark becomes ?")
	}
}

func TestTwelveSummaryWarningsShowTenAndACount(t *testing.T) {
	q := baseQuestion()
	q.Files = []string{"a"}
	for i := 1; i <= 12; i++ {
		q.Summary += fmt.Sprintf("WARN: warning %d\n", i)
	}
	text := q.Text()
	if n := strings.Count(text, "⚠ <code>warning "); n != 10 {
		t.Fatalf("%d warnings shown, want 10:\n%s", n, text)
	}
	if !strings.Contains(text, "⚠ <code>warning 10</code>") || strings.Contains(text, "warning 11") {
		t.Error("the first 10 are shown")
	}
	if !strings.Contains(headOf(text), "⚠ 2 more warnings from the summary") {
		t.Errorf("missing the count:\n%s", text)
	}
}

func TestLongSummaryWarningsAlwaysFit(t *testing.T) {
	q := baseQuestion()
	q.Broken = strings.Repeat("&", 900)
	for i := 0; i < 500; i++ {
		q.Files = append(q.Files, fmt.Sprintf("policy/%03d-%s", i, strings.Repeat("y", 150)))
	}
	for i := 0; i < 12; i++ {
		q.Summary += WarnPrefix + strings.Repeat("&\"<", 100) + "\n"
	}
	text := q.Text()
	if Len(text) > Limit {
		t.Fatalf("too long: %d", Len(text))
	}
	if strings.Count(text, "<code>") != strings.Count(text, "</code>") {
		t.Fatal("a <code> tag is not closed")
	}
	if !strings.Contains(text, "more warnings from the summary") || !strings.Contains(text, "Full: <code>"+q.SHA) {
		t.Fatalf("the count and the Full: line stay:\n%s", text)
	}
}

func TestSummaryWithoutWarningsIsUnchanged(t *testing.T) {
	// The text of 0.2.0 for the same question, built by hand.
	q := baseQuestion()
	q.Files = []string{"policy/a.toml", "b"}
	q.TotalCommits = 1
	q.Subjects = []string{"one"}
	q.Summary = "value 1\n  WARN: not at the start\nwarn: lower case\n"
	want := "pullgate on <b>example-host</b>, instance <b>config-repo</b>\n" +
		"Apply this commit?\n<code>aaaaaaaaaaaa</code>\nSince <code>bbbbbbbbbbbb</code>:\n" +
		"⚠ Watched path changed: <code>policy/a.toml</code>\n" +
		"\nCommits (1):\n• <code>one</code>\nChanged files (2):\n• <code>policy/a.toml</code>\n• <code>b</code>\n" +
		"Summary:\n<pre>value 1\n  WARN: not at the start\nwarn: lower case\n</pre>\n" +
		"Full: <code>" + q.SHA + "</code>"
	if got := q.Text(); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// Many path warnings and no summary warnings: as in 0.2.0.
	q = baseQuestion()
	for i := 0; i < 500; i++ {
		q.Files = append(q.Files, fmt.Sprintf("policy/%03d-%s", i, strings.Repeat("y", 150)))
	}
	if text := q.Text(); strings.Contains(text, "from the summary") || !strings.Contains(text, "⚠ … and") {
		t.Fatal("path warnings are cut as before")
	}
}

func TestSummaryWarningCRLFAndEmpty(t *testing.T) {
	q := baseQuestion()
	q.Files = []string{"a"}
	q.Summary = "WARN: crlf line\r\nWARN: \nWARN:    \nnormal\n"
	text := q.Text()
	if !strings.Contains(text, "⚠ <code>crlf line</code>") {
		t.Fatalf("a CRLF ending is not part of the warning:\n%s", text)
	}
	if strings.Contains(text, "⚠ <code></code>") || strings.Contains(text, "⚠ <code>   </code>") || strings.Contains(text, "WARN:") {
		t.Fatalf("an empty warning is dropped:\n%s", text)
	}
}

func TestTenLongSummaryWarningsAllStayWithAShortHead(t *testing.T) {
	q := baseQuestion()
	for i := 0; i < 500; i++ {
		q.Files = append(q.Files, fmt.Sprintf("policy/%03d-%s", i, strings.Repeat("y", 150)))
	}
	for i := 0; i < 10; i++ {
		q.Summary += WarnPrefix + fmt.Sprintf("%d", i) + strings.Repeat("&", 199) + "\n"
	}
	text := q.Text()
	if Len(text) > Limit {
		t.Fatalf("too long: %d", Len(text))
	}
	if strings.Count(text, "⚠ <code>") != 10 || strings.Contains(text, "from the summary") {
		t.Fatalf("all 10 long warnings stay:\n%s", text)
	}
}

func TestWatchedPathStaysWithTenLongSummaryWarnings(t *testing.T) {
	q := baseQuestion()
	q.Files = []string{"policy/sudoers"}
	for i := 0; i < 10; i++ {
		q.Summary += WarnPrefix + strings.Repeat("<", 200) + "\n"
	}
	text := q.Text()
	if !strings.Contains(headOf(text), "⚠ Watched path changed: <code>policy/sudoers</code>") {
		t.Fatalf("the watched path must stay:\n%s", headOf(text))
	}
	if strings.Count(text, "⚠ <code>&lt;") != 10 || Len(text) > Limit {
		t.Fatal("all 10 summary warnings stay, cut, and the message fits")
	}
}
