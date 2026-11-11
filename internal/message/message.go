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

// Clean makes text from the commit safe to show: control and invisible
// characters become "?", and the text is cut to MaxLine characters. It
// does not escape HTML; Esc does.
func Clean(s string) string {
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

// Esc cleans and HTML-escapes text from the commit.
func Esc(s string) string { return html.EscapeString(Clean(s)) }

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

// Fit joins head and body lines with newlines. The head always stays. Body
// lines are added while the text fits Limit; when some are left out, the
// last line says how many. The lines must be escaped already; the cut is on
// whole lines, so it never splits a tag. preOpen is the index of a body
// line that opens a <pre> block that ends with a line "</pre>"; when the
// cut falls inside it, the block is closed (-1: none).
func Fit(head, body []string, preOpen int) string {
	text := strings.Join(head, "\n")
	used := Len(text)
	reserve := Len("\n</pre>\n… 99999 more lines left out")
	for i, l := range body {
		if used+1+Len(l)+reserve > Limit {
			left := len(body) - i
			if preOpen >= 0 && i > preOpen && !closedBefore(body, preOpen, i) {
				text += "\n</pre>"
			}
			return text + fmt.Sprintf("\n… %d more lines left out", left)
		}
		text += "\n" + l
		used += 1 + Len(l)
	}
	return text
}

func closedBefore(body []string, open, i int) bool {
	for j := open + 1; j < i; j++ {
		if body[j] == "</pre>" {
			return true
		}
	}
	return false
}
