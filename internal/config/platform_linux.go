package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/code6x/pullgate/internal/safedir"
)

func validatePlatform(_ *Instance) error { return nil }

func listenerDefaults(c *Listener) {
	if c.SpoolDir == "" {
		c.SpoolDir = "/var/spool/pullgate"
	}
	if c.StateFile == "" {
		c.StateFile = "/var/lib/pullgate-listener/state.json"
	}
	if c.APIURL == "" {
		c.APIURL = "https://api.telegram.org"
	}
	if c.TokenFile == "" {
		if d := os.Getenv("CREDENTIALS_DIRECTORY"); d != "" {
			c.TokenFile = filepath.Join(d, "bot-token")
		}
	}
}

// checkProgram checks a program file like a gate file. A symlink on the
// way (such as /bin -> usr/bin) is allowed only when the directory that
// holds it passes the check too, and the target passes it.
func checkProgram(p string, uid uint32) error {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("the program %s: %w. Fix: install it, or fix the path", p, err)
	}
	if real != p {
		if err := safedir.CheckParents(filepath.Dir(p), uid); err != nil && !errors.Is(err, safedir.ErrSymlink) {
			return fmt.Errorf("the program %s: %w", p, err)
		}
		if err := checkLinks(p, uid); err != nil {
			return err
		}
	}
	return checkFile(real, uid, false)
}

// checkLinks checks that each symlink on the way to p is owned by uid
// (or root) and that the directory that holds it passes the check.
func checkLinks(p string, uid uint32) error {
	cur := "/"
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(p), "/"), "/") {
		next := filepath.Join(cur, part)
		fi, err := os.Lstat(next)
		if err != nil {
			return fmt.Errorf("the program %s: %w", p, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return err
			}
			if err := safedir.CheckParents(real, uid); err != nil {
				return fmt.Errorf("the program %s: the symlink %s is in an unsafe directory: %w", p, next, err)
			}
			if err := safedir.CheckOwner(fi, uid); err != nil {
				return fmt.Errorf("the program %s: the symlink %s: %w", p, next, err)
			}
		}
		if r, err := filepath.EvalSymlinks(next); err == nil {
			cur = r
		} else {
			return err
		}
	}
	return nil
}

// checkFile checks a file and the directories above it.
func checkFile(path string, uid uint32, private bool) error {
	if err := safedir.CheckParents(filepath.Dir(path), uid); err != nil {
		return err
	}
	if err := safedir.CheckFile(path, uid, private); err != nil {
		return err
	}
	return nil
}

// readSmall reads a regular file of at most max bytes. It follows no
// symlink and does not wait on a FIFO, so a file that is swapped after
// the check cannot make root read another file or hang.
// With owner >= 0 the open file must also be owned by owner and be
// readable by nobody else, checked on the file that is read.
func readSmall(path string, max int64, owner int) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // the checked token file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if owner >= 0 {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != owner || fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("the file changed after its check: it is not owned by uid %d with mode 0600", owner)
		}
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return io.ReadAll(io.LimitReader(f, max+1))
}

// ReadToken reads the bot token. The file must be owned by uid (the
// listener user) and readable by nobody else. The token is never printed.
func (c *Listener) ReadToken(uid uint32) (string, error) {
	owner := int(uid)
	if err := safedir.CheckFile(c.TokenFile, uid, true); err != nil {
		owner = -1 // a systemd credential: its directory is safe, its owner may differ
		if !strings.HasPrefix(c.TokenFile, os.Getenv("CREDENTIALS_DIRECTORY")+"/") || os.Getenv("CREDENTIALS_DIRECTORY") == "" {
			return "", fmt.Errorf("token_file: %w", err)
		}
	}
	b, err := readSmall(c.TokenFile, 4096, owner)
	if err != nil {
		return "", fmt.Errorf("read the token file %s: %w. Fix: make it a regular file with the token on one line", c.TokenFile, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, " /\n\t") {
		return "", errors.New("the token file holds no valid bot token. Fix: put the token from BotFather in it, on one line")
	}
	return tok, nil
}

// ReadGitHubToken reads the token file of [github] (DESIGN.md section 22):
// owned by uid (root), mode 0600, in safe folders, not a link, at most
// MaxGitHubToken bytes. One newline at the end is removed; any other
// space or control character is an error. The token is never printed.
func (c *Instance) ReadGitHubToken(uid uint32) (string, error) {
	g := c.GitHub
	if g == nil {
		return "", errors.New("no [github] block")
	}
	if err := checkFile(g.TokenFile, uid, true); err != nil {
		return "", c.errf("[github] token_file", "%v", err)
	}
	b, err := readSmall(g.TokenFile, MaxGitHubToken, int(uid))
	if err != nil {
		return "", c.errf("[github] token_file", "read %s: %v. Fix: make it a regular file, owned by root, mode 0600, with only the token in it", g.TokenFile, err)
	}
	tok := strings.TrimSuffix(string(b), "\n")
	bad := tok == ""
	for _, r := range tok {
		if r <= ' ' || r >= 0x7f {
			bad = true
			break
		}
	}
	if bad {
		return "", c.errf("[github] token_file", "%s does not hold one token: it is empty, or has a space, a control character or more than one line. Fix: write only the token in it, with at most one newline at the end", g.TokenFile)
	}
	return tok, nil
}
