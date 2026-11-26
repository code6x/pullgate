package winsecure

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

const maxTokenBytes = 4096
const maxProtectedBytes = 64 << 10

// ProtectToken encrypts a token for this machine. The caller must store the
// result under a checked ACL. Machine DPAPI does not restrict decryption to an
// individual Windows account.
func ProtectToken(token []byte) ([]byte, error) {
	if len(token) == 0 || len(token) > maxTokenBytes {
		return nil, errors.New("invalid bot token length. Fix: enter the bot token again")
	}
	in := windows.DataBlob{Size: uint32(len(token)), Data: &token[0]} //nolint:gosec // bounded above
	var out windows.DataBlob
	err := windows.CryptProtectData(&in, nil, nil, 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN|windows.CRYPTPROTECT_LOCAL_MACHINE, &out)
	if err != nil {
		return nil, errors.New("cannot protect the bot token with DPAPI. Fix: run token setup on this machine")
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) }() //nolint:gosec // DPAPI allocates with LocalAlloc
	if out.Size == 0 || out.Size > maxProtectedBytes || out.Data == nil {
		return nil, errors.New("invalid protected bot token size. Fix: run token setup again")
	}
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil //nolint:gosec // DPAPI returns this pointer and length together
}

// UnprotectToken decrypts a token read from a file with a checked ACL.
func UnprotectToken(protected []byte) ([]byte, error) {
	if len(protected) == 0 || len(protected) > maxProtectedBytes {
		return nil, errors.New("invalid protected bot token size. Fix: run token setup again")
	}
	in := windows.DataBlob{Size: uint32(len(protected)), Data: &protected[0]} //nolint:gosec // bounded above
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, errors.New("cannot decrypt the bot token. Fix: run token setup on this machine")
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) }() //nolint:gosec // DPAPI allocates with LocalAlloc
	if out.Data == nil {
		return nil, errors.New("missing decrypted bot token. Fix: run token setup again")
	}
	plain := unsafe.Slice(out.Data, out.Size) //nolint:gosec // DPAPI returns this pointer and length together
	defer clear(plain)
	if len(plain) == 0 || len(plain) > maxTokenBytes {
		return nil, errors.New("invalid bot token length. Fix: run token setup again")
	}
	return append([]byte(nil), plain...), nil
}
