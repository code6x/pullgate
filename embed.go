// Package pullgate holds the files that the binary carries: install.sh and
// the units of contrib/systemd/ of this version (DESIGN.md section 15). It
// also holds the tests of install.sh.
package pullgate

import (
	"embed"
	"io/fs"
	"strings"
)

// InstallScript is install.sh of this version. pullgate update runs it, so
// the code that checks a release is the code that was checked and
// installed before.
//
//go:embed install.sh
var InstallScript string

//go:embed contrib/systemd
var unitFS embed.FS

// Enable lists the units that README.md enables in its "start" block.
// NAME stands for each instance. pullgate update re-enables them after an
// update. A test checks this list against the README.
var Enable = []string{
	"pullgate-listen.service",
	"pullgate-apply@NAME.path",
	"pullgate-run@NAME.timer",
	"pullgate-check@NAME.path",
	"pullgate-update.timer",
	"pullgate-update.path",
}

// Units returns the unit files of contrib/systemd/ of this version, by
// name.
func Units() map[string]string {
	out := map[string]string{}
	entries, err := fs.ReadDir(unitFS, "contrib/systemd")
	if err != nil {
		panic("pullgate: the embedded units cannot be read: " + err.Error())
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := unitFS.ReadFile("contrib/systemd/" + e.Name())
		if err != nil {
			panic("pullgate: the embedded unit " + e.Name() + " cannot be read: " + err.Error())
		}
		out[e.Name()] = string(b)
	}
	return out
}
