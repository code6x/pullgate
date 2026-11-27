package spool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/code6x/pullgate/internal/safedir"
)

const safeReadFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
const safeWriteFlags = syscall.O_NOFOLLOW | os.O_WRONLY

func ownerUID() int { return os.Geteuid() }

func openSpoolRoot(path string) (*os.Root, error) { return os.OpenRoot(path) }

func checkReadFile(_ *os.File, fi os.FileInfo, uid int) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); uid >= 0 && (!ok || int(st.Uid) != uid) {
		return errors.New("not owned by the listener user")
	}
	return nil
}

// FixMode sets the owner (uid, gid; -1 keeps one) and the mode of an open
// file or directory. Tests replace it to play a kernel that does not keep
// a mode (issue #19): every caller reads the result back.
var FixMode = func(f *os.File, uid, gid int, mode os.FileMode) error {
	if uid >= 0 || gid >= 0 {
		if err := f.Chown(uid, gid); err != nil {
			return err
		}
	}
	return f.Chmod(mode)
}

// readBack checks that the open file f has the owner uid, the group gid
// (-1: any) and exactly the mode bits mode (also setuid, setgid, sticky).
func readBack(f *os.File, what string, uid, gid int, mode os.FileMode) error {
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("read back %s: %w", what, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("read back %s: no owner. This is a bug in pullgate", what)
	}
	got := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	if (uid >= 0 && int(st.Uid) != uid) || (gid >= 0 && int(st.Gid) != gid) || got != mode {
		return fmt.Errorf("%w on %s: want owner %d, group %d, mode %s; got owner %d, group %d, mode %s. "+
			"Fix: run \"pullgate doctor\" as root; check that the unit keeps CAP_CHOWN and CAP_FOWNER, and the mount options of the file system",
			ErrNotKept, what, uid, gid, mode, st.Uid, st.Gid, got)
	}
	return nil
}

// Ensure makes the spool of the instance with the owners and modes of
// DESIGN.md section 4. Base and Base/NAME belong to root (uid) and must
// pass the check of rule 6; outbox and inbox are fixed through an open
// directory, never through a path. No directory gets the setgid bit:
// pullgate-run@.service has RestrictSUIDSGID=yes, which refuses a chmod
// that sets it. Send gives each outbox file its group itself.
func (d Dirs) Ensure(uid uint32, o Owners) error {
	for _, p := range []string{d.Base, d.inst()} {
		if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("make %s: %w", p, err)
		}
	}
	for _, p := range []string{d.Base, d.inst()} {
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0) //nolint:gosec // the spool base and instance directory of root
		if err != nil {
			return fmt.Errorf("open %s: %w", p, err)
		}
		err = FixMode(f, -1, -1, 0o755)
		if err == nil {
			err = readBack(f, p, int(uid), -1, 0o755)
		}
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("fix %s: %w", p, err)
		}
	}
	if err := safedir.CheckParents(d.inst(), uid); err != nil {
		return fmt.Errorf("the spool is not safe: %w", err)
	}
	root, err := os.OpenRoot(d.inst())
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, s := range []struct {
		name     string
		uid, gid int
		mode     os.FileMode
	}{
		{"outbox", int(uid), o.ListenerGID, 0o770},
		{"inbox", o.ListenerUID, o.ListenerGID, 0o700},
		{"check", o.ListenerUID, o.ListenerGID, 0o700},
		{"receipts", o.ListenerUID, o.ListenerGID, 0o700},
	} {
		if err := root.Mkdir(s.name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("make %s: %w", s.name, err)
		}
		f, err := root.OpenFile(s.name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open %s/%s: %w", d.inst(), s.name, err)
		}
		err = FixMode(f, s.uid, s.gid, s.mode)
		if err == nil {
			err = readBack(f, d.inst()+"/"+s.name, s.uid, s.gid, s.mode)
		}
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("fix %s/%s: %w", d.inst(), s.name, err)
		}
	}
	return nil
}
