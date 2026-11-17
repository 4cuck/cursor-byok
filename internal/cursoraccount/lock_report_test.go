package cursoraccount

import (
	"testing"
	"time"
)

// Compile-time proof that Manager satisfies the proxy's reporter interface.
// If this breaks, device-limit detection silently stops marking accounts locked.
type deviceLimitReporterContract interface {
	ReportDeviceLimit(reason string)
}

var _ deviceLimitReporterContract = (*Manager)(nil)

// A repeated failure must not extend the cooldown: the window is fixed once and
// hammering the API should not push it further out.
func TestReportDeviceLimitDoesNotExtendExistingLock(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root, "", nil)
	if _, err := manager.ImportToken(nil, "unused"); err == nil {
		t.Fatal("expected import to fail without a client")
	}

	summary, err := manager.store.Upsert(credentials{
		AccessToken:  "test-access",
		RefreshToken: "crsr_" + "c",
		AuthID:       "auth-device",
		Email:        "device@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.store.SetCurrent(summary.ID); err != nil {
		t.Fatal(err)
	}
	manager.adoptCurrentFromStore()

	firstUntil := time.Now().Add(2 * time.Hour)
	if _, err := manager.store.MarkAccountLocked(summary.ID, LockReasonDeviceLimit, firstUntil); err != nil {
		t.Fatal(err)
	}

	// A second report while already locked must leave the window untouched.
	manager.ReportDeviceLimit("device_limit")

	_, locked := manager.CurrentAccountLock()
	if !locked {
		t.Fatal("account should still be locked")
	}
	listed, err := manager.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("account count = %d", len(listed))
	}
	if got := listed[0].LockedUntilUnixMS; got != firstUntil.UnixMilli() {
		t.Fatalf("cooldown was extended: got %d, want %d", got, firstUntil.UnixMilli())
	}
}

// With no lock present, reporting must start the full 24h cooldown.
func TestReportDeviceLimitStartsCooldown(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root, "", nil)
	summary, err := manager.store.Upsert(credentials{
		AccessToken:  "test-access",
		RefreshToken: "crsr_" + "d",
		AuthID:       "auth-device-2",
		Email:        "device2@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.store.SetCurrent(summary.ID); err != nil {
		t.Fatal(err)
	}
	manager.adoptCurrentFromStore()

	before := time.Now()
	manager.ReportDeviceLimit("device_limit")

	current, locked := manager.CurrentAccountLock()
	if !locked {
		t.Fatal("account should be locked after reporting")
	}
	remaining := time.Until(time.UnixMilli(current.LockedUntilUnixMS))
	// Allow a little slack for execution time.
	if remaining < AccountDeviceCooldown-time.Minute || remaining > AccountDeviceCooldown+time.Minute {
		t.Fatalf("cooldown = %v, want about %v", remaining, AccountDeviceCooldown)
	}
	if current.LockedUntilUnixMS < before.Add(AccountDeviceCooldown-time.Minute).UnixMilli() {
		t.Fatal("cooldown ends too early")
	}
}