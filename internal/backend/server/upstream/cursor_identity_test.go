package upstream

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Cursor builds x-cursor-checksum as base64(timestamp) + machineId, read from
// workbench.desktop.main.js (hj0). It is NOT a hash of the Authorization header.
// Getting this wrong makes the server answer model calls with
// ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT ("Update Required").
func TestBuildCursorChecksumAppendsMachineID(t *testing.T) {
	const machineID = "e0d56722-04c6-4be7-8e31-b88972515264"

	got := BuildCursorChecksumWithMachineID(machineID)
	if !strings.HasSuffix(got, machineID) {
		t.Fatalf("checksum %q must end with the machine id %q", got, machineID)
	}

	prefix := strings.TrimSuffix(got, machineID)
	if prefix == "" {
		t.Fatal("checksum is missing the timestamp prefix")
	}
	// The prefix is base64 of 6 bytes, so it decodes to exactly 6 bytes.
	raw, err := base64.StdEncoding.DecodeString(prefix + strings.Repeat("=", (4-len(prefix)%4)%4))
	if err != nil {
		t.Fatalf("prefix %q is not valid base64: %v", prefix, err)
	}
	if len(raw) != 6 {
		t.Fatalf("prefix decodes to %d bytes, want 6", len(raw))
	}
}

// An empty machine id must degrade to the timestamp prefix rather than emitting
// a trailing separator.
func TestBuildCursorChecksumWithoutMachineID(t *testing.T) {
	got := BuildCursorChecksumWithMachineID("")
	if got == "" {
		t.Fatal("checksum must not be empty")
	}
	if strings.Contains(got, "/") {
		t.Fatalf("checksum %q must not contain a separator when no machine id is set", got)
	}
}

// The legacy formula must differ, so a regression back to sha256 is caught.
func TestLegacyChecksumDiffersFromCursorFormula(t *testing.T) {
	const machineID = "e0d56722-04c6-4be7-8e31-b88972515264"
	authorization := "Bearer test-token"

	current := BuildCursorChecksumWithMachineID(machineID)
	legacy := legacyBuildCursorChecksum(authorization)

	if current == legacy {
		t.Fatal("the machineId checksum must differ from the sha256 checksum")
	}
	if strings.HasSuffix(current, machineID) == strings.HasSuffix(legacy, machineID) {
		t.Fatal("only the current formula should end with the machine id")
	}
}

// The version must never fall back to a hardcoded ancient build when detection
// fails; the fallback should track the current client line.
func TestCursorClientVersionFallbackIsRecent(t *testing.T) {
	version := cursorClientVersionFallback
	if !strings.HasPrefix(version, "3.") {
		t.Fatalf("fallback version %q is not a 3.x client version", version)
	}
	if version == "3.14.7" {
		t.Fatal("fallback must not be the stale 3.14.7 value")
	}
}