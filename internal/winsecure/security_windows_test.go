package winsecure

import (
	"bytes"
	"crypto/rand"
	"testing"

	"golang.org/x/sys/windows"
)

func TestDescriptor(t *testing.T) {
	tests := []struct {
		name, sddl string
		ok         bool
	}{
		{"system and administrators", "O:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", true},
		{"untrusted owner", "O:BUD:P(A;;FA;;;SY)", false},
		{"missing DACL", "O:SY", false},
		{"null DACL", "O:SYD:NO_ACCESS_CONTROL", false},
		{"users can read", "O:BAD:P(A;;FA;;;BA)(A;;FR;;;BU)", false},
		{"users inherit access", "O:SYD:P(A;;FA;;;SY)(A;OICIIO;FA;;;BU)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tt.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkDescriptor(sd); (err == nil) != tt.ok {
				t.Fatalf("checkDescriptor returned %v, want success %v", err, tt.ok)
			}
		})
	}
}

func TestDPAPI(t *testing.T) {
	token := make([]byte, 32)
	rand.Read(token)
	protected, err := ProtectToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(protected, token) {
		t.Fatal("protected blob contains the token")
	}
	plain, err := UnprotectToken(protected)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(plain)
	if !bytes.Equal(plain, token) {
		t.Fatal("decrypted token differs")
	}
	if _, err := UnprotectToken(protected[:len(protected)/2]); err == nil {
		t.Fatal("accepted a truncated DPAPI blob")
	}
	if _, err := ProtectToken(nil); err == nil {
		t.Fatal("accepted an empty token")
	}
}
