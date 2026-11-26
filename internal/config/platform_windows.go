package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/code6x/pullgate/internal/gitx"
	"github.com/code6x/pullgate/internal/winsecure"
)

func validatePlatform(c *Instance) error {
	if c.Mode == ModeAuto {
		return c.errf("mode", "Windows needs approval before apply. Fix: set mode to ask or off")
	}
	if len(c.Commands.Check) != 0 || len(c.Commands.Summary) != 0 {
		return c.errf("[commands]", "Windows cannot run check or summary as a separate user. Fix: remove both commands")
	}
	for _, argv := range [][]string{c.Commands.Apply, c.Commands.Health} {
		if len(argv) > 0 && (!gitx.SafePath(argv[0]) || !strings.EqualFold(filepath.Ext(argv[0]), ".ps1")) {
			return c.errf("[commands]", "Windows commands must be PowerShell scripts. Fix: give the clean absolute path to a .ps1 script")
		}
	}
	return nil
}

func listenerDefaults(c *Listener) {
	root := filepath.Dir(c.Path)
	if c.SpoolDir == "" {
		c.SpoolDir = filepath.Join(root, "spool")
	}
	if c.StateFile == "" {
		c.StateFile = filepath.Join(root, "listener-state.json")
	}
	if c.TokenFile == "" {
		c.TokenFile = filepath.Join(root, "bot-token.dpapi")
	}
	if c.APIURL == "" {
		c.APIURL = "https://api.telegram.org"
	}
}

// The Windows CLI must check the ancestors of these protected directories
// before loading a config or token.
func checkFile(path string, _ uint32, _ bool) error {
	if err := winsecure.Check(filepath.Dir(path), true); err != nil {
		return err
	}
	return winsecure.Check(path, false)
}

func checkProgram(path string, uid uint32) error { return checkFile(path, uid, true) }

func (c *Listener) ReadToken(uid uint32) (string, error) {
	if err := checkFile(c.TokenFile, uid, true); err != nil {
		return "", fmt.Errorf("token_file: %w", err)
	}
	f, err := os.Open(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read the bot token file: %w. Fix: run token setup", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return "", errors.New("cannot read the protected bot token. Fix: run token setup")
	}
	plain, err := winsecure.UnprotectToken(b)
	if err != nil {
		return "", err
	}
	defer clear(plain)
	token := strings.TrimSpace(string(plain))
	if token == "" || strings.ContainsAny(token, " /\n\r\t") {
		return "", errors.New("invalid bot token. Fix: run token setup and enter the token on one line")
	}
	return token, nil
}
