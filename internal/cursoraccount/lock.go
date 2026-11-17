package cursoraccount

import (
	"errors"
	"strings"
	"time"

	"cursor/internal/logger"
)

// Cursor caps how many distinct devices may use one account within a rolling
// 24-hour window. When tripped, model calls fail with a device-limit error and
// keep failing for the remainder of the window. Retrying immediately cannot
// succeed and only wastes quota, so the account is put on a cooldown locally.
const (
	// AccountDeviceCooldown is Cursor's stated window for the device cap.
	AccountDeviceCooldown = 24 * time.Hour

	LockReasonDeviceLimit = "device_limit"
)

// IsLocked reports whether the account is currently inside a cooldown.
func (summary CursorAccountSummary) IsLocked(now time.Time) bool {
	return summary.LockedUntilUnixMS > 0 && now.UnixMilli() < summary.LockedUntilUnixMS
}

// LockedRemaining returns how long is left on the cooldown (zero when unlocked).
func (summary CursorAccountSummary) LockedRemaining(now time.Time) time.Duration {
	if !summary.IsLocked(now) {
		return 0
	}
	return time.Until(time.UnixMilli(summary.LockedUntilUnixMS))
}

// deviceLimitMarkers are the phrases Cursor uses when the per-account device
// cap is hit. Matched case-insensitively against upstream error payloads.
var deviceLimitMarkers = []string{
	"too many computers",
	"too many devices",
	"device_limit",
	"device limit",
	"used within the last 24 hours",
}

// IsDeviceLimitError reports whether an upstream error payload means "this
// account is temporarily locked because of too many devices".
func IsDeviceLimitError(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range deviceLimitMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// MarkAccountLockedByID starts a cooldown for one account.
func (manager *Manager) MarkAccountLockedByID(id string, until time.Time) (CursorAccountSummary, error) {
	if manager == nil || manager.store == nil {
		return CursorAccountSummary{}, errors.New("cursor account store is not initialized")
	}
	return manager.store.MarkAccountLocked(id, LockReasonDeviceLimit, until)
}

// ClearAccountLockByID ends a cooldown early. This backs the explicit "try
// anyway" action: the user accepts the risk of another failed attempt.
func (manager *Manager) ClearAccountLockByID(id string) (CursorAccountSummary, error) {
	if manager == nil || manager.store == nil {
		return CursorAccountSummary{}, errors.New("cursor account store is not initialized")
	}
	return manager.store.ClearAccountLock(id)
}

// ReportDeviceLimit implements upstream.DeviceLimitReporter. It puts the active
// account on a 24-hour cooldown, because Cursor refuses every request from a
// locked account until the window rolls over.
func (manager *Manager) ReportDeviceLimit(reason string) {
	if manager == nil || manager.store == nil {
		return
	}
	summary, locked := manager.CurrentAccountLock()
	if summary.ID == "" {
		return
	}
	if locked {
		// Already on cooldown; do not extend it on every failed request.
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = LockReasonDeviceLimit
	}
	until := time.Now().Add(AccountDeviceCooldown)
	if _, err := manager.MarkAccountLockedByID(summary.ID, until); err != nil {
		logger.Warnf("mark cursor account locked failed account=%s err=%v", summary.ID, err)
		return
	}
	logger.Infof("cursor account locked by device limit account=%s until=%s",
		summary.ID, until.UTC().Format(time.RFC3339))
}

// CurrentAccountLock reports the lock state of the active account.
func (manager *Manager) CurrentAccountLock() (CursorAccountSummary, bool) {
	if manager == nil || manager.store == nil {
		return CursorAccountSummary{}, false
	}
	id := manager.store.CurrentAccountID()
	if strings.TrimSpace(id) == "" {
		return CursorAccountSummary{}, false
	}
	record, err := manager.store.loadRecord(id)
	if err != nil {
		return CursorAccountSummary{}, false
	}
	summary := recordToSummary(record, true)
	return summary, summary.IsLocked(time.Now())
}