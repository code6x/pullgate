// Package message builds the text of the Telegram messages (DESIGN.md
// section 12): it cleans text from the commit, matches the watch list, and
// cuts the question to fit one message.
package message

import (
	"fmt"
	"html"
	"path"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Limit is the most characters (UTF-16 units) of one message. Telegram
// allows 4096; the rest is room for the cut line.
const Limit = 3800

// MaxLine is the most characters of one cleaned line.
const MaxLine = 200

// clean makes text from the commit safe to show: control and invisible
// characters become "?", and the text is cut to MaxLine characters. It
// does not escape HTML; esc does.
func clean(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= MaxLine {
			b.WriteString("…")
			break
		}
		if r == utf8.RuneError || unicode.IsControl(r) || isInvisible(r) {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// isInvisible reports characters that change how text looks or reads
// without being seen: format characters (bidi controls, zero-width), and
// odd spaces.
func isInvisible(r rune) bool {
	if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
		return true
	}
	if unicode.IsSpace(r) && r != ' ' {
		return true
	}
	switch r {
	case 'ᅟ', 'ᅠ', 'ㅤ', 'ﾠ', '⠀':
		return true
	}
	return unicode.Is(unicode.Variation_Selector, r)
}

// Clean is clean for other packages: control and invisible characters
// become "?", cut to MaxLine characters. It does not escape HTML.
func Clean(s string) string { return clean(s) }

// esc cleans and HTML-escapes text from the commit. It is not exported:
// text from the repo leaves this package only through Code and Output, so
// it is always inside <code> or <pre> (code6x/pullgate#29).
func esc(s string) string { return html.EscapeString(clean(s)) }

// Code is the one way to put one line of text from the repo, the commit,
// or a command that ran on it into a message: cleaned, HTML-escaped and
// inside <code>. Telegram makes no link, mention, hashtag or command of
// text inside <code> (code6x/pullgate#23).
func Code(s string) string { return "<code>" + esc(s) + "</code>" }

// SHA shows a commit as its short form, in <code> (code6x/pullgate#30).
// The full SHA is shown once per question, on the "Full:" line.
func SHA(sha string) string { return Code(Short(sha)) }

// OutputLine is the most characters of one line of command output in a
// message. A longer line keeps its start and its end, with "…" between:
// most tools print the reason last (code6x/pullgate#25).
const OutputLine = 300

// OutputMax is the most characters of command output in one message.
const OutputMax = 1500

// Output is the way to put the output of a command into a message: its
// last lines (at most n, and at most OutputMax characters), each cleaned
// and cut in the middle, in a <pre> block. It is "" for empty output.
func Output(out string, n int) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var keep []string
	used := 0
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		l := cutMiddle(lines[i], OutputLine)
		r := Len(l) + 1 // UTF-16 units, as Telegram counts
		if used+r > OutputMax && len(keep) > 0 {
			break
		}
		used += r
		keep = append([]string{html.EscapeString(l)}, keep...)
	}
	s := strings.Join(keep, "\n")
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "<pre>" + s + "</pre>"
}

// Reason is an error in a message: one line, cut in the middle so its
// start and its end (where most tools print the reason) stay, in <code>
// (code6x/pullgate#25).
func Reason(s string) string { return Code(CutLine(s)) }

// CutLine is s as one line of at most OutputLine characters, cut in the
// middle.
func CutLine(s string) string {
	return cutMiddle(strings.Join(strings.Fields(s), " "), OutputLine)
}

// BrokenReason is the reason of the broken state in a message. It holds
// two errors, each cut with CutLine already, so it gets more room than
// Reason.
func BrokenReason(s string) string {
	return Code(cutMiddle(strings.Join(strings.Fields(s), " "), 2*OutputLine+200))
}

// cutMiddle cleans one line like clean, but a line longer than max keeps
// its first max/4 and its last characters, with "…" between.
func cutMiddle(s string, max int) string {
	r := []rune(s)
	for i, c := range r {
		if c == utf8.RuneError || unicode.IsControl(c) || isInvisible(c) {
			r[i] = '?'
		}
	}
	if len(r) > max {
		head := max / 4
		r = append(append(r[:head:head], '…'), r[len(r)-(max-head-1):]...)
	}
	return string(r)
}

// Len is the length of s in UTF-16 units, as Telegram counts.
func Len(s string) int { return len(utf16.Encode([]rune(s))) }

// Match reports whether p matches the watch pattern. "*" and the other
// path.Match patterns work inside one path part; "**" matches any number
// of parts. A pattern without "/" matches the last part (the file name)
// too, so "*.toml" matches "a/b.toml".
func Match(pattern, p string) bool {
	if !strings.Contains(pattern, "/") && pattern != "**" {
		if ok, _ := path.Match(pattern, path.Base(p)); ok {
			return true
		}
	}
	return matchParts(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchParts(pat, parts []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(parts); i++ {
				if matchParts(pat[1:], parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], parts[0]); err != nil || !ok {
			return false
		}
		pat, parts = pat[1:], parts[1:]
	}
	return len(parts) == 0
}

// Watched returns the files that match any watch pattern.
func Watched(patterns, files []string) []string {
	var out []string
	for _, f := range files {
		for _, p := range patterns {
			if Match(p, f) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// Fit joins head and body lines with newlines, then tail, if it is not
// "". The head and the tail always stay. Body lines are added while the
// text fits Limit; when some are left out, a line says how many. The lines must be escaped already; the cut is on
// whole lines, so it never splits a tag. preOpen is the index of a body
// line that opens a <pre> block that ends with a line "</pre>"; when the
// cut falls inside it, the block is closed (-1: none).
func Fit(head, body []string, tail string, preOpen int) string {
	text := strings.Join(head, "\n")
	used := Len(text)
	reserve := Len("\n</pre>\n… 99999 more lines left out")
	end := ""
	if tail != "" {
		end = "\n" + tail
		reserve += Len(end)
	}
	for i, l := range body {
		if used+1+Len(l)+reserve > Limit {
			left := len(body) - i
			if preOpen >= 0 && i > preOpen && !closedBefore(body, preOpen, i) {
				text += "\n</pre>"
			}
			return text + fmt.Sprintf("\n… %d more lines left out", left) + end
		}
		text += "\n" + l
		used += 1 + Len(l)
	}
	return text + end
}

func closedBefore(body []string, open, i int) bool {
	for j := open + 1; j < i; j++ {
		if body[j] == "</pre>" {
			return true
		}
	}
	return false
}

// ReportLines is the most lines of the apply report in a message.
const ReportLines = 10

// Report is the report file of apply in a message (DESIGN.md section
// 11): its first ReportLines lines, each cleaned and in <code>, at most
// OutputMax characters in all; a last line counts the lines left out.
// truncated says that the file was longer than root read. It is "" when
// the report has no lines.
func Report(text string, truncated bool) string {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	out := []string{"Report of apply:"}
	used := 0
	shown := 0
	for _, l := range lines {
		if shown >= ReportLines {
			break
		}
		c := Code(strings.TrimRight(l, "\r"))
		if used+Len(c)+1 > OutputMax && shown > 0 {
			break
		}
		out = append(out, c)
		used += Len(c) + 1
		shown++
	}
	if left := len(lines) - shown; left > 0 || truncated {
		more := fmt.Sprintf("… %d more lines in the report", left)
		if truncated {
			more = "… more lines in the report (only its start was read)"
		}
		out = append(out, more)
	}
	return strings.Join(out, "\n")
}
