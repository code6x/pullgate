// Package winsecure checks Windows file security and wraps machine DPAPI.
package winsecure

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

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
	defer windows.CloseHandle(h)
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
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if privileged(sid) {
				continue
			}
		}
		return errors.New("DACL contains an unsupported or untrusted access entry")
	}
	return nil
}
