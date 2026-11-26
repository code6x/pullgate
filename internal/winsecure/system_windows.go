package winsecure

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// ProgramRoot finds the machine's data directory using the Windows known
// folder API and confirms ordinary users cannot redirect or replace it.
func ProgramRoot() (string, error) {
	data, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return "", fmt.Errorf("find ProgramData: %w. Fix: check the Windows known folder settings", err)
	}
	if err := checkSystemPath(data, true, true); err != nil {
		return "", err
	}
	return filepath.Join(data, "pullgate"), nil
}

// CheckSystemPath validates a Windows-installed path with public read
// access. Directories above it can allow child creation but must protect
// existing children from replacement.
func CheckSystemPath(path string, directory bool) error {
	return checkSystemPath(path, directory, false)
}

func checkSystemPath(path string, directory, dataRoot bool) error {
	volume := filepath.VolumeName(path)
	if _, err := relativeParts(path, path); err != nil {
		return err
	}
	root := volume + `\`
	drive, err := windows.UTF16PtrFromString(root)
	if err != nil || windows.GetDriveType(drive) != windows.DRIVE_FIXED {
		return fmt.Errorf("check system path %s: use a fixed local drive", path)
	}
	for _, item := range pathChain(root, path) {
		dir := item != path || directory
		// Trusted system files may be hard-linked. NTFS applies the
		// checked ACL to the file through every one of its names.
		if err := checkWithPolicy(item, dir, false, func(sd *windows.SECURITY_DESCRIPTOR) error {
			return checkSystemDescriptor(sd, item == root || (dataRoot && item == path))
		}); err != nil {
			return fmt.Errorf("check system path %s: %w. Fix: restore the Windows ACL or use an administrator-controlled installation", item, err)
		}
	}
	return nil
}

func pathChain(root, path string) []string {
	chain := []string{path}
	for path != root {
		path = filepath.Dir(path)
		chain = append(chain, path)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

func checkSystemDescriptor(sd *windows.SECURITY_DESCRIPTOR, allowChildCreation bool) error {
	// Service identity used by Windows for protected system files.
	installer, _, _, _ := windows.LookupSID("", `NT SERVICE\TrustedInstaller`)
	trusted := func(sid *windows.SID) bool {
		return privileged(sid) || (sid != nil && installer != nil && sid.Equals(installer))
	}
	allowed := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE | windows.GENERIC_READ | windows.GENERIC_EXECUTE)
	if allowChildCreation {
		// Default ProgramData ACLs let users create their own children.
		// DELETE, FILE_DELETE_CHILD, WRITE_OWNER and WRITE_DAC remain forbidden.
		allowed |= windows.FILE_GENERIC_WRITE | windows.GENERIC_WRITE
	}
	return checkAccess(sd, trusted, allowed, true)
}
