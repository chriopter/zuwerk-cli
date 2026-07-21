package main

import (
	"bytes"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"version"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", exitCode, stderr.String())
	}
	if got, want := stdout.String(), "zuwerk 0.0.1\n"; got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}
