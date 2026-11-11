// Package safedir checks that no other user can write a directory or the
// files in it (rule 6, DESIGN.md section 6).
package safedir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ownerOf returns the uid of a file.
func ownerOf(fi fs.FileInfo) (uint32, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no owner data for this file")
	}
	return st.Uid, nil
}

// ErrSymlink is part of the error for a symlink on the way.
var ErrSymlink = errors.New("symlink on the way")

// CheckOwner checks that fi is owned by uid or by root.
func CheckOwner(fi fs.FileInfo, uid uint32) error {
	own, err := ownerOf(fi)
	if err != nil {
		return err
	}
	if own != uid && own != 0 {
		return fmt.Errorf("owned by uid %d, not by uid %d or root", own, uid)
	}
	return nil
}

// CheckParents checks dir and each directory above it up to "/". Each one
// must be a real directory (not a symlink), owned by uid or by root, and
// not writable by group or others. A directory that others can write is
// allowed only when it is owned by root and has the sticky bit (like /tmp).
func CheckParents(dir string, uid uint32) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("check %s: %w", dir, err)
	}
	p := abs
	for {
		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("check %s: %w", p, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; root uses only real directories here. Fix: use a path without symlinks: %w", p, ErrSymlink)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", p)
		}
		own, err := ownerOf(fi)
		if err != nil {
			return fmt.Errorf("check %s: %w", p, err)
		}
		if own != uid && own != 0 {
			return fmt.Errorf("%s is owned by uid %d, not by uid %d or root; another user could change git's files. Fix: chown root %s", p, own, uid, p)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			sticky := fi.Mode()&fs.ModeSticky != 0
			if p == abs || !sticky || own != 0 {
				return fmt.Errorf("%s can be written by group or others (mode %04o); another user could change git's files. Fix: chmod go-w %s", p, fi.Mode().Perm(), p)
			}
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}

// CheckTree checks dir with CheckParents, and then every file and
// directory inside it: each must be owned by uid, not writable by group or
// others, and not a symlink or a special file.
func CheckTree(dir string, uid uint32) error {
	if err := CheckParents(dir, uid); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("check %s: %w", p, err)
		}
		fi, err := d.Info()
		if err != nil {
			return fmt.Errorf("check %s: %w", p, err)
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return fmt.Errorf("%s is not a regular file or a directory (mode %s); root runs git only in a clean directory. Fix: remove it, or remove %s and let pullgate clone again", p, fi.Mode(), dir)
		}
		own, err := ownerOf(fi)
		if err != nil {
			return fmt.Errorf("check %s: %w", p, err)
		}
		if own != uid {
			return fmt.Errorf("%s is owned by uid %d, not by uid %d; another user could change git's files. Fix: remove %s and let pullgate clone again", p, own, uid, dir)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s can be written by group or others (mode %04o). Fix: chmod -R go-w %s", p, fi.Mode().Perm(), dir)
		}
		return nil
	})
}

// CheckFile checks a config, key or known hosts file: a regular file (not a
// symlink), owned by uid, not writable by group or others. With private,
// it must not be readable by group or others either.
func CheckFile(path string, uid uint32, private bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("check %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (mode %s). Fix: use a regular file, not a symlink", path, fi.Mode())
	}
	own, err := ownerOf(fi)
	if err != nil {
		return fmt.Errorf("check %s: %w", path, err)
	}
	if own != uid {
		return fmt.Errorf("%s is owned by uid %d, not by uid %d. Fix: chown root %s", path, own, uid, path)
	}
	mask := fs.FileMode(0o022)
	fix := "chmod go-w"
	if private {
		mask = 0o077
		fix = "chmod 600"
	}
	if fi.Mode().Perm()&mask != 0 {
		return fmt.Errorf("%s has mode %04o, which other users can change or read. Fix: %s %s", path, fi.Mode().Perm(), fix, path)
	}
	return nil
}
