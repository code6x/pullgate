package main

import (
	"bytes"
	"strings"
	"testing"
)

func runArgs(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersionForms(t *testing.T) {
	old := version
	version = "1.2.3"
	t.Cleanup(func() { version = old })
	cases := [][]string{
		{"version"},
		{"--version"},
		{"-version"},
		{"--config-dir", "/x", "--version"},
		{"--config-dir=/x", "--state-dir", "/y", "--spool-dir", "/z", "-version"},
		// --version wins over a command: it reads no config.
		{"--version", "run", "example"},
	}
	for _, args := range cases {
		code, out, errOut := runArgs(t, args...)
		if code != 0 || out != "pullgate 1.2.3\n" || errOut != "" {
			t.Errorf("pullgate %v: exit %d, stdout %q, stderr %q; want exit 0, stdout %q, no stderr",
				args, code, out, errOut, "pullgate 1.2.3\n")
		}
	}
}

func TestVersionDeny(t *testing.T) {
	cases := [][]string{
		{"--version=maybe"},           // not a bool
		{"--versions"},                // unknown flag
		{"--config-dir", "--version"}, // --version is the value of --config-dir, no command
		// --version is a global flag: after a command it is an unknown argument.
		{"run", "example", "--version"},
		{"status", "example", "--version"},
		{"listen", "--version"},
	}
	for _, args := range cases {
		code, out, errOut := runArgs(t, args...)
		if code != 64 || out != "" || !strings.Contains(errOut, "pullgate --help") {
			t.Errorf("pullgate %v: exit %d, stdout %q, stderr %q; want exit 64, no stdout, a hint", args, code, out, errOut)
		}
	}
	// --version=false is no version request: a command is still needed.
	code, out, _ := runArgs(t, "--version=false")
	if code != 64 || out != "" {
		t.Errorf("pullgate --version=false: exit %d, stdout %q; want exit 64, no stdout", code, out)
	}
}

func TestHelpListsVersion(t *testing.T) {
	for _, h := range []string{"--help", "-h"} {
		code, out, _ := runArgs(t, h)
		if code != 0 {
			t.Fatalf("pullgate %s: exit %d, want 0", h, code)
		}
		if !strings.Contains(out, "--version") || !strings.Contains(out, "\n  version") {
			t.Errorf("pullgate %s does not list --version and version:\n%s", h, out)
		}
	}
}
