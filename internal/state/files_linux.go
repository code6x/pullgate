package state

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/code6x/pullgate/internal/safedir"
)

// Ensure makes the state directory (0711) and the trees directory. The
// directories above must pass the check of rule 6.
func (d Dir) Ensure() error {
	if err := safedir.CheckParents(filepath.Dir(d.Path), uint32(os.Geteuid())); err != nil { //nolint:gosec // a uid fits in uint32
		return fmt.Errorf("the state directory is not safe: %w", err)
	}
	for _, p := range []string{d.Path, d.Trees()} {
		if err := os.Mkdir(p, 0o711); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("make %s: %w", p, err)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory. Fix: remove it", p)
		}
		if err := os.Chmod(p, 0o711); err != nil {
			return err
		}
	}
	return nil
}

// PauseIfNone writes the pause file only if there is none. It never
// replaces one, even one that appears at the same time (a hard link
// fails when the name exists). It reports whether it wrote it.
func (d Dir) PauseIfNone(reason string) (bool, error) {
	f, err := os.CreateTemp(filepath.Dir(d.PauseFile()), ".tmp-")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, werr := f.WriteString(reason + "\n")
	if werr == nil {
		werr = f.Chmod(0o644)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false, werr
	}
	if err := os.Link(tmp, d.PauseFile()); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// PauseReason is the first line of the pause file, at most 200 bytes, or
// "" when there is none. The file is root's; it is never followed.
func (d Dir) PauseReason() string {
	f, err := os.OpenFile(d.PauseFile(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 200)
	n, _ := io.ReadFull(f, b)
	line, _, _ := strings.Cut(string(b[:n]), "\n")
	return line
}

// ResumeTelegram removes the pause file only if it is a /pause one
// (TelegramPaused). It moves the file aside first and checks that the file
// it moved is the one it checked; if not (root wrote a new pause in
// between), it puts that one back without replacing anything, and keeps
// the pause. It reports whether the pause was lifted.
func (d Dir) ResumeTelegram(uid uint32) (bool, error) {
	f, err := os.OpenFile(d.PauseFile(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, nil
	}
	defer func() { _ = f.Close() }()
	checked, err := f.Stat()
	if err != nil || !linuxTelegramPauseFile(f, checked, uid) {
		return false, nil
	}
	tmp := d.file(".resume-tmp")
	_ = os.Remove(tmp)
	if err := os.Rename(d.PauseFile(), tmp); err != nil {
		return false, err
	}
	moved, err := os.Lstat(tmp)
	if err != nil || !os.SameFile(checked, moved) {
		// Not the file that was checked: put it back, never over a new one.
		if lerr := os.Link(tmp, d.PauseFile()); lerr != nil && !errors.Is(lerr, fs.ErrExist) {
			return false, lerr
		}
		_ = os.Remove(tmp)
		return false, nil
	}
	return true, os.Remove(tmp)
}

// TelegramPaused reports whether the pause file is one that /pause wrote:
// a regular file of uid, not linked twice, of one line that is exactly the
// reason of /pause. Any other pause (by pullgate itself or by root) is not.
func (d Dir) TelegramPaused(uid uint32) bool {
	f, err := os.OpenFile(d.PauseFile(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return linuxTelegramPauseFile(f, fi, uid)
}

// Lock takes the lock of the instance. With wait false it fails at once
// when another process holds it. Close the file to free it.
func (d Dir) Lock(wait bool) (*os.File, error) {
	f, err := os.OpenFile(d.file("lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the lock: %w", err)
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("take the lock: %w", err)
	}
	return f, nil
}

func linuxTelegramPauseFile(f *os.File, fi os.FileInfo, uid uint32) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uid && st.Nlink == 1 && telegramPauseFile(f, fi)
}

func readStateFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // state under root's directory
}

func replaceStateFile(tmp, path string) error {
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if df, err := os.Open(filepath.Dir(path)); err == nil { //nolint:gosec // our own directory
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}

func createStateTemp(dir string) (*os.File, error) { return os.CreateTemp(dir, ".tmp-") }
