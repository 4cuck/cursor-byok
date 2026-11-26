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

func readStoredMachineID(t *testing.T, store *AccountStore, id string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.accountsDir(), id+".json"))
	if err != nil {
		t.Fatalf("read account file: %v", err)
	}
	var rec accountRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse account file: %v", err)
	}
	return strings.TrimSpace(rec.MachineID)
}

func clearStoredMachineID(t *testing.T, store *AccountStore, id string) {
	t.Helper()
	path := filepath.Join(store.accountsDir(), id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec accountRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	rec.MachineID = ""
	writeTestJSON(t, path, rec)
}

// Each account gets its own stable machine id, generated once at creation and
// kept, so distinct accounts never share a device and an account never changes
// its device.
func TestMachineIDPerAccountStableAndDistinct(t *testing.T) {
	store := NewAccountStore(t.TempDir(), "")
	a, err := store.Upsert(distinctCreds("auth-a", "acc-a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Upsert(distinctCreds("auth-b", "acc-b"))
	if err != nil {
		t.Fatal(err)
	}

	idA, err := store.MachineIDForToken("acc-a", "auth-a")
	if err != nil || strings.TrimSpace(idA) == "" {
		t.Fatalf("expected machine id for account a, got %q err=%v", idA, err)
	}
	if again, _ := store.MachineIDForToken("acc-a", "auth-a"); again != idA {
		t.Fatalf("machine id for account a changed: %q -> %q", idA, again)
	}

	idB, err := store.MachineIDForToken("acc-b", "auth-b")
	if err != nil || strings.TrimSpace(idB) == "" {
		t.Fatalf("expected machine id for account b, got %q err=%v", idB, err)
	}
	if idA == idB {
		t.Fatal("distinct accounts must not share a machine id")
	}

	if got := readStoredMachineID(t, store, a.ID); got != idA {
		t.Fatalf("account a machine id not persisted: disk=%q resolved=%q", got, idA)
	}
	if got := readStoredMachineID(t, store, b.ID); got != idB {
		t.Fatalf("account b machine id not persisted: disk=%q resolved=%q", got, idB)
	}
}

// Accounts stored before machine ids existed get one generated and persisted on
// first use, and it stays constant afterwards ("only changed once").
func TestMachineIDBackfillsLegacyAccountOnce(t *testing.T) {
	store := NewAccountStore(t.TempDir(), "")
	a, err := store.Upsert(distinctCreds("auth-a", "acc-a"))
	if err != nil {
		t.Fatal(err)
	}
	clearStoredMachineID(t, store, a.ID)
	if got := readStoredMachineID(t, store, a.ID); got != "" {
		t.Fatalf("precondition failed, machine id still present: %q", got)
	}

	first, err := store.MachineIDForToken("acc-a", "auth-a")
	if err != nil || strings.TrimSpace(first) == "" {
		t.Fatalf("expected backfilled machine id, got %q err=%v", first, err)
	}
	if got := readStoredMachineID(t, store, a.ID); got != first {
		t.Fatalf("backfilled machine id not persisted: disk=%q resolved=%q", got, first)
	}
	if second, _ := store.MachineIDForToken("acc-a", "auth-a"); second != first {
		t.Fatalf("backfilled machine id changed on reuse: %q -> %q", first, second)
	}
}

// A token with no stored account resolves to empty so callers use the per-install id.
func TestMachineIDForTokenUnmatchedReturnsEmpty(t *testing.T) {
	store := NewAccountStore(t.TempDir(), "")
	if _, err := store.Upsert(distinctCreds("auth-a", "acc-a")); err != nil {
		t.Fatal(err)
	}
	id, err := store.MachineIDForToken("not-a-stored-token", "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Fatalf("unmatched token should resolve empty, got %q", id)
	}
}

// After Cursor rotates the access token, the account is still found by its JWT
// subject, so its machine id does not change.
func TestMachineIDMatchesBySubjectAfterTokenRotation(t *testing.T) {
	store := NewAccountStore(t.TempDir(), "")
	if _, err := store.Upsert(distinctCreds("auth-a", "acc-a")); err != nil {
		t.Fatal(err)
	}
	original, err := store.MachineIDForToken("acc-a", "auth-a")
	if err != nil || original == "" {
		t.Fatalf("expected machine id, got %q err=%v", original, err)
	}
	rotated, err := store.MachineIDForToken("rotated-token-not-on-disk", "auth-a")
	if err != nil {
		t.Fatal(err)
	}
	if rotated != original {
		t.Fatalf("machine id changed after token rotation: %q -> %q", original, rotated)
	}
}

func TestManagerMachineIDForAuthorization(t *testing.T) {
	manager := NewManager(t.TempDir(), "", nil)
	if _, err := manager.store.Upsert(distinctCreds("auth-a", "acc-a")); err != nil {
		t.Fatal(err)
	}

	id := manager.MachineIDForAuthorization("Bearer acc-a")
	if strings.TrimSpace(id) == "" {
		t.Fatal("expected a machine id for the stored account")
	}
	if again := manager.MachineIDForAuthorization("Bearer acc-a"); again != id {
		t.Fatalf("cached machine id changed: %q -> %q", id, again)
	}
	if got := manager.MachineIDForAuthorization("Bearer not-stored"); got != "" {
		t.Fatalf("unmatched authorization should resolve empty, got %q", got)
	}
}

// Creating a manager wires its resolver into the upstream checksum builder, so
// ChecksumMachineID returns the account's id for its bearer token.
func TestManagerRegistersUpstreamResolver(t *testing.T) {
	t.Cleanup(func() { upstream.SetMachineIDResolver(nil) })

	manager := NewManager(t.TempDir(), "", nil)
	if _, err := manager.store.Upsert(distinctCreds("auth-a", "acc-a")); err != nil {
		t.Fatal(err)
	}

	want := manager.MachineIDForAuthorization("Bearer acc-a")
	if want == "" {
		t.Fatal("expected a machine id for the stored account")
	}
	if got := upstream.ChecksumMachineID("Bearer acc-a"); got != want {
		t.Fatalf("upstream resolver not wired: got %q want %q", got, want)
	}
}
