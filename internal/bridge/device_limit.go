package bridge

import (
	"strings"
	"time"

	"cursor/internal/cursoraccount"
	"cursor/internal/logger"
)

// accountDeviceLimitReporter puts the active Cursor account on a 24-hour
// cooldown when upstream refuses a request for device-count reasons.
//
// Cursor returns "Too many computers used within the last 24 hours" as ordinary
// error text inside a 200 response, so the proxy sniffs it out of the response
// stream and reports it here. Retrying inside the window cannot succeed.
type accountDeviceLimitReporter struct {
	accounts *cursoraccount.Manager
}

func newAccountDeviceLimitReporter(accounts *cursoraccount.Manager) *accountDeviceLimitReporter {
	if accounts == nil {
		return nil
	}
	return &accountDeviceLimitReporter{accounts: accounts}
}

// ReportDeviceLimit implements upstream.DeviceLimitReporter.
func (reporter *accountDeviceLimitReporter) ReportDeviceLimit(reason string) {
	if reporter == nil || reporter.accounts == nil {
		return
	}
	summary, ok := reporter.accounts.CurrentAccountLock()
	if !ok || summary.ID == "" {
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = cursoraccount.LockReasonDeviceLimit
	}
	until := time.Now().Add(cursoraccount.AccountDeviceCooldown)
	if _, err := reporter.accounts.MarkAccountLockedByID(summary.ID, until); err != nil {
		logger.Warnf("mark cursor account locked failed account=%s err=%v", summary.ID, err)
		return
	}
	logger.Infof("cursor account locked by device limit account=%s until=%s",
		summary.ID, until.UTC().Format(time.RFC3339))
}