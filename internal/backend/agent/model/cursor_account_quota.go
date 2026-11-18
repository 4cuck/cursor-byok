package modeladapter

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Cursor rejects a model call for usage/plan reasons either via ErrorDetails.Error
// codes (such as ERROR_PRO_USER_USAGE_LIMIT or ERROR_API_KEY_RATE_LIMIT) or via
// CustomErrorDetails prose ("Switched to Auto after reaching Other Models usage
// limit") over a 200 Connect stream with no HTTP error status.
//
// Remembering exhausted (account, model) pairs lets both the Cursor account
// gateway and the channel router fail over immediately to any other saved
// Cursor account that still has quota for that model, while allowing the spent
// account to continue serving other models (such as Auto or Composer).
const (
	// cursorAccountModelQuotaCooldown is how long a refused (account, model) pair
	// is skipped before being probed again so recovery happens automatically when
	// Cursor's usage window resets.
	cursorAccountModelQuotaCooldown = 20 * time.Minute
)

// CursorAccountExhaustedModel is a non-secret diagnostic view of one active
// per-(account, model) quota cooldown.
type CursorAccountExhaustedModel struct {
	AccountID   string `json:"accountId"`
	ModelID     string `json:"modelId"`
	Reason      string `json:"reason,omitempty"`
	UntilUnixMS int64  `json:"untilUnixMs"`
}

type cursorAccountQuotaRecord struct {
	accountID string
	modelID   string
	reason    string
	until     time.Time
}

type cursorAccountQuotaMemory struct {
	mu      sync.Mutex
	records map[string]cursorAccountQuotaRecord
}

var cursorAccountQuotaStore = &cursorAccountQuotaMemory{records: map[string]cursorAccountQuotaRecord{}}

func cursorAccountQuotaKey(accountID, modelID string) string {
	return strings.TrimSpace(accountID) + "\n" + strings.ToLower(strings.TrimSpace(modelID))
}

// MarkCursorAccountModelExhausted records that Cursor refused this account for
// this model. An empty account or model is ignored.
func MarkCursorAccountModelExhausted(accountID, modelID string) {
	MarkCursorAccountModelExhaustedWithReason(accountID, modelID, "")
}

// MarkCursorAccountModelExhaustedWithReason records an exhausted (account, model)
// pair along with a sanitized human-readable reason for diagnostics.
func MarkCursorAccountModelExhaustedWithReason(accountID, modelID, reason string) {
	accountID = strings.TrimSpace(accountID)
	modelID = strings.TrimSpace(modelID)
	if accountID == "" || modelID == "" {
		return
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 200 {
		reason = reason[:200]
	}
	cursorAccountQuotaStore.mu.Lock()
	cursorAccountQuotaStore.records[cursorAccountQuotaKey(accountID, modelID)] = cursorAccountQuotaRecord{
		accountID: accountID,
		modelID:   strings.ToLower(modelID),
		reason:    reason,
		until:     time.Now().Add(cursorAccountModelQuotaCooldown),
	}
	cursorAccountQuotaStore.mu.Unlock()
}

// ClearCursorAccountModelExhausted forgets the record after a call succeeds, so a
// recovered account returns to rotation without waiting out the cooldown.
func ClearCursorAccountModelExhausted(accountID, modelID string) {
	cursorAccountQuotaStore.mu.Lock()
	delete(cursorAccountQuotaStore.records, cursorAccountQuotaKey(accountID, modelID))
	cursorAccountQuotaStore.mu.Unlock()
}

// ClearAllCursorAccountExhausted clears all per-model cooldowns for one account
// (or all accounts when accountID is empty) and returns how many were cleared.
func ClearAllCursorAccountExhausted(accountID string) int {
	accountID = strings.TrimSpace(accountID)
	cursorAccountQuotaStore.mu.Lock()
	defer cursorAccountQuotaStore.mu.Unlock()
	cleared := 0
	for key, rec := range cursorAccountQuotaStore.records {
		if accountID == "" || rec.accountID == accountID {
			delete(cursorAccountQuotaStore.records, key)
			cleared++
		}
	}
	return cleared
}

// CursorAccountModelCooldownUntil reports the active quota cooldown deadline for
// an (account, model) pair, dropping expired entries on read.
func CursorAccountModelCooldownUntil(accountID, modelID string) (time.Time, bool) {
	accountID = strings.TrimSpace(accountID)
	modelID = strings.TrimSpace(modelID)
	if accountID == "" || modelID == "" {
		return time.Time{}, false
	}
	key := cursorAccountQuotaKey(accountID, modelID)
	now := time.Now()
	cursorAccountQuotaStore.mu.Lock()
	defer cursorAccountQuotaStore.mu.Unlock()
	rec, ok := cursorAccountQuotaStore.records[key]
	if !ok {
		return time.Time{}, false
	}
	if !rec.until.After(now) {
		delete(cursorAccountQuotaStore.records, key)
		return time.Time{}, false
	}
	return rec.until, true
}

// CursorAccountModelExhausted reports whether this account is inside the cooldown
// for this model. Expired entries are dropped on read.
func CursorAccountModelExhausted(accountID, modelID string) bool {
	_, active := CursorAccountModelCooldownUntil(accountID, modelID)
	return active
}

// ListCursorAccountExhaustedModels returns active per-model quota cooldowns for
// one account (or across all accounts when accountID is empty).
func ListCursorAccountExhaustedModels(accountID string) []CursorAccountExhaustedModel {
	accountID = strings.TrimSpace(accountID)
	now := time.Now()
	cursorAccountQuotaStore.mu.Lock()
	out := make([]CursorAccountExhaustedModel, 0, len(cursorAccountQuotaStore.records))
	for key, rec := range cursorAccountQuotaStore.records {
		if !rec.until.After(now) {
			delete(cursorAccountQuotaStore.records, key)
			continue
		}
		if accountID != "" && rec.accountID != accountID {
			continue
		}
		out = append(out, CursorAccountExhaustedModel{
			AccountID:   rec.accountID,
			ModelID:     rec.modelID,
			Reason:      rec.reason,
			UntilUnixMS: rec.until.UnixMilli(),
		})
	}
	cursorAccountQuotaStore.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].AccountID != out[j].AccountID {
			return out[i].AccountID < out[j].AccountID
		}
		return out[i].ModelID < out[j].ModelID
	})
	return out
}

