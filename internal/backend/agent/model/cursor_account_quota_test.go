package modeladapter

import (
	"testing"
	"time"
)

type poolLockDirectory struct {
	lockDirectory
	candidates []string
}

func (dir poolLockDirectory) CandidateAccountIDs() []string {
	return dir.candidates
}

func TestCursorAccountQuotaRefusalTextMatchesUsageLimitAndOfficialCodes(t *testing.T) {
	covered := []string{
		"cursor refused the model call: Switched to Auto after reaching Other Models usage limit.",
		"resource_exhausted: quota exceeded for this model",
		"this model is not included in your plan",
		"ERROR_PRO_USER_USAGE_LIMIT: You have reached your usage limit",
		"ERROR_API_KEY_RATE_LIMIT: API key rate limit exceeded",
		"ERROR_FREE_USER_RATE_LIMIT_EXCEEDED",
		"ERROR_USAGE_PRICING_REQUIRED",
	}
	for _, text := range covered {
		if !CursorAccountQuotaRefusalText(text) {
			t.Errorf("CursorAccountQuotaRefusalText(%q) = false, want true", text)
		}
	}
	// Transport wording and retired-endpoint / client-version codes must NOT park
	// the account in quota cooldown.
	notRefusals := []string{
		"upstream unavailable",
		"request timed out",
		"context canceled",
		"too many requests",
		"ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT: Please update to the latest version",
		"ERROR_OUTDATED_CLIENT: This client version is outdated",
	}
	for _, text := range notRefusals {
		if CursorAccountQuotaRefusalText(text) {
			t.Errorf("CursorAccountQuotaRefusalText(%q) = true, want false", text)
		}
	}
	// "does not have access" is a per-model denial that triggers failover without
	// putting the account in usage cooldown.
	if CursorAccountQuotaRefusalText("this account does not have access to gemini-4-carbon") {
		t.Error("access denial must not be treated as usage exhaustion")
	}
	if !cursorAccountRefusesModelText("this account does not have access to gemini-4-carbon") {
		t.Error("access denial must still trigger account failover")
	}
}

func TestCursorAccountModelExhaustionIsScopedAndInspectable(t *testing.T) {
	const account = "acct-quota-scope"
	const model = "gemini-4-carbon"
	t.Cleanup(func() {
		ClearAllCursorAccountExhausted("")
	})
	ClearAllCursorAccountExhausted("")

	MarkCursorAccountModelExhaustedWithReason(account, model, "reaching Other Models usage limit")
	if !CursorAccountModelExhausted(account, model) {
		t.Fatal("exhausted pair not remembered")
	}
	if CursorAccountModelExhausted(account, "other-model") {
		t.Error("exhaustion leaked to another model on the same account")
	}
	if CursorAccountModelExhausted("other-account", model) {
		t.Error("exhaustion leaked to another account on the same model")
	}

	list := ListCursorAccountExhaustedModels(account)
	if len(list) != 1 || list[0].ModelID != model || list[0].Reason != "reaching Other Models usage limit" {
		t.Fatalf("ListCursorAccountExhaustedModels = %+v", list)
	}
	if cleared := ClearAllCursorAccountExhausted(account); cleared != 1 {
		t.Fatalf("ClearAllCursorAccountExhausted = %d, want 1", cleared)
	}
	if CursorAccountModelExhausted(account, model) {
		t.Error("cleared pair still reported as exhausted")
	}
}

func TestCursorAccountModelExhaustionIgnoresEmptyIdentity(t *testing.T) {
	MarkCursorAccountModelExhausted("", "gemini-4-carbon")
	MarkCursorAccountModelExhausted("acct", "")
	if CursorAccountModelExhausted("", "gemini-4-carbon") {
		t.Error("empty account was recorded")
	}
	if CursorAccountModelExhausted("acct", "") {
		t.Error("empty model was recorded")
	}
}

func TestCandidateCursorAccountsWalksFullPoolWhenCurrentIsExhausted(t *testing.T) {
	const (
		acct1 = "acct-1-current"
		acct2 = "acct-2-secondary"
		acct3 = "acct-3-tertiary"
		model = "gemini-4-carbon"
	)
	t.Cleanup(func() {
		ClearAllCursorAccountExhausted("")
	})
	ClearAllCursorAccountExhausted("")

	auth := poolLockDirectory{
		lockDirectory: lockDirectory{
			current: acct1,
			locked:  map[string]bool{},
		},
		candidates: []string{acct1, acct2, acct3},
	}

	// Initially acct1 is chosen first, with acct2 and acct3 ready behind it.
	if got := usableCursorAccountIDForModel(auth, acct1, model); got != acct1 {
		t.Fatalf("got %q, want %q", got, acct1)
	}

	// Once acct1 (which IS CurrentAccountID!) is exhausted, failover must still
	// advance to acct2 instead of giving up because used == current.
	MarkCursorAccountModelExhausted(acct1, model)
	if got := alternateCursorAccountForModel(auth, acct1, model); got != acct2 {
		t.Fatalf("alternate after current account exhausted = %q, want %q", got, acct2)
	}
	if got := usableCursorAccountIDForModel(auth, acct1, model); got != acct2 {
		t.Fatalf("usable account after acct1 exhausted = %q, want %q", got, acct2)
	}

	// Once acct2 is also exhausted, failover advances to acct3.
	MarkCursorAccountModelExhausted(acct2, model)
	if got := usableCursorAccountIDForModel(auth, acct1, model); got != acct3 {
		t.Fatalf("usable account after acct1+acct2 exhausted = %q, want %q", got, acct3)
	}

	// When every account in the pool is exhausted, fall back to requested so
	// one live attempt surfaces the authentic upstream error.
	MarkCursorAccountModelExhausted(acct3, model)
	if got := usableCursorAccountIDForModel(auth, acct1, model); got != acct1 {
		t.Fatalf("usable account when all exhausted = %q, want %q", got, acct1)
	}
}

func TestCursorAccountQuotaRefusalGetsCooldown(t *testing.T) {
	refusal := errorString("cursor refused the model call: Switched to Auto after reaching Other Models usage limit.")
	if got := channelFailureCooldown(refusal, 1); got <= 0 {
		t.Fatalf("cooldown for a usage refusal = %v, want a positive duration", got)
	}
	if got := channelFailureCooldown(refusal, 1); got > 24*time.Hour {
		t.Fatalf("cooldown for a usage refusal = %v, want less than a day", got)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
