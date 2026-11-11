// Command pullgate pulls a git repo, asks the owner on Telegram to apply one
// exact commit, and applies it as root. See DESIGN.md.
package main

import (
	"fmt"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println("pullgate", version)
		return
	}
	fmt.Fprintln(os.Stderr, `pullgate: give a command: version; see DESIGN.md (exit 64)`)
	os.Exit(64)
}
