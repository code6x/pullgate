package pullgate

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestEnableIsTheReadme: Enable, which pullgate bootstrap enables and
// pullgate update re-enables after an update, is the list of units of
// step 5 of bootstrap in the README.
func TestEnableIsTheReadme(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(b), "5. **Doctor, then the units.**")
	if !ok {
		t.Fatal("README.md has no step 5 of bootstrap (Doctor, then the units)")
	}
	block, _, _ = strings.Cut(block, "\n\n")
	re := regexp.MustCompile(`pullgate-[a-z-]+(@NAME)?\.(service|timer|path)`)
	var got []string
	for line := range strings.SplitSeq(block, "\n") {
		line, _, _ = strings.Cut(line, "#")
		got = append(got, re.FindAllString(line, -1)...)
	}
	slices.Sort(got)
	want := slices.Clone(Enable)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the README bootstrap step 5 enables %q, Enable is %q. Fix: change both together", got, want)
	}
	units := Units()
	for _, e := range Enable {
		if _, ok := units[strings.Replace(e, "@NAME.", "@.", 1)]; !ok {
			t.Errorf("%s: no unit file in contrib/systemd", e)
		}
	}
}

// TestUnitsAreEmbedded: the binary carries every file of contrib/systemd.
func TestUnitsAreEmbedded(t *testing.T) {
	entries, err := os.ReadDir("contrib/systemd")
	if err != nil {
		t.Fatal(err)
	}
	units := Units()
	if len(units) != len(entries) {
		t.Fatalf("embedded %d units, contrib/systemd has %d files", len(units), len(entries))
	}
	for _, e := range entries {
		b, _ := os.ReadFile("contrib/systemd/" + e.Name())
		if units[e.Name()] != string(b) {
			t.Errorf("%s: the embedded copy differs", e.Name())
		}
	}
	if b, _ := os.ReadFile("install.sh"); InstallScript != string(b) {
		t.Error("the embedded install.sh differs")
	}
}
