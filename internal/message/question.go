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
	if q.Held != "" {
		head = append(head, fmt.Sprintf("⚠ The last apply (<code>%s</code>) failed.", q.Held))
	}
	if q.NotDescendant {
		head = append(head, "⚠ This commit is not a descendant of the applied commit.")
	}
	head = append(head, "Apply this commit?", "<code>"+q.SHA+"</code>")
	if q.Applied == "" {
		head = append(head, fmt.Sprintf("First apply: there is no applied commit yet. All %d files are listed.", len(q.Files)))
	} else {
		head = append(head, "Since <code>"+q.Applied+"</code>:")
	}
	for _, f := range Watched(q.Watch, q.Files) {
		head = append(head, "⚠ Watched path changed: <code>"+Esc(f)+"</code>")
	}
	for _, s := range q.Symlinks {
		head = append(head, "⚠ Symlink out of the tree: <code>"+Esc(s.Path)+"</code> → <code>"+Esc(s.Target)+"</code>")
	}
	for _, s := range q.Submodules {
		head = append(head, "⚠ Submodule skipped: <code>"+Esc(s)+"</code>")
	}
	// The head holds text from the commit too (watched paths); keep it
	// within the limit on its own.
	if Len(strings.Join(head, "\n")) > Limit-200 {
		head = cutHead(head)
	}
	var body []string
	body = append(body, "", fmt.Sprintf("Commits (%d):", q.TotalCommits))
	for _, s := range q.Subjects {
		body = append(body, "• "+Esc(s))
	}
	if q.TotalCommits > len(q.Subjects) {
		body = append(body, fmt.Sprintf("… and %d more", q.TotalCommits-len(q.Subjects)))
	}
	body = append(body, fmt.Sprintf("Changed files (%d):", len(q.Files)))
	for _, f := range q.Files {
		body = append(body, "• "+Esc(f))
	}
	pre := -1
	switch {
	case q.SummaryFailed != "":
		body = append(body, "⚠ The summary command failed: "+Esc(q.SummaryFailed))
	case strings.TrimSpace(q.Summary) != "":
		body = append(body, "Summary:")
		pre = len(body)
		lines := strings.Split(strings.TrimRight(q.Summary, "\n"), "\n")
		body = append(body, "<pre>"+Esc(lines[0]))
		for _, l := range lines[1:] {
			body = append(body, Esc(l))
		}
		body = append(body, "</pre>")
	}
	return Fit(head, body, pre)
}

// cutHead keeps the fixed lines and as many warnings as fit.
func cutHead(head []string) []string {
	var out []string
	used := 0
	left := 0
	for _, l := range head {
		if strings.HasPrefix(l, "⚠ Watched") || strings.HasPrefix(l, "⚠ Symlink") || strings.HasPrefix(l, "⚠ Submodule") {
			if used+Len(l)+1 > Limit-400 {
				left++
				continue
			}
		}
		out = append(out, l)
		used += Len(l) + 1
	}
	if left > 0 {
		out = append(out, fmt.Sprintf("⚠ … and %d more warnings", left))
	}
	return out
}