// CursorAccountQuotaRefusalText reports whether upstream text is a usage/plan/quota
// refusal from Cursor's ErrorDetails.Error enum or CustomErrorDetails prose rather
// than a transient transport or client-version failure.
func CursorAccountQuotaRefusalText(text string) bool {
	lower := strings.ToLower(text)
	// Retired endpoint / client-version errors use ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT
	// or ERROR_OUTDATED_CLIENT; never misclassify those as account quota exhaustion.
	if strings.Contains(lower, "error_gpt_4_vision_preview_rate_limit") ||
		strings.Contains(lower, "error_outdated_client") ||
		strings.Contains(lower, "outdated version") ||
		strings.Contains(lower, "update to the latest version") {
		return false
	}
	for _, marker := range []string{
		// Exact Cursor IDE 3.30.14 aiserver.v1.ErrorDetails.Error codes (lowercased):
		"error_free_user_usage_limit",
		"error_pro_user_usage_limit",
		"error_free_user_rate_limit_exceeded",
		"error_pro_user_rate_limit_exceeded",
		"error_resource_exhausted",
		"error_api_key_rate_limit",
		"error_openai_rate_limit_exceeded",
		"error_generic_rate_limit_exceeded",
		"error_rate_limited",
		"error_pro_user_only",
		"error_usage_pricing_required",
		// CustomErrorDetails prose markers:
		"usage limit",
		"usage_limit",
		"quota exceeded",
		"quota_exceeded",
		"resource_exhausted",
		"out of usage",
		"spend limit",
		"not included in your plan",
		"not available on your plan",
		"requires a paid",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// cursorAccountRefusesModelText reports whether upstream text means "this account
// cannot serve this model right now", covering both usage/plan refusals and
// per-model access denials so the gateway can fail over to another account.
func cursorAccountRefusesModelText(text string) bool {
	if CursorAccountQuotaRefusalText(text) {
		return true
	}
	lower := strings.ToLower(text)
	for _, marker := range []string{
		"does not have access",
		"do not have access",
		"don't have access",
		"error_model_blocked",
		"error_max_mode_required",
		"model is not available",
		"model not available",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// cursorAccountPoolDirectory is implemented by *cursoraccount.Manager so failover
// can walk every unlocked saved Cursor account, not only CurrentAccountID().
type cursorAccountPoolDirectory interface {
	CandidateAccountIDs() []string
}

// candidateCursorAccountsForModel returns the ordered list of saved Cursor account
// IDs to attempt for a turn:
//  1. requested (when non-empty, unlocked, and not exhausted for modelID)
//  2. current account (when unlocked and not exhausted for modelID)
//  3. any other unlocked, non-exhausted accounts in the saved account pool
//
// If every known account is locked or exhausted, it falls back to requested (or
// current) so one live attempt still produces an up-to-date upstream error.
func candidateCursorAccountsForModel(auth CursorAccountAuthorizer, requested, modelID string) []string {
	requested = strings.TrimSpace(requested)
	directory, ok := auth.(cursorAccountDirectory)
	if !ok {
		return []string{requested}
	}
	usable := func(accountID string) bool {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			return false
		}
		return !directory.AccountIsLocked(accountID) && !CursorAccountModelExhausted(accountID, modelID)
	}

	seen := make(map[string]struct{}, 4)
	out := make([]string, 0, 4)
	push := func(accountID string) {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			return
		}
		if _, exists := seen[accountID]; exists {
			return
		}
		if !usable(accountID) {
			return
		}
		seen[accountID] = struct{}{}
		out = append(out, accountID)
	}

	push(requested)
	current := strings.TrimSpace(directory.CurrentAccountID())
	push(current)
	if pool, hasPool := auth.(cursorAccountPoolDirectory); hasPool {
		for _, id := range pool.CandidateAccountIDs() {
			push(id)
		}
	}

	if len(out) > 0 {
		return out
	}
	if requested != "" {
		return []string{requested}
	}
	if current != "" {
		return []string{current}
	}
	return []string{""}
}

// alternateCursorAccountForModel returns the next eligible account after `used`
// that can serve `modelID`, consulting both CurrentAccountID() and the full saved
// account pool.
func alternateCursorAccountForModel(auth CursorAccountAuthorizer, used, modelID string) string {
	used = strings.TrimSpace(used)
	for _, candidate := range candidateCursorAccountsForModel(auth, "", modelID) {
		if candidate != "" && candidate != used {
			if dir, ok := auth.(cursorAccountDirectory); ok {
				if dir.AccountIsLocked(candidate) || CursorAccountModelExhausted(candidate, modelID) {
					continue
				}
			}
			return candidate
		}
	}
	return ""
}
