package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cursor/gen/aiserverv1"
)

// This file adds read + create support for a Cursor account's personal user API
// keys (Dashboard → API Keys, crsr_ / grok_). It is the inverse of
// cursoraccount.ExchangeCursorAPIKey: given a saved account's session identity we
// call DashboardService/ListUserApiKeys and DashboardService/CreateUserApiKey,
// reusing the same authorized proto transport the spend-limit feature uses
// (resolveCursorAuthorization + postCursorDashboardDetailed).
//
// Creation is never automatic: CreateCursorAccountApiKey only runs when the UI
// explicitly asks for it. The caller either supplies a name or asks for a
// privacy-safe random one (see generateApiKeyName).

const (
	cursorListUserApiKeysPath  = "/aiserver.v1.DashboardService/ListUserApiKeys"
	cursorCreateUserApiKeyPath = "/aiserver.v1.DashboardService/CreateUserApiKey"

	apiKeyNameMaxLen = 100

	// ApiKeyNameModeRandom / ApiKeyNameModeCustom are the two ways the UI can
	// name a new key.
	ApiKeyNameModeRandom = "random"
	ApiKeyNameModeCustom = "custom"
)

// CursorAccountApiKey is the non-secret description of one user API key as the
// dashboard lists it. MaskedKey is Cursor's own "crsr_...abcd" preview; the full
// secret is only ever returned once, by CreateCursorAccountApiKey.
type CursorAccountApiKey struct {
	ID                 int32    `json:"id"`
	Name               string   `json:"name"`
	MaskedKey          string   `json:"maskedKey,omitempty"`
	Scopes             []string `json:"scopes,omitempty"`
	CreatedAtMs        int64    `json:"createdAtMs,omitempty"`
	ExpiresAtMs        int64    `json:"expiresAtMs,omitempty"`
	ServiceAccountName string   `json:"serviceAccountName,omitempty"`
}

// CursorAccountApiKeyList is the set of user API keys on one saved account.
type CursorAccountApiKeyList struct {
	AccountID string                `json:"accountId"`
	Keys      []CursorAccountApiKey `json:"keys"`
}

