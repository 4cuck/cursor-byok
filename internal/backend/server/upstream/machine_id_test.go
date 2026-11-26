package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A per-install id must be generated once and then reused from disk, so a
// restart keeps presenting the same single device to Cursor.
func TestLoadOrCreateIDPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine-id")

	first := loadOrCreateID(path, func() string { return "generated-id" })
	if first != "generated-id" {
		t.Fatalf("expected the generated id, got %q", first)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("id file was not written: %v", err)
	}
	if strings.TrimSpace(string(data)) != "generated-id" {
		t.Fatalf("persisted id %q != returned id %q", strings.TrimSpace(string(data)), first)
	}

	// A second call must reuse the persisted value, never calling gen again.
	second := loadOrCreateID(path, func() string { return "different-id" })
	if second != "generated-id" {
		t.Fatalf("install id changed on reload: %q", second)
	}
}

// The install identity is a stable machine id + macMachineId pair.
func TestInstallChecksumIdentityIsAPair(t *testing.T) {
	identity := InstallChecksumIdentity()
	if strings.TrimSpace(identity.MachineID) == "" || strings.TrimSpace(identity.MacMachineID) == "" {
		t.Fatalf("install identity must have both ids, got %+v", identity)
	}
	if identity.MachineID == identity.MacMachineID {
		t.Fatal("machineId and macMachineId must differ")
	}
	if InstallMachineID() != identity.MachineID {
		t.Fatal("InstallMachineID must match the pair's MachineID")
	}
}

// checksumIdentityFor prefers the per-account resolver and only falls back to the
// per-install pair when the resolver yields nothing.
func TestChecksumIdentityUsesResolverThenFallback(t *testing.T) {
	t.Cleanup(func() { SetMachineIDResolver(nil) })

	SetMachineIDResolver(func(authorization string) ChecksumIdentity {
		if authorization == "Bearer known" {
			return ChecksumIdentity{MachineID: "account-machine", MacMachineID: "account-mac"}
		}
		return ChecksumIdentity{}
	})

	if got := checksumIdentityFor("Bearer known"); got.MachineID != "account-machine" || got.MacMachineID != "account-mac" {
		t.Fatalf("resolver identity not used, got %+v", got)
	}
	if got := checksumIdentityFor("Bearer unknown"); got != InstallChecksumIdentity() {
		t.Fatalf("unmatched token should fall back to install identity, got %+v", got)
	}

	SetMachineIDResolver(nil)
	if got := checksumIdentityFor("Bearer whatever"); got != InstallChecksumIdentity() {
		t.Fatalf("nil resolver should fall back to install identity, got %+v", got)
	}
}

// The full checksum must append /macMachineId after the machine id.
func TestBuildCursorChecksumAppendsMacMachineID(t *testing.T) {
	t.Cleanup(func() { SetMachineIDResolver(nil) })
	SetMachineIDResolver(func(string) ChecksumIdentity {
		return ChecksumIdentity{MachineID: "mid", MacMachineID: "macid"}
	})

	got := BuildCursorChecksum("Bearer token")
	if !strings.HasSuffix(got, "mid/macid") {
		t.Fatalf("checksum %q must end with machineId/macMachineId", got)
	}
}

// The explicit builder covers all three degrade paths.
func TestBuildCursorChecksumWithMachineIDsForms(t *testing.T) {
	if got := BuildCursorChecksumWithMachineIDs("m", "mac"); !strings.HasSuffix(got, "m/mac") {
		t.Fatalf("both ids: %q must end with m/mac", got)
	}
	machineOnly := BuildCursorChecksumWithMachineIDs("m", "")
	if !strings.HasSuffix(machineOnly, "m") || strings.Contains(machineOnly, "/") {
		t.Fatalf("machine-only form wrong: %q", machineOnly)
	}
	emptyMachine := BuildCursorChecksumWithMachineIDs("", "mac")
	if strings.Contains(emptyMachine, "/") || strings.Contains(emptyMachine, "mac") {
		t.Fatalf("empty machine id must degrade to the timestamp prefix: %q", emptyMachine)
	}
}
