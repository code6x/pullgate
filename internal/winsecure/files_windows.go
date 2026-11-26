// Package winsecure checks Windows file security and wraps machine DPAPI.
package winsecure

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CheckManaged checks path inside an existing, protected root. Callers must
// establish that the ancestors above root cannot replace or redirect root.
func CheckManaged(root, path string, directory bool) error {
	parts, err := relativeParts(root, path)
	if err != nil {
		return err
	}
	if len(parts) == 0 && !directory {
		return errors.New("the managed root must be a directory. Fix: give a file path below the root")
	}
	if err := Check(root, true); err != nil {
		return err
	}
	for i, part := range parts {
		root = filepath.Join(root, part)
		if err := Check(root, i < len(parts)-1 || directory); err != nil {
			return err
		}
	}
	return nil
}

func relativeParts(root, path string) ([]string, error) {
	invalid := errors.New("invalid managed path. Fix: use a clean absolute drive path under the managed directory")
	for _, p := range []string{root, path} {
		volume := filepath.VolumeName(p)
		if len(volume) != 2 || volume[1] != ':' || !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, invalid
		}
		for _, part := range strings.Split(p[3:], `\`) {
			if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") ||
				strings.ContainsFunc(part, func(r rune) bool { return r < 32 }) ||
				strings.ContainsAny(part, `/:*?"<>|`) || !filepath.IsLocal(part) {
				return nil, invalid
			}
		}
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, invalid
	}
	if rel == "." {
		return nil, nil
	}
	return strings.Split(rel, `\`), nil
}

// Check checks a file or directory under already trusted parents. The caller
// must check the parents first and keep untrusted users from replacing them.
func Check(path string, directory bool) error {
	if err := check(path, directory); err != nil {
		return fmt.Errorf("check %s: %w. Fix: use a local path without reparse points, owned by SYSTEM or Administrators, with access limited to those groups", path, err)
	}
	return nil
}

func check(path string, directory bool) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("reparse point")
	}
	if (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return errors.New("unexpected file type")
	}
	if !directory && info.NumberOfLinks != 1 {
		return errors.New("file has multiple hard links")
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return checkDescriptor(sd)
}

func privileged(sid *windows.SID) bool {
	return sid != nil && (sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}

func checkDescriptor(sd *windows.SECURITY_DESCRIPTOR) error {
	owner, _, err := sd.Owner()
	if err != nil || !privileged(owner) {
		return errors.New("untrusted owner")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return errors.New("missing or null DACL")
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) //nolint:gosec // GetAce returns the Windows layout with an inline SID
			if privileged(sid) {
				continue
			}
		}
		return errors.New("DACL contains an unsupported or untrusted access entry")
	}
	return nil
}
