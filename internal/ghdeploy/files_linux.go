package ghdeploy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"

	"github.com/code6x/pullgate/internal/safedir"
)

// ensureDir makes the queue folder, sets its mode itself (the apply unit
// has UMask=0022) and reads the owner and the mode back.
func ensureDir(dir string, uid uint32) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("make the GitHub queue folder %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder. Fix: remove it; pullgate makes it again", dir)
	}
	if err := safedir.CheckOwner(fi, uid); err != nil {
		return fmt.Errorf("the GitHub queue folder %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return CheckDir(dir, uid)
}

// CheckDir reads back the owner (uid) and the mode (0700) of the queue
// folder, without following a link.
func CheckDir(dir string, uid uint32) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder. Fix: remove it; pullgate makes it again", dir)
	}
	if err := safedir.CheckOwner(fi, uid); err != nil {
		return fmt.Errorf("the GitHub queue folder %s: %w", dir, err)
	}
	if fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("the GitHub queue folder %s has mode %04o, not 0700. Fix: chmod 700 %s", dir, fi.Mode().Perm(), dir)
	}
	return nil
}

// readFile reads the queue file: not a link, not a FIFO, owned by uid,
// mode 0600, at most max bytes.
func readFile(path string, uid uint32, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // root's own queue folder
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file. Fix: remove it")
	}
	if err := safedir.CheckOwner(fi, uid); err != nil {
		return nil, err
	}
	if fi.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("mode %04o, not 0600. Fix: chmod 600 %s", fi.Mode().Perm(), path)
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("larger than %d bytes. Fix: remove it", max)
	}
	return io.ReadAll(io.LimitReader(f, max+1))
}
