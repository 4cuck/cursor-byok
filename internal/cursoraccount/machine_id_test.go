package cursoraccount

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cursor/internal/backend/server/upstream"
)

func distinctCreds(authID, token string) credentials {
	return credentials{
		AccessToken:  token,
		RefreshToken: "ref-" + token,
		AuthID:       authID,
		Email:        authID + "@example.test",
	}
}

func readStoredRecord(t *testing.T, store *AccountStore, id string) accountRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.accountsDir(), id+".json"))
	if err != nil {
		t.Fatalf("read account file: %v", err)
	}
	var rec accountRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse account file: %v", err)
	}
	return rec
}

func clearStoredDeviceIdentity(t *testing.T, store *AccountStore, id string) {
	t.Helper()
	path := filepath.Join(store.accountsDir(), id+".json")
	rec := readStoredRecord(t, store, id)
	rec.MachineID = ""
	rec.MacMachineID = ""
	rec.DevDeviceID = ""
	rec.TelemetryMachineID = ""
	rec.SqmID = ""
	writeTestJSON(t, path, rec)
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func assertWellFormedDeviceIdentity(t *testing.T, id DeviceIdentity) {
	t.Helper()
	if !strings.Contains(id.ServiceMachineID, "-") || len(id.ServiceMachineID) != 36 {
		t.Fatalf("serviceMachineId is not a uuid: %q", id.ServiceMachineID)
	}
	if !strings.Contains(id.DevDeviceID, "-") || len(id.DevDeviceID) != 36 {
		t.Fatalf("devDeviceId is not a uuid: %q", id.DevDeviceID)
	}
	if !isHex64(id.MacMachineID) {
		t.Fatalf("macMachineId is not 64-hex: %q", id.MacMachineID)
	}
	if !isHex64(id.TelemetryMachineID) {
		t.Fatalf("telemetry machineId is not 64-hex: %q", id.TelemetryMachineID)
	}
	if !strings.HasPrefix(id.SqmID, "{") || !strings.HasSuffix(id.SqmID, "}") {
		t.Fatalf("sqmId is not a braced guid: %q", id.SqmID)
	}
}

// Each account gets its own well-formed device identity, generated once at
// creation and kept, and distinct accounts never share any identifier.
func TestDeviceIdentityPerAccountStableDistinctWellFormed(t *testing.T) {
	store := NewAccountStore(t.TempDir(), "")
	a, err := store.Upsert(distinctCreds("auth-a", "acc-a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Upsert(distinctCreds("auth-b", "acc-b"))
	if err != nil {
		t.Fatal(err)
	}

	idA, found, err := store.DeviceIdentityForToken("acc-a", "auth-a")
	if err != nil || !found {
		t.Fatalf("expected identity for account a, found=%v err=%v", found, err)
	}
	assertWellFormedDeviceIdentity(t, idA)
	again, _, _ := store.DeviceIdentityForToken("acc-a", "auth-a")
	if again != idA {
		t.Fatalf("identity for account a changed: %+v -> %+v", idA, again)
	}

	idB, found, err := store.DeviceIdentityForToken("acc-b", "auth-b")
	if err != nil || !found {
		t.Fatalf("expected identity for account b, found=%v err=%v", found, err)
	}
	assertWellFormedDeviceIdentity(t, idB)

	if idA.ServiceMachineID == idB.ServiceMachineID || idA.MacMachineID == idB.MacMachineID ||
		idA.DevDeviceID == idB.DevDeviceID || idA.TelemetryMachineID == idB.TelemetryMachineID || idA.SqmID == idB.SqmID {
		t.Fatalf("distinct accounts must not share any device id: %+v vs %+v", idA, idB)
	}

	// Persisted to disk.
	recA := readStoredRecord(t, store, a.ID)
	if recA.MachineID != idA.ServiceMachineID || recA.MacMachineID != idA.MacMachineID || recA.SqmID != idA.SqmID {
		t.Fatalf("account a identity not persisted: %+v", recA)
	}
	_ = b
}

// Accounts stored before device identities existed get them generated and
// persisted on first use, and they stay constant afterwards ("set once").
func TestDeviceIdentityBackfillsLegacyAccountOnce(t *testing.T) {
	store := NewAccountStore(t.TempDir(), "")
	a, err := store.Upsert(distinctCreds("auth-a", "acc-a"))
	if err != nil {
		t.Fatal(err)
	}
	clearStoredDeviceIdentity(t, store, a.ID)
	if rec := readStoredRecord(t, store, a.ID); rec.MachineID != "" || rec.MacMachineID != "" {
		t.Fatalf("precondition failed, identity still present: %+v", rec)
	}

	first, found, err := store.DeviceIdentityForToken("acc-a", "auth-a")
	if err != nil || !found {
		t.Fatalf("expected backfilled identity, found=%v err=%v", found, err)
	}
	assertWellFormedDeviceIdentity(t, first)
	if rec := readStoredRecord(t, store, a.ID); rec.MachineID != first.ServiceMachineID {
		t.Fatalf("backfilled identity not persisted: %+v", rec)
	}
	if second, _, _ := store.DeviceIdentityForToken("acc-a", "auth-a"); second != first {
		t.Fatalf("backfilled identity changed on reuse: %+v -> %+v", first, second)
	}
}

func TestDeviceIdentityForTokenUnmatched(t *testing.T) {
	store := NewAccountStore(t.TempDir(), "")
	if _, err := store.Upsert(distinctCreds("auth-a", "acc-a")); err != nil {
		t.Fatal(err)
	}
	_, found, err := store.DeviceIdentityForToken("not-a-stored-token", "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("unmatched token should not resolve an identity")
	}
}

// After Cursor rotates the access token, the account is still found by its JWT
// subject, so its device identity does not change.
func TestDeviceIdentityMatchesBySubjectAfterTokenRotation(t *testing.T) {
	store := NewAccountStore(t.TempDir(), "")
	if _, err := store.Upsert(distinctCreds("auth-a", "acc-a")); err != nil {
		t.Fatal(err)
	}
	original, _, err := store.DeviceIdentityForToken("acc-a", "auth-a")
	if err != nil {
		t.Fatal(err)
	}
	rotated, found, err := store.DeviceIdentityForToken("rotated-token-not-on-disk", "auth-a")
	if err != nil || !found {
		t.Fatalf("subject match failed, found=%v err=%v", found, err)
	}
	if rotated != original {
		t.Fatalf("identity changed after token rotation: %+v -> %+v", original, rotated)
	}
}

func TestEnsureDeviceIdentityPersistsAndIsStable(t *testing.T) {
	store := NewAccountStore(t.TempDir(), "")
	a, err := store.Upsert(distinctCreds("auth-a", "acc-a"))
	if err != nil {
		t.Fatal(err)
	}
	clearStoredDeviceIdentity(t, store, a.ID)
	first, err := store.EnsureDeviceIdentity(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertWellFormedDeviceIdentity(t, first)
	second, err := store.EnsureDeviceIdentity(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("EnsureDeviceIdentity is not stable: %+v -> %+v", first, second)
	}
}

func TestManagerChecksumIdentityForAuthorization(t *testing.T) {
	manager := NewManager(t.TempDir(), "", nil)
	if _, err := manager.store.Upsert(distinctCreds("auth-a", "acc-a")); err != nil {
		t.Fatal(err)
	}

	identity := manager.ChecksumIdentityForAuthorization("Bearer acc-a")
	if strings.TrimSpace(identity.MachineID) == "" || strings.TrimSpace(identity.MacMachineID) == "" {
		t.Fatalf("expected a full checksum identity, got %+v", identity)
	}
	if again := manager.ChecksumIdentityForAuthorization("Bearer acc-a"); again != identity {
		t.Fatalf("cached identity changed: %+v -> %+v", identity, again)
	}
	if got := manager.ChecksumIdentityForAuthorization("Bearer not-stored"); got != (upstream.ChecksumIdentity{}) {
		t.Fatalf("unmatched authorization should resolve empty, got %+v", got)
	}
}

func TestManagerCurrentDeviceIdentity(t *testing.T) {
	manager := NewManager(t.TempDir(), "", nil)
	if _, ok := manager.CurrentDeviceIdentity(); ok {
		t.Fatal("no current account should yield no identity")
	}
	summary, err := manager.SeedAccountForTest("auth-a", "a@example.test", true)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := manager.CurrentDeviceIdentity()
	if !ok {
		t.Fatal("expected a current device identity")
	}
	assertWellFormedDeviceIdentity(t, identity)
	if rec := readStoredRecord(t, manager.store, summary.ID); rec.MachineID != identity.ServiceMachineID {
		t.Fatalf("current identity not persisted: %+v", rec)
	}
}

// Creating a manager wires its resolver into the upstream checksum builder, so
// BuildCursorChecksum ends with the account's machineId/macMachineId.
func TestManagerRegistersUpstreamResolver(t *testing.T) {
	t.Cleanup(func() { upstream.SetMachineIDResolver(nil) })

	manager := NewManager(t.TempDir(), "", nil)
	if _, err := manager.store.Upsert(distinctCreds("auth-a", "acc-a")); err != nil {
		t.Fatal(err)
	}

	identity := manager.ChecksumIdentityForAuthorization("Bearer acc-a")
	if identity.MachineID == "" || identity.MacMachineID == "" {
		t.Fatal("expected a full identity for the stored account")
	}
	got := upstream.BuildCursorChecksum("Bearer acc-a")
	if !strings.HasSuffix(got, identity.MachineID+"/"+identity.MacMachineID) {
		t.Fatalf("upstream resolver not wired: %q must end with %s/%s", got, identity.MachineID, identity.MacMachineID)
	}
}
