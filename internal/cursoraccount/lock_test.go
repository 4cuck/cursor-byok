package cursoraccount

import (
	"testing"
	"time"
)

// Cursor's device cap surfaces as free text, not a status code, so detection has
// to match the wording it actually returns.
func TestIsDeviceLimitError(t *testing.T) {
	locked := []string{
		`{"error":"Too many computers."}`,
		`ERROR_CUSTOM_MESSAGE: Too many computers used within the last 24 hours for the same Cursor account.`,
		`too many devices`,
		`{"error":"device_limit_exceeded"}`,
	}
	for _, text := range locked {
		if !IsDeviceLimitError(text) {
			t.Fatalf("expected device-limit match for %q", text)
		}
	}

	unlocked := []string{
		``,
		`ERROR_NOT_LOGGED_IN`,
		`rate limit exceeded`,
		`invalid api key`,
	}
	for _, text := range unlocked {
		if IsDeviceLimitError(text) {
			t.Fatalf("false positive for %q", text)
		}
	}
}

func TestIsLockedRespectsCooldownWindow(t *testing.T) {
	now := time.Now()

	active := CursorAccountSummary{LockedUntilUnixMS: now.Add(time.Hour).UnixMilli()}
	if !active.IsLocked(now) {
		t.Fatal("account inside the window must report locked")
	}
	if remaining := active.LockedRemaining(now); remaining <= 0 {
		t.Fatalf("remaining = %v, want > 0", remaining)
	}

	expired := CursorAccountSummary{LockedUntilUnixMS: now.Add(-time.Minute).UnixMilli()}
	if expired.IsLocked(now) {
		t.Fatal("account past the window must report unlocked")
	}

	never := CursorAccountSummary{}
	if never.IsLocked(now) {
		t.Fatal("account with no lock fields must report unlocked")
	}
}

// A lock must survive a store round-trip: the whole point is that restarting the
// app does not hand the user a fresh attempt.
func TestAccountLockPersistsAcrossReload(t *testing.T) {
	root := t.TempDir()
	store := NewAccountStore(root, "")
	summary, err := store.Upsert(credentials{
		AccessToken:  "test-access",
		RefreshToken: "crsr_" + "a",
		AuthID:       "auth-lock",
		Email:        "lock@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}

	until := time.Now().Add(AccountDeviceCooldown)
	if _, err := store.MarkAccountLocked(summary.ID, LockReasonDeviceLimit, until); err != nil {
		t.Fatal(err)
	}

	// A fresh store instance simulates an app restart.
	reloaded := NewAccountStore(root, "")
	listed, err := reloaded.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("account count = %d, want 1", len(listed))
	}
	if !listed[0].IsLocked(time.Now()) {
		t.Fatal("lock did not persist across reload")
	}
	if listed[0].LockReason != LockReasonDeviceLimit {
		t.Fatalf("lock reason = %q", listed[0].LockReason)
	}
	if got := listed[0].LockedUntilUnixMS; got != until.UnixMilli() {
		t.Fatalf("lockedUntil = %d, want %d", got, until.UnixMilli())
	}
}

// The explicit "try anyway" action must fully clear the cooldown.
func TestClearAccountLockRemovesCooldown(t *testing.T) {
	root := t.TempDir()
	store := NewAccountStore(root, "")
	summary, err := store.Upsert(credentials{
		AccessToken:  "test-access",
		RefreshToken: "crsr_" + "b",
		AuthID:       "auth-clear",
		Email:        "clear@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkAccountLocked(summary.ID, LockReasonDeviceLimit, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cleared, err := store.ClearAccountLock(summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.IsLocked(time.Now()) {
		t.Fatal("account must be unlocked after clearing")
	}
	if cleared.LockedUntilUnixMS != 0 || cleared.LockReason != "" {
		t.Fatalf("lock fields not reset: until=%d reason=%q", cleared.LockedUntilUnixMS, cleared.LockReason)
	}
}