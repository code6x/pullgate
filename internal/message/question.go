package message

import (
	"fmt"
	"html"
	"strings"
)

// Symlink is a symlink of the tree that points out of it.
type Symlink struct{ Path, Target string }

// Question holds what the question shows.
type Question struct {
	Host, Instance string
	SHA, Applied   string // Applied is "" on the first apply
	Held           string // the held SHA, if any
	Broken         string // why the instance is broken, if it is (DESIGN.md section 10)
	NotDescendant  bool   // with allow_older
	Subjects       []string
	TotalCommits   int
	Files          []string
	Watch          []string // the watch patterns
	Symlinks       []Symlink
	Submodules     []string
	Summary        string // the output of the summary command
	SummaryFailed  string // why the summary command failed, if it did
}

// Head is the first line of every message.
func Head(host, instance string) string {
	return fmt.Sprintf("pullgate on <b>%s</b>, instance <b>%s</b>", html.EscapeString(host), html.EscapeString(instance))
}

// Short is the first 12 characters of a SHA, for reports.
func Short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Text builds the question text, cut to fit one message.
func (q *Question) Text() string {
	head := []string{Head(q.Host, q.Instance)}
	if q.Broken != "" {
		head = append(head, "⚠ The instance is broken: an apply failed and pullgate could not roll it back. The host may be in a half state. This commit is a fix forward.",
			"Reason: "+BrokenReason(q.Broken))
	}
	if q.Held != "" {
		head = append(head, "⚠ The last apply ("+SHA(q.Held)+") failed.")
	}
	if q.NotDescendant {
		head = append(head, "⚠ This commit is not a descendant of the applied commit.")
	}
	head = append(head, "Apply this commit?", SHA(q.SHA))
	if q.Applied == "" {
		head = append(head, fmt.Sprintf("First apply: there is no applied commit yet. All %d files are listed.", len(q.Files)))
	} else {
		head = append(head, "Since "+SHA(q.Applied)+":")
	}
	var watched, other []string
	for _, f := range Watched(q.Watch, q.Files) {
		watched = append(watched, "⚠ Watched path changed: "+Code(f))
	}
	for _, s := range q.Symlinks {
		other = append(other, "⚠ Symlink out of the tree: "+Code(s.Path)+" → "+Code(s.Target))
	}
	for _, s := range q.Submodules {
		other = append(other, "⚠ Submodule skipped: "+Code(s))
	}
	var warns []string
	summary := q.Summary
	if q.SummaryFailed == "" {
		warns, summary = SplitWarnings(q.Summary)
	}
	head = buildHead(head, watched, warns, other)
	var body []string
	body = append(body, "", fmt.Sprintf("Commits (%d):", q.TotalCommits))
	for _, s := range q.Subjects {
		body = append(body, "• "+Code(s))
	}
	if q.TotalCommits > len(q.Subjects) {
		body = append(body, fmt.Sprintf("… and %d more", q.TotalCommits-len(q.Subjects)))
	}
	body = append(body, fmt.Sprintf("Changed files (%d):", len(q.Files)))
	for _, f := range q.Files {
		body = append(body, "• "+Code(f))
	}
	pre := -1
	switch {
	case q.SummaryFailed != "":
		body = append(body, "⚠ The summary command failed: "+Code(q.SummaryFailed))
	case strings.TrimSpace(summary) != "":
		body = append(body, "Summary:")
		pre = len(body)
		lines := strings.Split(strings.TrimRight(summary, "\n"), "\n")
		body = append(body, "<pre>"+esc(lines[0]))
		for _, l := range lines[1:] {
			body = append(body, esc(l))
		}
		body = append(body, "</pre>")
	}
	// The full SHA once, at the end, to copy and compare. The tap is bound
	// to the question, not to this text.
	return Fit(head, body, "Full: "+Code(q.SHA), pre)
}

// WarnPrefix starts a line of the summary output that is shown in the
// head of the question, not in the summary block (DESIGN.md section 12).
const WarnPrefix = "WARN: "

// MaxWarnings is the most summary warnings shown; more give one line
// that counts them.
const MaxWarnings = 10

// maxWarnLen is the most UTF-16 units of one summary warning line, after
// the escape. 200 plain characters fit; text that the escape makes longer
// is cut. So 10 of them take at most about 2250 units, and the watched
// paths, root's own warnings, always keep room in the head.
const maxWarnLen = 220

// SplitWarnings takes the lines that start with WarnPrefix out of the
// summary output. It returns their text (without the prefix) and the
// rest of the output.
func SplitWarnings(summary string) (warns []string, rest string) {
	if !strings.Contains(summary, WarnPrefix) {
		return nil, summary
	}
	var keep []string
	for _, l := range strings.Split(summary, "\n") {
		if t, ok := strings.CutPrefix(l, WarnPrefix); ok {
			// A warning with no text is dropped; a CRLF line ending is
			// not part of the text.
			if t = strings.TrimRight(t, "\r"); strings.TrimSpace(t) != "" {
				warns = append(warns, t)
			}
			continue
		}
		keep = append(keep, l)
	}
	return warns, strings.Join(keep, "\n")
}

// warnLine is one summary warning in the head: cleaned, as code, and cut
// so that its escaped text is at most maxWarnLen.
func warnLine(s string) string {
	l := "⚠ " + Code(s)
	r := []rune(s)
	for n := len(r); Len(l) > maxWarnLen && n > 0; {
		n = n * 3 / 4
		l = "⚠ " + Code(string(r[:n])+"…")
	}
	return l
}

// buildHead adds the warnings to the fixed lines of the head. The
// summary warnings come after the watched paths and are never cut for
// the lists or for other warnings. Only when the fixed lines and the
// summary warnings alone do not fit are the last summary warnings counted
// instead. Path warnings are kept while they fit, and the rest counted.
func buildHead(fixed, watched, warns, other []string) []string {
	var sum []string
	more := 0
	for i, w := range warns {
		if i >= MaxWarnings {
			more = len(warns) - MaxWarnings
			break
		}
		sum = append(sum, warnLine(w))
	}
	moreLine := func() []string {
		if more == 0 {
			return nil
		}
		return []string{fmt.Sprintf("⚠ %d more warnings from the summary", more)}
	}
	all := append(append(append(append(append([]string{}, fixed...), watched...), sum...), moreLine()...), other...)
	if Len(strings.Join(all, "\n")) <= Limit-200 {
		return all
	}
	size := func(ls []string) int {
		n := 0
		for _, l := range ls {
			n += Len(l) + 1
		}
		return n
	}
	used := size(fixed)
	// The summary warnings get the whole room of the head (Limit-200);
	// the path warnings only what is left within Limit-400.
	for len(sum) > 0 && used+size(sum)+size(moreLine()) > Limit-200 {
		sum = sum[:len(sum)-1]
		more++
	}
	used += size(sum) + size(moreLine())
	left := 0
	keep := func(ls []string) []string {
		var out []string
		for _, l := range ls {
			if used+Len(l)+1 > Limit-400 {
				left++
				continue
			}
			out = append(out, l)
			used += Len(l) + 1
		}
		return out
	}
	kw, ko := keep(watched), keep(other)
	out := append(append(append(append(append([]string{}, fixed...), kw...), sum...), moreLine()...), ko...)
	if left > 0 {
		out = append(out, fmt.Sprintf("⚠ … and %d more warnings", left))
	}
	return out
}
