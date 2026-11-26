package cursor

import (
	"os"
	"strings"
	"testing"
)

func testAccountDeviceIdentity() CursorDeviceIdentity {
	return CursorDeviceIdentity{
		ServiceMachineID:   "11111111-1111-1111-1111-111111111111",
		MacMachineID:       strings.Repeat("a", 64),
		DevDeviceID:        "22222222-2222-2222-2222-222222222222",
		TelemetryMachineID: strings.Repeat("b", 64),
		SqmID:              "{AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE}",
	}
}

func originalDeviceState() map[string]string {
	return map[string]string{
		cursorStateServiceMachineIDKey: "orig-service-machine-id",
		cursorTelemetryMachineIDKey:    "orig-telemetry-machine-id",
		cursorTelemetryMacMachineIDKey: "orig-mac-machine-id",
		cursorTelemetryDevDeviceIDKey:  "orig-dev-device-id",
		cursorTelemetrySqmIDKey:        "{ORIGINAL-SQM}",
	}
}

func assertStateValues(t *testing.T, statePath string, want map[string]string) {
	t.Helper()
	for key, value := range want {
		got, ok := readCursorAuthValue(t, statePath, key)
		if !ok {
			t.Fatalf("expected key %q present", key)
		}
		if got != value {
			t.Fatalf("key %q: got %q, want %q", key, got, value)
		}
	}
}

// Injecting a device identity overwrites the host's serviceMachineId + telemetry
// block, snapshots the originals once, and RestoreCursorUserInfo puts them back.
func TestApplyAndRestoreCursorDeviceIdentityRoundTrip(t *testing.T) {
	statePath := newTestCursorStateDB(t, originalDeviceState())
	account := testAccountDeviceIdentity()

	if err := ApplyCursorDeviceIdentity(statePath, account); err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertStateValues(t, statePath, map[string]string{
		cursorStateServiceMachineIDKey: account.ServiceMachineID,
		cursorTelemetryMachineIDKey:    account.TelemetryMachineID,
		cursorTelemetryMacMachineIDKey: account.MacMachineID,
		cursorTelemetryDevDeviceIDKey:  account.DevDeviceID,
		cursorTelemetrySqmIDKey:        account.SqmID,
	})

	backup, err := os.ReadFile(cursorDeviceBackupPath())
	if err != nil {
		t.Fatalf("device backup not written: %v", err)
	}
	for _, original := range originalDeviceState() {
		if !strings.Contains(string(backup), original) {
			t.Fatalf("device backup missing original %q: %s", original, backup)
		}
	}

	if err := RestoreCursorUserInfo(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertStateValues(t, statePath, originalDeviceState())
}

// The original snapshot is captured once: a second injection must not record the
// first account's values as the "original" to restore.
func TestBackupCursorDeviceIdentityCapturedOnce(t *testing.T) {
	statePath := newTestCursorStateDB(t, originalDeviceState())

	first := testAccountDeviceIdentity()
	if err := ApplyCursorDeviceIdentity(statePath, first); err != nil {
		t.Fatalf("apply first: %v", err)
	}
	second := CursorDeviceIdentity{
		ServiceMachineID:   "99999999-9999-9999-9999-999999999999",
		MacMachineID:       strings.Repeat("c", 64),
		DevDeviceID:        "88888888-8888-8888-8888-888888888888",
		TelemetryMachineID: strings.Repeat("d", 64),
		SqmID:              "{SECOND-SQM}",
	}
	if err := ApplyCursorDeviceIdentity(statePath, second); err != nil {
		t.Fatalf("apply second: %v", err)
	}

	// Restore must return the host's ORIGINAL identity, not the first account's.
	if err := RestoreCursorUserInfo(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertStateValues(t, statePath, originalDeviceState())
}

// Keys that did not exist on the host originally are removed on restore, rather
// than left pointing at an account's injected value.
func TestRestoreCursorDeviceIdentityRemovesKeysAbsentOriginally(t *testing.T) {
	// Start with a state DB that has no device keys at all.
	statePath := newTestCursorStateDB(t, map[string]string{
		"cursorAuth/accessToken": "x",
	})
	account := testAccountDeviceIdentity()
	if err := ApplyCursorDeviceIdentity(statePath, account); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got, ok := readCursorAuthValue(t, statePath, cursorStateServiceMachineIDKey); !ok || got != account.ServiceMachineID {
		t.Fatalf("expected injected serviceMachineId, got %q ok=%v", got, ok)
	}

	if err := RestoreCursorUserInfo(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, key := range cursorDeviceIdentityKeys {
		if _, ok := readCursorAuthValue(t, statePath, key); ok {
			t.Fatalf("key %q should have been removed on restore", key)
		}
	}
}

// An empty identity is a no-op: no telemetry overwrite and no backup created.
func TestApplyCursorDeviceIdentityEmptyIsNoop(t *testing.T) {
	statePath := newTestCursorStateDB(t, originalDeviceState())
	if err := ApplyCursorDeviceIdentity(statePath, CursorDeviceIdentity{}); err != nil {
		t.Fatalf("apply empty: %v", err)
	}
	if _, err := os.Stat(cursorDeviceBackupPath()); !os.IsNotExist(err) {
		t.Fatalf("no device backup should be created for an empty identity, stat err=%v", err)
	}
	assertStateValues(t, statePath, originalDeviceState())
}