// CreateCursorAccountApiKeyRequest asks to mint one user API key on a saved
// account. NameMode is "random" (auto, privacy-safe) or "custom" (use Name).
type CreateCursorAccountApiKeyRequest struct {
	AccountID string   `json:"accountId,omitempty"`
	NameMode  string   `json:"nameMode,omitempty"`
	Name      string   `json:"name,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}

// CreateCursorAccountApiKeyResult carries the freshly minted secret (shown once)
// plus a best-effort list entry for it.
type CreateCursorAccountApiKeyResult struct {
	AccountID string              `json:"accountId"`
	Name      string              `json:"name"`
	ApiKey    string              `json:"apiKey"`
	Key       CursorAccountApiKey `json:"key"`
}

// ListCursorAccountApiKeys reads the user API keys on one saved Cursor account.
// An empty accountID uses the current account.
func (s *ProxyService) ListCursorAccountApiKeys(accountID string) (CursorAccountApiKeyList, error) {
	if s == nil || s.cursorAccount == nil {
		return CursorAccountApiKeyList{}, fmt.Errorf("Cursor account service is not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cursorHealthTimeout)
	defer cancel()

	resolvedID, authorization, err := s.resolveCursorAuthorization(ctx, accountID)
	if err != nil {
		return CursorAccountApiKeyList{}, err
	}

	keys, err := fetchCursorAccountApiKeys(ctx, authorization)
	if err != nil && resolvedID != "" && (errors.Is(err, errPlanAuthRejected) || s.cursorAccount.UsesAPIKey(resolvedID)) {
		if renewed, refreshErr := s.cursorAccount.ForceRefresh(ctx, resolvedID); refreshErr == nil {
			keys, err = fetchCursorAccountApiKeys(ctx, renewed)
		}
	}
	if err != nil {
		return CursorAccountApiKeyList{}, err
	}
	return CursorAccountApiKeyList{AccountID: resolvedID, Keys: keys}, nil
}

// CreateCursorAccountApiKey mints one user API key on a saved Cursor account.
// This is only ever invoked on an explicit user action; it never runs on its own.
func (s *ProxyService) CreateCursorAccountApiKey(req CreateCursorAccountApiKeyRequest) (CreateCursorAccountApiKeyResult, error) {
	if s == nil || s.cursorAccount == nil {
		return CreateCursorAccountApiKeyResult{}, fmt.Errorf("Cursor account service is not initialized")
	}

	mode := strings.ToLower(strings.TrimSpace(req.NameMode))
	if mode == "" {
		if strings.TrimSpace(req.Name) != "" {
			mode = ApiKeyNameModeCustom
		} else {
			mode = ApiKeyNameModeRandom
		}
	}
	if mode != ApiKeyNameModeRandom && mode != ApiKeyNameModeCustom {
		return CreateCursorAccountApiKeyResult{}, fmt.Errorf("nameMode must be %q or %q", ApiKeyNameModeRandom, ApiKeyNameModeCustom)
	}
	if mode == ApiKeyNameModeCustom {
		if err := validateApiKeyName(req.Name); err != nil {
			return CreateCursorAccountApiKeyResult{}, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), cursorHealthTimeout)
	defer cancel()

	resolvedID, authorization, err := s.resolveCursorAuthorization(ctx, req.AccountID)
	if err != nil {
		return CreateCursorAccountApiKeyResult{}, err
	}

	name := strings.TrimSpace(req.Name)
	if mode == ApiKeyNameModeRandom {
		// Base the random name on the account's existing keys so it blends in.
		// Best-effort: an empty list just falls back to Cursor's own convention.
		name = generateApiKeyName(existingApiKeyNames(ctx, authorization))
	}

	created, err := createCursorAccountApiKey(ctx, authorization, name, req.Scopes)
	if err != nil && resolvedID != "" && (errors.Is(err, errPlanAuthRejected) || s.cursorAccount.UsesAPIKey(resolvedID)) {
		if renewed, refreshErr := s.cursorAccount.ForceRefresh(ctx, resolvedID); refreshErr == nil {
			authorization = renewed
			created, err = createCursorAccountApiKey(ctx, renewed, name, req.Scopes)
		}
	}
	if err != nil {
		return CreateCursorAccountApiKeyResult{}, err
	}

	entry := synthesizeApiKeyEntry(name, created)
	if list, listErr := fetchCursorAccountApiKeys(ctx, authorization); listErr == nil {
		if match := findCreatedApiKey(list, created); match != nil {
			entry = *match
		}
	}

	return CreateCursorAccountApiKeyResult{
		AccountID: resolvedID,
		Name:      name,
		ApiKey:    created,
		Key:       entry,
	}, nil
}

func fetchCursorAccountApiKeys(ctx context.Context, authorization string) ([]CursorAccountApiKey, error) {
	resp := &aiserverv1.ListUserApiKeysResponse{}
	if err := postCursorDashboardDetailed(ctx, authorization, cursorListUserApiKeysPath, &aiserverv1.ListUserApiKeysRequest{}, resp); err != nil {
		return nil, err
	}
	keys := make([]CursorAccountApiKey, 0, len(resp.GetApiKeys()))
	for _, k := range resp.GetApiKeys() {
		keys = append(keys, toCursorAccountApiKey(k))
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].CreatedAtMs != keys[j].CreatedAtMs {
			return keys[i].CreatedAtMs > keys[j].CreatedAtMs
		}
		return keys[i].ID > keys[j].ID
	})
	return keys, nil
}

func existingApiKeyNames(ctx context.Context, authorization string) []string {
	resp := &aiserverv1.ListUserApiKeysResponse{}
	if err := postCursorDashboardDetailed(ctx, authorization, cursorListUserApiKeysPath, &aiserverv1.ListUserApiKeysRequest{}, resp); err != nil {
		return nil
	}
	names := make([]string, 0, len(resp.GetApiKeys()))
	for _, k := range resp.GetApiKeys() {
		if name := strings.TrimSpace(k.GetName()); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func createCursorAccountApiKey(ctx context.Context, authorization, name string, scopes []string) (string, error) {
	request := &aiserverv1.CreateUserApiKeyRequest{Name: name}
	if len(scopes) > 0 {
		request.Scopes = scopes
	}
	resp := &aiserverv1.CreateUserApiKeyResponse{}
	if err := postCursorDashboardDetailed(ctx, authorization, cursorCreateUserApiKeyPath, request, resp); err != nil {
		return "", err
	}
	key := strings.TrimSpace(resp.GetApiKey())
	if key == "" {
		return "", fmt.Errorf("Cursor did not return a new API key")
	}
	return key, nil
}

func toCursorAccountApiKey(k *aiserverv1.ApiKey) CursorAccountApiKey {
	if k == nil {
		return CursorAccountApiKey{}
	}
	return CursorAccountApiKey{
		ID:                 k.GetId(),
		Name:               strings.TrimSpace(k.GetName()),
		MaskedKey:          strings.TrimSpace(k.GetMaskedKey()),
		Scopes:             k.GetScopes(),
		CreatedAtMs:        normalizeEpochMs(k.GetCreatedAt()),
		ExpiresAtMs:        normalizeEpochMs(k.GetExpiresAt()),
		ServiceAccountName: strings.TrimSpace(k.GetServiceAccountName()),
	}
}

func synthesizeApiKeyEntry(name, rawKey string) CursorAccountApiKey {
	return CursorAccountApiKey{
		Name:        strings.TrimSpace(name),
		MaskedKey:   maskRawApiKey(rawKey),
		CreatedAtMs: time.Now().UnixMilli(),
	}
}

func findCreatedApiKey(list []CursorAccountApiKey, rawKey string) *CursorAccountApiKey {
	suffix := lastRunes(rawKey, 4)
	if suffix == "" {
		return nil
	}
	var best *CursorAccountApiKey
	for i := range list {
		if list[i].MaskedKey == "" || !strings.HasSuffix(list[i].MaskedKey, suffix) {
			continue
		}
		if best == nil || list[i].CreatedAtMs > best.CreatedAtMs {
			best = &list[i]
		}
	}
	return best
}

func maskRawApiKey(rawKey string) string {
	rawKey = strings.TrimSpace(rawKey)
	prefix := "crsr_"
	if strings.HasPrefix(rawKey, "grok_") {
		prefix = "grok_"
	}
	return prefix + "..." + lastRunes(rawKey, 4)
}

func normalizeEpochMs(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v < 1_000_000_000_000 { // seconds, not milliseconds
		return v * 1000
	}
	return v
}

func lastRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}

func validateApiKeyName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("enter a name for the API key")
	}
	if len([]rune(name)) > apiKeyNameMaxLen {
		return fmt.Errorf("API key name must be %d characters or fewer", apiKeyNameMaxLen)
	}
	for _, r := range name {
		if r == '\n' || r == '\r' || r == '\t' {
			return fmt.Errorf("API key name must not contain line breaks or tabs")
		}
	}
	return nil
}

// ---- privacy-safe random name generation -----------------------------------

const (
	agentWorkerPrefix    = "cursor-agent-worker"
	agentWorkerPrefixExt = "cursor-agent-worker-ext"

	apiKeyNameCharset = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// generateApiKeyName returns a privacy-safe automatic name for a new user API
// key. When the account already has keys it mimics the SHAPE of an existing one
// so the new key blends in; otherwise it falls back to Cursor's own
// "cursor-agent-worker-ext-..." convention.
//
// In every branch the only variable parts are freshly randomized (host/user ->
// random token, pid -> random number) or the current timestamp. Nothing stable
// to this machine/user (hostname, OS username, real pid) is ever included, so
// names minted for two different accounts share no token that could correlate
// them to the same person or device.
func generateApiKeyName(existingNames []string) string {
	if template := pickApiKeyNameTemplate(existingNames); template != "" {
		if name, ok := mimicAgentWorkerName(template); ok {
			return clampApiKeyName(name)
		}
		if name, ok := mimicStructuredName(template); ok {
			return clampApiKeyName(name)
		}
	}
	return clampApiKeyName(defaultAgentWorkerName())
}

// pickApiKeyNameTemplate prefers a Cursor-generated "cursor-agent-worker" name
// (the strongest signal of the convention to imitate); otherwise it returns the
// longest/most-structured existing name as a shape to mimic.
func pickApiKeyNameTemplate(names []string) string {
	best := ""
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, agentWorkerPrefix) {
			return name
		}
		if len(name) > len(best) {
			best = name
		}
	}
	return best
}

func defaultAgentWorkerName() string {
	return fmt.Sprintf("%s-%s-%d-%d", agentWorkerPrefixExt, randomLowerToken(6), randomPID(), time.Now().UnixMilli())
}

// mimicAgentWorkerName reproduces the recognizable cursor-agent-worker[-ext]
// prefix and regenerates everything after it (host/user, pid, timestamp) so no
// real identifier leaks.
func mimicAgentWorkerName(template string) (string, bool) {
	for _, prefix := range []string{agentWorkerPrefixExt, agentWorkerPrefix} {
		if template == prefix || strings.HasPrefix(template, prefix+"-") {
			return fmt.Sprintf("%s-%s-%d-%d", prefix, randomLowerToken(6), randomPID(), time.Now().UnixMilli()), true
		}
	}
	return "", false
}

// mimicStructuredName reproduces the segment shape of template (count of
// dash-separated groups and whether each was numeric or alphabetic) while
// replacing every segment's content with fresh random data. A long digit run
// (>=12) becomes the current epoch-ms so the name still looks timestamped.
func mimicStructuredName(template string) (string, bool) {
	segments := strings.Split(template, "-")
	out := make([]string, 0, len(segments))
	hasContent := false
	for _, seg := range segments {
		switch {
		case seg == "":
			out = append(out, "")
		case isAllDigits(seg):
			hasContent = true
			if len(seg) >= 12 {
				out = append(out, strconv.FormatInt(time.Now().UnixMilli(), 10))
			} else {
				out = append(out, randomDigits(len(seg)))
			}
		default:
			hasContent = true
			out = append(out, randomLowerToken(tokenLenFor(seg)))
		}
	}
	if !hasContent {
		return "", false
	}
	return strings.Join(out, "-"), true
}

func tokenLenFor(seg string) int {
	n := 0
	for _, r := range seg {
		if r != '.' {
			n++
		}
	}
	if n < 3 {
		n = 3
	}
	if n > 12 {
		n = 12
	}
	return n
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func clampApiKeyName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > apiKeyNameMaxLen {
		name = strings.TrimSpace(name[:apiKeyNameMaxLen])
	}
	return name
}

func randomBytes(n int) []byte {
	if n <= 0 {
		n = 1
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		seed := time.Now().UnixNano()
		for i := range buf {
			seed = seed*1103515245 + 12345
			buf[i] = byte(seed >> 33)
		}
	}
	return buf
}

func randomLowerToken(n int) string {
	if n <= 0 {
		n = 6
	}
	buf := randomBytes(n)
	out := make([]byte, n)
	for i := range out {
		out[i] = apiKeyNameCharset[int(buf[i])%len(apiKeyNameCharset)]
	}
	return string(out)
}

func randomDigits(n int) string {
	if n <= 0 {
		n = 1
	}
	buf := randomBytes(n)
	out := make([]byte, n)
	for i := range out {
		d := int(buf[i]) % 10
		if i == 0 && d == 0 {
			d = 1
		}
		out[i] = byte('0' + d)
	}
	return string(out)
}

// randomPID returns a plausible but random process-id-like number. It is NOT the
// real os.Getpid(), which would be identical across accounts in one run and thus
// correlatable.
func randomPID() int {
	buf := randomBytes(2)
	v := int(buf[0])<<8 | int(buf[1])
	return 1000 + (v % 60000)
}
