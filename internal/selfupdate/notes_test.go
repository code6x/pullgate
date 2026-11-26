package selfupdate

import (
	"strings"
	"testing"
)

const changelog = `# Changelog

## [0.6.0](https://github.com/code6x/pullgate/compare/v0.5.0...v0.6.0) (2026-12-10)


### Features

* **listener:** /doctor from Telegram ([#73](https://github.com/code6x/pullgate/issues/73)) ([9a0179d](https://github.com/code6x/pullgate/commit/9a0179d0000000000000000000000000000000))
* a <b>bold</b> & [linked](https://example.com) text, the question closes later ([#74](https://example.com/74)) ([abc1234](https://example.com/c)), closes [#50](https://example.com/50)


### Documentation

* a doc change that is not shown ([#75](https://example.com/75))

## [0.5.0](https://github.com/code6x/pullgate/compare/v0.4.0...v0.5.0) (2026-12-01)


### Bug Fixes

* an entry with no pull request link ([def5678](https://example.com/c))

## [0.4.0](https://github.com/code6x/pullgate/compare/v0.3.0...v0.4.0) (2026-11-26)


### Features

* an old feature that is installed already ([#67](https://example.com/67))
`

func TestNotesOneVersion(t *testing.T) {
	n, ok := parseNotes(changelog, "0.5.0", "0.6.0")
	if !ok {
		t.Fatal("no notes")
	}
	want := []string{
		"<b>0.6.0</b>",
		"Features:",
		"• listener: /doctor from Telegram (#73)",
		"• a &lt;b&gt;bold&lt;/b&gt; &amp; linked text, the question closes later (#74)",
	}
	if strings.Join(n.Lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(n.Lines, "\n"), strings.Join(want, "\n"))
	}
	if n.Security {
		t.Fatal("no security section")
	}
	all := strings.Join(n.Lines, "\n")
	for _, bad := range []string{"9a0179d", "abc1234", "https://", "#50", "closes [", "doc change", "**"} {
		if strings.Contains(all, bad) {
			t.Errorf("the notes hold %q", bad)
		}
	}
}

func TestNotesSeveralVersions(t *testing.T) {
	n, ok := parseNotes(changelog, "0.4.0", "0.6.0")
	all := strings.Join(n.Lines, "\n")
	if !ok || !strings.Contains(all, "<b>0.6.0</b>") || !strings.Contains(all, "<b>0.5.0</b>\nBug Fixes:\n• an entry with no pull request link") ||
		strings.Contains(all, "0.4.0") || strings.Contains(all, "old feature") || strings.Contains(all, "def5678") {
		t.Fatalf("0.5.0 and 0.6.0, not 0.4.0:\n%s", all)
	}
	if strings.Index(all, "0.6.0") > strings.Index(all, "0.5.0") {
		t.Fatal("the newest version first, as in the file")
	}
}

func TestNotesSecurity(t *testing.T) {
	text := strings.Replace(changelog, "### Bug Fixes", "### Security", 1)
	if n, ok := parseNotes(text, "0.4.0", "0.6.0"); !ok || !n.Security || !strings.Contains(strings.Join(n.Lines, "\n"), "Security:") {
		t.Fatalf("a Security section in 0.5.0 sets the flag: %+v", n)
	}
	// A Security section of a version that is installed already does not.
	text = strings.Replace(changelog, "## [0.4.0](https://github.com/code6x/pullgate/compare/v0.3.0...v0.4.0) (2026-11-26)\n\n\n### Features",
		"## [0.4.0](x) (2026-11-26)\n\n\n### Security", 1)
	if n, ok := parseNotes(text, "0.4.0", "0.6.0"); !ok || n.Security {
		t.Fatalf("a Security section of 0.4.0 must not count: %+v", n)
	}
}

