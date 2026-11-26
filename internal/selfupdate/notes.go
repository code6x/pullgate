package selfupdate

import (
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/code6x/pullgate/internal/message"
)

// The limits of the notes in a message (code6x/pullgate#70).
const (
	NotesLines = 12   // the most lines: versions, headings and entries
	NotesChars = 1200 // the most characters of those lines, before escaping
	NotesEntry = 200  // the most characters of one entry
	NotesFile  = 262144
)

// The sections of a version that the notes show, as release-please names
// them. Other sections (Documentation, Miscellaneous) are left out.
var notesSections = []string{"Features", "Bug Fixes", "Security"}

var (
	reVersionHead = regexp.MustCompile(`^## \[?(\d+\.\d+\.\d+)\]?(?:\(|\s|$)`)
	reCloses      = regexp.MustCompile(`(?i),\s*closes\s+\[?#\d.*$`)
	reCommitLink  = regexp.MustCompile(`\s*\(\[[0-9a-f]{7,40}\]\([^)]*\)\)`)
	rePRLink      = regexp.MustCompile(`\(\[#(\d+)\]\([^)]*\)\)`)
	reMDLink      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// notes is what parseNotes takes out of CHANGELOG.md.
type notes struct {
	Lines    []string // HTML, escaped
	Security bool     // a version in the range has a "### Security" section
}

// parseNotes reads the sections of the versions newer than installed, up
// to and with newV, from a CHANGELOG.md in release-please's format. ok is
// false when the text has no section for newV: a broken or a wrong file
// gives no notes. Every line is HTML-escaped.
func parseNotes(text, installed, newV string) (notes, bool) {
	type section struct {
		name    string
		entries []string
	}
	type version struct {
		v        string
		sections []*section
	}
	var vs []*version
	var cur *version
	var sec *section
	found := false
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if m := reVersionHead.FindStringSubmatch(line); m != nil {
			cur, sec = nil, nil
			v := m[1]
			if v == newV {
				found = true
			}
			if newer(v, installed) && !newer(v, newV) {
				cur = &version{v: v}
				vs = append(vs, cur)
			}
			continue
		}
		if cur == nil {
			continue
		}
		if name, ok := strings.CutPrefix(line, "### "); ok {
			sec = &section{name: strings.TrimSpace(name)}
			cur.sections = append(cur.sections, sec)
			continue
		}
		if e, ok := strings.CutPrefix(line, "* "); ok && sec != nil {
			if e = cleanEntry(e); e != "" {
				sec.entries = append(sec.entries, e)
			}
		}
	}
	if !found {
		return notes{}, false
	}
	var n notes
	used, shown, total := 0, 0, 0
	add := func(plain, h string) bool {
		if len(n.Lines) >= NotesLines-1 || used+utf8.RuneCountInString(plain) > NotesChars {
			return false
		}
		n.Lines = append(n.Lines, h)
		used += utf8.RuneCountInString(plain) + 1
		return true
	}
	full := false
	for _, v := range vs {
		head := false
		for _, s := range v.sections {
			if s.name == "Security" {
				n.Security = true
			}
			if !slices.Contains(notesSections, s.name) || len(s.entries) == 0 {
				continue
			}
			total += len(s.entries)
			if full {
				continue
			}
			// A version line and a heading only with room for one entry.
			need := 2
			if !head {
				need = 3
			}
			if len(n.Lines)+need > NotesLines-1 {
				full = true
				continue
			}
			if !head {
				if !add(v.v, "<b>"+v.v+"</b>") {
					full = true
					continue
				}
				head = true
			}
			name := message.Clean(s.name)
			if !add(name+":", html.EscapeString(name)+":") {
				full = true
				continue
			}
			for _, e := range s.entries {
				if full || !add("• "+e, "• "+html.EscapeString(e)) {
					full = true
					break
				}
				shown++
			}
		}
	}
	if total > shown {
		n.Lines = append(n.Lines, "… and "+strconv.Itoa(total-shown)+" more")
	}
	return n, true
}

// cleanEntry makes one entry short: the text and "(#N)" of its pull
// request, without links, commit ids and the ", closes …" part.
func cleanEntry(e string) string {
	e = reCommitLink.ReplaceAllString(e, "")
	e = reCloses.ReplaceAllString(e, "")
	e = rePRLink.ReplaceAllString(e, "(#$1)")
	e = reMDLink.ReplaceAllString(e, "$1")
	e = strings.ReplaceAll(e, "**", "")
	// Control and invisible characters (bidi, zero-width) become "?", as
	// in every text from outside (DESIGN.md section 12).
	e = message.Clean(strings.Join(strings.Fields(e), " "))
	if utf8.RuneCountInString(e) > NotesEntry {
		r := []rune(e)
		e = string(r[:NotesEntry-1]) + "…"
	}
	return e
}
