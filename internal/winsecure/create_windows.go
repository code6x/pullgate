package winsecure

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func privateAttributes() (*windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read the process identity: %w. Fix: run as SYSTEM or an elevated administrator", err)
	}
	owner := "BA"
	if user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		owner = "SY"
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + owner + "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}, nil
}

// Mkdir creates one directory with a private ACL. The parent must already
// be trusted.
func Mkdir(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa, err := privateAttributes()
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(name, sa); err != nil {
		return fmt.Errorf("make %s: %w. Fix: run as SYSTEM or an elevated administrator and check the parent ACL", path, err)
	}
	return nil
}

// CreateExclusive creates a private file and fails if the path exists.
func CreateExclusive(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa, err := privateAttributes()
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w. Fix: run as SYSTEM or an elevated administrator and check the parent ACL", path, err)
	}
	return os.NewFile(uintptr(h), path), nil
}

// CreateTemp creates a private temporary file in an already trusted directory.
func CreateTemp(dir string) (*os.File, error) {
	var b [16]byte
	rand.Read(b[:])
	return CreateExclusive(filepath.Join(dir, ".tmp-"+hex.EncodeToString(b[:])))
}