func TestNotesCut(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Changelog\n\n## [0.6.0](x) (2026-12-10)\n\n### Features\n\n")
	for i := range 30 {
		b.WriteString("* entry number " + strings.Repeat("x", i%3) + " ([#1](x))\n")
	}
	n, ok := parseNotes(b.String(), "0.5.0", "0.6.0")
	if !ok || len(n.Lines) != NotesLines || n.Lines[len(n.Lines)-1] != "… and 21 more" {
		t.Fatalf("want %d lines, the last \"… and 21 more\": %d %q", NotesLines, len(n.Lines), n.Lines)
	}
	// The character limit cuts too.
	b.Reset()
	b.WriteString("## [0.6.0](x) (2026-12-10)\n\n### Features\n\n")
	for range 8 {
		b.WriteString("* " + strings.Repeat("y", 300) + "\n")
	}
	n, _ = parseNotes(b.String(), "0.5.0", "0.6.0")
	total := 0
	for _, l := range n.Lines {
		total += len([]rune(l)) + 1
	}
	if total > NotesChars+40 || !strings.HasPrefix(n.Lines[len(n.Lines)-1], "… and ") {
		t.Fatalf("the notes must stay near %d characters: %d %q", NotesChars, total, n.Lines)
	}
	for _, l := range n.Lines {
		if len([]rune(l)) > NotesEntry+2 {
			t.Fatalf("an entry is cut at %d characters: %d", NotesEntry, len([]rune(l)))
		}
	}
}

// TestNotesCleanInvisible: control and invisible characters in an entry
// become "?".
func TestNotesCleanInvisible(t *testing.T) {
	text := "## [0.6.0](x)\n\n### Features\n\n* a\u202eb\u200bc\x1bd\x00e ([#1](x))\n"
	n, ok := parseNotes(text, "0.5.0", "0.6.0")
	if !ok || len(n.Lines) != 3 || n.Lines[2] != "• a?b?c?d?e (#1)" {
		t.Fatalf("got %q", n.Lines)
	}
}

// TestNotesNoHeadingWithoutEntry: when the limit is near, a heading or a
// version line is not added alone, and no entry comes under another
// heading.
func TestNotesNoHeadingWithoutEntry(t *testing.T) {
	var b strings.Builder
	b.WriteString("## [0.6.0](x)\n\n### Features\n\n")
	for range 6 {
		b.WriteString("* " + strings.Repeat("z", 199) + "\n")
	}
	b.WriteString("\n### Bug Fixes\n\n* x\n")
	n, ok := parseNotes(b.String(), "0.5.0", "0.6.0")
	if !ok {
		t.Fatal("no notes")
	}
	for i, l := range n.Lines {
		if l == "• x" && (i == 0 || n.Lines[i-1] != "Bug Fixes:") {
			t.Fatalf("an entry under the wrong heading: %q", n.Lines)
		}
		if strings.HasSuffix(l, ":") && (i+1 >= len(n.Lines) || !strings.HasPrefix(n.Lines[i+1], "• ")) {
			t.Fatalf("a heading with no entry: %q", n.Lines)
		}
	}
	if n.Lines[len(n.Lines)-1] != "… and 1 more" && n.Lines[len(n.Lines)-1] != "… and 2 more" {
		t.Fatalf("want the cut line: %q", n.Lines)
	}
}

func TestNotesBroken(t *testing.T) {
	for name, text := range map[string]string{
		"empty":            "",
		"not a changelog":  "\x00\x01 binary",
		"no new version":   strings.ReplaceAll(changelog, "0.6.0", "0.9.0"),
		"no headings":      "* an entry\n* another\n",
		"only a new title": "## [0.6.0]\n",
	} {
		n, ok := parseNotes(text, "0.5.0", "0.6.0")
		if name == "only a new title" {
			if !ok || len(n.Lines) != 0 {
				t.Errorf("%s: no entries, no lines: %v %q", name, ok, n.Lines)
			}
			continue
		}
		if ok || len(n.Lines) != 0 {
			t.Errorf("%s: want no notes, got %q", name, n.Lines)
		}
	}
}
