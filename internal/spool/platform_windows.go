package spool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/code6x/pullgate/internal/winsecure"
)

const safeReadFlags = int(windows.O_FILE_FLAG_OPEN_REPARSE_POINT)
const safeWriteFlags = safeReadFlags | os.O_RDWR

func ownerUID() int { return 0 }

func openSpoolRoot(path string) (*os.Root, error) {
	if err := winsecure.Check(path, true); err != nil {
		return nil, err
	}
	return os.OpenRoot(path)
}

func checkReadFile(f *os.File, _ os.FileInfo, _ int) error {
	return winsecure.CheckFileHandle(f)
}

// Windows uses one privileged account for the spool. The private ACL is
// inherited from its parent, and checked before a file is published.
func FixMode(_ *os.File, _, _ int, _ os.FileMode) error { return nil }

func readBack(f *os.File, what string, _, _ int, _ os.FileMode) error {
	if err := winsecure.CheckFileHandle(f); err != nil {
		return fmt.Errorf("%w on %s: %w. Fix: check the spool ACL", ErrNotKept, what, err)
	}
	return nil
}

func (d Dirs) Ensure(_ uint32, _ Owners) error {
	if err := winsecure.Check(filepath.Dir(d.Base), true); err != nil {
		return err
	}
	for _, path := range []string{d.Base, d.inst(), d.Outbox(), d.Inbox(), d.CheckDir(), d.Receipts()} {
		if err := winsecure.Mkdir(path); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return err
		}
		if err := winsecure.Check(path, true); err != nil {
			return err
		}
	}
	return nil
}
