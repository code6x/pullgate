package state

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/code6x/pullgate/internal/winsecure"
)

// Ensure creates state inside a protected base. The caller must validate
// the base and its ancestors before constructing Dir.
func (d Dir) Ensure() error {
	if err := winsecure.Check(filepath.Dir(d.Path), true); err != nil {
		return err
	}
	for _, path := range []string{d.Path, d.Trees()} {
		if err := winsecure.Mkdir(path); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return fmt.Errorf("make %s: %w. Fix: check access to the state directory", path, err)
		}
		if err := winsecure.Check(path, true); err != nil {
			return err
		}
	}
	return nil
}

func (d Dir) PauseIfNone(reason string) (bool, error) {
	if err := winsecure.Check(d.Path, true); err != nil {
		return false, err
	}
	f, err := winsecure.CreateTemp(d.Path)
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.WriteString(reason + "\n")
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	if err := winsecure.Check(f.Name(), false); err != nil {
		return false, err
	}
	err = moveStateFile(f.Name(), d.PauseFile(), windows.MOVEFILE_WRITE_THROUGH)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return false, nil
	}
	return err == nil, err
}

func (d Dir) PauseReason() string {
	f, err := openStateFile(d.PauseFile())
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 200)
	n, _ := io.ReadFull(f, b)
	line, _, _ := strings.Cut(string(b[:n]), "\n")
	return line
}

func (d Dir) TelegramPaused(_ uint32) bool {
	f, err := openStateFile(d.PauseFile())
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	return err == nil && telegramPauseFile(f, fi)
}

// ResumeTelegram removes the pause file only if it is a /pause one
// (TelegramPaused). It checks and deletes the file through one handle that
// does not share write or delete access, so nobody can replace the file
// between the check and the delete (status --pause takes no lock). It
// reports whether the pause was lifted.
func (d Dir) ResumeTelegram(_ uint32) (bool, error) {
	if err := winsecure.Check(d.Path, true); err != nil {
		return false, err
	}
	name, err := windows.UTF16PtrFromString(d.PauseFile())
	if err != nil {
		return false, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.DELETE,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open the pause file: %w. Fix: if another pullgate command uses this instance, wait for it and send /resume again; else check the ACL of the state directory", err)
	}
	f := os.NewFile(uintptr(h), d.PauseFile())
	defer func() { _ = f.Close() }()
	if winsecure.CheckFileHandle(f) != nil {
		return false, nil
	}
	fi, err := f.Stat()
	if err != nil || !telegramPauseFile(f, fi) {
		return false, nil
	}
	info := fileDispositionInfo{DeleteFile: true}
	if err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil { //nolint:gosec // documented Win32 disposition structure
		return false, fmt.Errorf("remove the pause file: %w. Fix: check the ACL of the state directory, or run pullgate status NAME --resume as an administrator", err)
	}
	return true, nil
}

func (d Dir) Lock(wait bool) (*os.File, error) {
	if err := winsecure.Check(d.Path, true); err != nil {
		return nil, err
	}
	path := d.file("lock")
	if err := winsecure.Check(path, false); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, err
	}
	f, err := winsecure.CreateExclusive(path)
	if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		f, err = os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // checked directory and existing lock path
	}
	if err != nil {
		return nil, fmt.Errorf("open the lock: %w. Fix: check ownership and ACL of the state directory", err)
	}
	if err := winsecure.Check(path, false); err != nil {
		_ = f.Close()
		return nil, err
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{}); err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("take the lock: %w. Fix: use local NTFS state storage", err)
	}
	return f, nil
}

func openStateFile(path string) (*os.File, error) {
	if err := winsecure.Check(filepath.Dir(path), true); err != nil {
		return nil, err
	}
	if err := winsecure.Check(path, false); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	return os.Open(path) //nolint:gosec // directory and file checked above
}

func readStateFile(path string) ([]byte, error) {
	f, err := openStateFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

func createStateTemp(dir string) (*os.File, error) {
	if err := winsecure.Check(dir, true); err != nil {
		return nil, err
	}
	return winsecure.CreateTemp(dir)
}

func replaceStateFile(tmp, path string) error {
	if err := winsecure.Check(path, false); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return err
	}
	if err := winsecure.Check(tmp, false); err != nil {
		return err
	}
	return moveStateFile(tmp, path, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func moveStateFile(from, to string, flags uint32) error {
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(src, dst, flags)
}

// fileDispositionInfo is FILE_DISPOSITION_INFO; x/sys/windows has no type
// for it. DeleteFile is a one-byte BOOLEAN.
type fileDispositionInfo struct {
	DeleteFile bool
}
