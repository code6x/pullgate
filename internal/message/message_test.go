package message

import (
	"fmt"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                   "plain",
		"a\nb":                    "a?b",
		"bidi\u202eevil":          "bidi?evil",
		"zero\u200bwidth":         "zero?width",
		"tab\there":               "tab?here",
		"esc\x1b[31m":             "esc?[31m",
		string([]byte{0xff, 'x'}): "?x",
		strings.Repeat("x", 300):  strings.Repeat("x", 200) + "…",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Esc("<b>&</b>"); got != "&lt;b&gt;&amp;&lt;/b&gt;" {
		t.Errorf("Esc: %q", got)
	}
}

func TestMatch(t *testing.T) {
	yes := [][2]string{
		{"policy/**", "policy/a.toml"}, {"policy/**", "policy/x/y"}, {"*.toml", "a/b.toml"},
		{"*.toml", "c.toml"}, {"etc/*/conf", "etc/x/conf"}, {"**/units/*.service", "a/b/units/x.service"},
		{"**", "anything/at/all"}, {"README.md", "README.md"},
	}
	no := [][2]string{
		{"policy/**", "other/a"}, {"*.toml", "a/b.yaml"}, {"etc/*/conf", "etc/x/y/conf"},
		{"a/b", "a/b/c"}, {"[", "x"},
	}
	for _, c := range yes {
		if !Match(c[0], c[1]) {
			t.Errorf("%q must match %q", c[0], c[1])
		}
	}
	for _, c := range no {
		if Match(c[0], c[1]) {
			t.Errorf("%q must not match %q", c[0], c[1])
		}
	}
}

func baseQuestion() *Question {
	return &Question{
		Host: "example-host", Instance: "config-repo",
		SHA:     strings.Repeat("a", 40),
		Applied: strings.Repeat("b", 40),
		Watch:   []string{"policy/**"},
	}
}

func TestWatchWarningsAppear(t *testing.T) {
	q := baseQuestion()
	q.Files = []string{"README.md", "policy/main.toml", "policy/<x>.toml"}
	q.TotalCommits = 1
	q.Subjects = []string{"change policy"}
	text := q.Text()
	for _, want := range []string{
		"⚠ Watched path changed: <code>policy/main.toml</code>",
		"⚠ Watched path changed: <code>policy/&lt;x&gt;.toml</code>",
		"<code>" + q.SHA + "</code>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "Watched path changed: <code>README.md") {
		t.Error("README.md is not watched")
	}
	// The warnings come before the lists.
	if strings.Index(text, "⚠ Watched") > strings.Index(text, "Changed files") {
		t.Error("the warnings must come before the file list")
	}
}

func TestSummaryIsCutToFit(t *testing.T) {
	q := baseQuestion()
	for i := 0; i < 2000; i++ {
		q.Files = append(q.Files, fmt.Sprintf("dir/file-%04d.txt", i))
	}
	q.TotalCommits = 60
	for i := 0; i < 50; i++ {
		q.Subjects = append(q.Subjects, strings.Repeat("<&>", 100))
	}
	q.Summary = strings.Repeat("line of summary <>\n", 500)
	text := q.Text()
	if n := Len(text); n > 4096 || n > Limit {
		t.Fatalf("the message has %d characters", n)
	}
	if !strings.Contains(text, "more lines left out") {
		t.Fatal("the cut must say how much was left out")
	}
	if !strings.Contains(text, q.SHA) || !strings.HasPrefix(text, "pullgate on <b>example-host</b>") {
		t.Fatal("the head must stay")
	}
	// No broken entity or tag at the cut.
	if strings.Count(text, "<code>") != strings.Count(text, "</code>") {
		t.Fatal("a <code> tag is not closed")
	}
}

func TestSummaryPreClosedAtCut(t *testing.T) {
	q := baseQuestion()
	q.Files = []string{"a"}
	q.Summary = strings.Repeat("x\n", 3000)
	text := q.Text()
	if strings.Count(text, "<pre>") != strings.Count(text, "</pre>") {
		t.Fatalf("the <pre> block must be closed after a cut:\n%s", text[len(text)-200:])
	}
	if Len(text) > Limit {
		t.Fatal("too long")
	}
}

func TestManyWatchWarningsStillFit(t *testing.T) {
	q := baseQuestion()
	for i := 0; i < 500; i++ {
		q.Files = append(q.Files, fmt.Sprintf("policy/%03d-%s", i, strings.Repeat("y", 150)))
	}
	text := q.Text()
	if Len(text) > Limit {
		t.Fatalf("too long: %d", Len(text))
	}
	if !strings.Contains(text, "more warnings") || !strings.Contains(text, q.SHA) {
		t.Fatal("the head must keep the SHA and say how many warnings were left out")
	}
}

func TestFirstApplyAndHeld(t *testing.T) {
	q := baseQuestion()
	q.Applied = ""
	q.Held = strings.Repeat("c", 40)
	q.Files = []string{"a", "b"}
	text := q.Text()
	if !strings.Contains(text, "First apply: there is no applied commit yet. All 2 files are listed.") {
		t.Error("the first question must say so")
	}
	if !strings.Contains(text, "⚠ The last apply (<code>"+q.Held+"</code>) failed.") {
		t.Error("the held SHA warning is missing")
	}
	if strings.Index(text, "The last apply") > strings.Index(text, "Apply this commit?") {
		t.Error("the held warning must be at the top")
	}
}
