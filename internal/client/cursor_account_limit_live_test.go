//go:build liveprobe

package client

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"cursor/internal/cursoraccount"
)

// Live probe verifying DashboardService/GetHardLimit (and optionally
// SetHardLimit when CURSOR_LIVE_SET_LIMIT is explicitly provided) against
// api2.cursor.sh using either an OAuth JWT or a crsr_/grok_ user API key.
//
// Read-only check:
//   $env:CURSOR_LIVE_CREDENTIAL='grok_... or eyJ...'; go test -tags liveprobe -run TestLiveCursorSpendLimit -v ./internal/client/
//
// Opt-in write test (restores the original limit afterwards unless CURSOR_LIVE_KEEP_LIMIT=1):
//   $env:CURSOR_LIVE_CREDENTIAL='...'; $env:CURSOR_LIVE_SET_LIMIT='disabled|50|unlimited'; go test -tags liveprobe -run TestLiveCursorSpendLimit -v ./internal/client/
func TestLiveCursorSpendLimit(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("CURSOR_LIVE_CREDENTIAL"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("CURSOR_LIVE_API_KEY"))
	}
	if raw == "" {
		t.Skip("CURSOR_LIVE_CREDENTIAL (or CURSOR_LIVE_API_KEY) not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	manager := cursoraccount.NewManager(t.TempDir(), "", &http.Client{Timeout: 20 * time.Second})
	summary, err := manager.ImportToken(ctx, raw)
	if err != nil {
		t.Fatalf("ImportToken: %v", err)
	}
	svc := &ProxyService{cursorAccount: manager}

	before, err := svc.getCursorAccountSpendLimit(ctx, summary.ID)
	if err != nil {
		t.Fatalf("GetCursorAccountSpendLimit: %v", err)
	}
	fmt.Printf("account=%s credType=%s membership=%s status=%s mode=%s limit=$%d editable=%v teamId=%d personalScope=%v\n",
		summary.Email, summary.CredentialType, before.MembershipType, before.SubscriptionStatus,
		before.Mode, before.HardLimitDollars, before.Editable, before.TeamID, before.PersonalScope)

	target := strings.TrimSpace(os.Getenv("CURSOR_LIVE_SET_LIMIT"))
	if target == "" {
		return
	}

	req := CursorAccountSpendLimitRequest{AccountID: summary.ID}
	switch strings.ToLower(target) {
	case "disabled", "off", "0":
		req.Mode = CursorSpendLimitModeDisabled
	case "unlimited", "none":
		req.Mode = CursorSpendLimitModeUnlimited
	default:
		dollars, parseErr := strconv.Atoi(strings.TrimPrefix(target, "$"))
		if parseErr != nil || dollars <= 0 {
			t.Fatalf("invalid CURSOR_LIVE_SET_LIMIT=%q (use disabled, unlimited, or positive integer dollars)", target)
		}
		req.Mode = CursorSpendLimitModeFixed
		req.HardLimitDollars = int32(dollars)
	}

	after, err := svc.setCursorAccountSpendLimit(ctx, req)
	if err != nil {
		t.Fatalf("SetCursorAccountSpendLimit(%+v): %v", req, err)
	}
	fmt.Printf("updated: mode=%s limit=$%d noUsageBasedAllowed=%v\n", after.Mode, after.HardLimitDollars, after.NoUsageBasedAllowed)

	if os.Getenv("CURSOR_LIVE_KEEP_LIMIT") == "1" {
		return
	}
	restoreReq := CursorAccountSpendLimitRequest{
		AccountID:        summary.ID,
		Mode:             before.Mode,
		HardLimitDollars: before.HardLimitDollars,
		TeamID:           before.TeamID,
		PersonalScope:    before.PersonalScope,
	}
	restored, err := svc.setCursorAccountSpendLimit(ctx, restoreReq)
	if err != nil {
		t.Fatalf("restore original limit (%+v) failed: %v", restoreReq, err)
	}
	fmt.Printf("restored: mode=%s limit=$%d\n", restored.Mode, restored.HardLimitDollars)
}
