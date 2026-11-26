package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The per-install id must be generated once and then reused from disk, so a
// restart keeps presenting the same single device to Cursor.
func TestLoadOrCreateInstallMachineIDPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine-id")

	first := loadOrCreateInstallMachineID(path)
	if strings.TrimSpace(first) == "" {
		t.Fatal("expected a generated machine id")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("machine id file was not written: %v", err)
	}
	if strings.TrimSpace(string(data)) != first {
		t.Fatalf("persisted id %q != returned id %q", strings.TrimSpace(string(data)), first)
	}

	second := loadOrCreateInstallMachineID(path)
	if second != first {
		t.Fatalf("install id changed on reload: %q -> %q", first, second)
	}

	other := loadOrCreateInstallMachineID(filepath.Join(t.TempDir(), "machine-id"))
	if other == first {
		t.Fatal("a fresh path must get its own random id")
	}
}

// ChecksumMachineID prefers the per-account resolver and only falls back to the
// per-install id when the resolver yields nothing. Cursor's serviceMachineId is
// never consulted.
func TestChecksumMachineIDUsesResolverThenFallback(t *testing.T) {
	t.Cleanup(func() { SetMachineIDResolver(nil) })

	SetMachineIDResolver(func(authorization string) string {
		if authorization == "Bearer known" {
			return "account-machine-id"
		}
		return ""
	})

	if got := ChecksumMachineID("Bearer known"); got != "account-machine-id" {
		t.Fatalf("resolver id not used, got %q", got)
	}
	if got := ChecksumMachineID("Bearer unknown"); got != InstallMachineID() {
		t.Fatalf("unmatched token should fall back to install id, got %q want %q", got, InstallMachineID())
	}

	SetMachineIDResolver(nil)
	if got := ChecksumMachineID("Bearer whatever"); got != InstallMachineID() {
		t.Fatalf("nil resolver should fall back to install id, got %q", got)
	}
}

// The full checksum must end with the resolved machine id so Cursor accepts
// model traffic.
func TestBuildCursorChecksumUsesResolvedMachineID(t *testing.T) {
	t.Cleanup(func() { SetMachineIDResolver(nil) })
	SetMachineIDResolver(func(string) string { return "resolved-id" })

	got := BuildCursorChecksum("Bearer token")
	if !strings.HasSuffix(got, "resolved-id") {
		t.Fatalf("checksum %q must end with the resolved machine id", got)
	}
}
