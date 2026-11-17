package message

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
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
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := esc("<b>&</b>"); got != "&lt;b&gt;&amp;&lt;/b&gt;" {
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
	if !strings.Contains(text, "⚠ The last apply (<code>"+Short(q.Held)+"</code>) failed.") {
		t.Error("the held SHA warning is missing")
	}
	if strings.Index(text, "The last apply") > strings.Index(text, "Apply this commit?") {
		t.Error("the held warning must be at the top")
	}
}

// hostile are values that a commit may hold to make Telegram show a link,
// a mention, a hashtag, a command or markup in a trusted message
// (code6x/pullgate#23, #29).
var hostile = []string{
	"README.md",
	"login.example.com",
	"docs.example.org/setup",
	"https://example.com/x",
	"<a href=\"https://example.com\">Apply</a>",
	"@username",
	"#tag",
	"/command",
	"\u202Edm.EMDAER",
	strings.Repeat("long.example.net ", 300),
}

// markers must never be seen outside <code> and <pre>.
var markers = []string{"README.md", "example.com", "example.org", "example.net", "a href", "@username", "#tag", "/command", "EMDAER"}

var codeOrPre = regexp.MustCompile(`(?s)<code>.*?</code>|<pre>.*?</pre>`)

// outsideCode returns what is left of text without its <code> and <pre>
// parts.
func outsideCode(text string) string { return codeOrPre.ReplaceAllString(text, "") }

// TestRepoTextIsOnlyInsideCode feeds hostile values into every field of the
// question that comes from the repo.
func TestRepoTextIsOnlyInsideCode(t *testing.T) {
	for _, failed := range []bool{false, true} {
		q := baseQuestion()
		q.Watch = []string{"**"}
		for _, h := range hostile {
			q.Subjects = append(q.Subjects, h)
			q.Files = append(q.Files, h)
			q.Symlinks = append(q.Symlinks, Symlink{Path: h, Target: h})
			q.Submodules = append(q.Submodules, h)
		}
		q.TotalCommits = len(q.Subjects)
		q.Summary = strings.Join(hostile, "\n")
		if failed {
			q.SummaryFailed = strings.Join(hostile, " ")
		}
		for _, cut := range []bool{false, true} {
			if cut {
				q.Files = append(q.Files, make([]string, 500)...) // force a cut
			}
			text := q.Text()
			if Len(text) > 4096 {
				t.Fatalf("too long for Telegram: %d", Len(text))
			}
			out := outsideCode(text)
			for _, m := range markers {
				if strings.Contains(out, m) {
					t.Errorf("failed=%v cut=%v: %q is outside <code>/<pre>:\n%s", failed, cut, m, out)
				}
			}
			if strings.ContainsRune(text, '‮') {
				t.Error("a right-to-left mark must not reach the message")
			}
		}
	}
	for _, h := range hostile {
		for _, s := range []string{Code(h), Output(h+"\n"+h, 10)} {
			if out := outsideCode(s); out != "" {
				t.Errorf("Code/Output leave text outside: %q", out)
			}
		}
	}
}

// TestShortSHAsAndOneFullSHA: the question shows short SHAs, and the full
// SHA of the commit once, on the last line (code6x/pullgate#30).
func TestShortSHAsAndOneFullSHA(t *testing.T) {
	q := baseQuestion()
	q.Held = strings.Repeat("c", 40)
	q.Files = make([]string, 2000) // the last line stays after a cut
	text := q.Text()
	if strings.Count(text, q.SHA) != 1 || !strings.HasSuffix(text, "\nFull: <code>"+q.SHA+"</code>") {
		t.Fatalf("the full SHA must be on the last line, once:\n%s", text)
	}
	for _, s := range []string{q.Applied, q.Held} {
		if strings.Contains(text, s) || !strings.Contains(text, "<code>"+Short(s)+"</code>") {
			t.Errorf("only the short form of %s must be shown", Short(s))
		}
	}
	if !strings.Contains(text, "Apply this commit?\n<code>"+Short(q.SHA)+"</code>") {
		t.Error("the short SHA must follow the question line")
	}
	if Len(text) > Limit {
		t.Fatalf("too long: %d", Len(text))
	}
}

// TestOutputKeepsTheEnd: command output keeps its last lines, and a long
// line keeps its start and its end (code6x/pullgate#25).
func TestOutputKeepsTheEnd(t *testing.T) {
	if Output("a\nb", 0) != "" || Output("\n\n", 10) != "" {
		t.Error("no lines, no block")
	}
	long := "start " + strings.Repeat("ж", 1000) + " the \"reason\"\x07"
	got := Output(strings.Repeat("noise\n", 50)+long, 10)
	if !strings.HasSuffix(got, " the &#34;reason&#34;?</pre>") || !strings.Contains(got, "start жж") || !strings.Contains(got, "…") {
		t.Fatalf("the long line must keep its start and end, cleaned and escaped: %q", got)
	}
	if !utf8.ValidString(got) || strings.Count(got, "noise") != 9 {
		t.Fatalf("want valid text and the last 10 lines: %q", got)
	}
	big := Output(strings.Repeat(strings.Repeat("x", 299)+"\n", 20)+"last", 20)
	if !strings.HasSuffix(big, "\nlast</pre>") || utf8.RuneCountInString(big) > OutputMax+20 {
		t.Fatalf("output over OutputMax keeps its last line and stays short: %d", utf8.RuneCountInString(big))
	}
}
