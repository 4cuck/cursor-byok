package claudeaccount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	tokenAlice    = "sk-ant-oat01-alice-token-AAAAAAAAAAAAAAAAAAAA1111"
	tokenAliceNew = "sk-ant-oat01-alice-token-BBBBBBBBBBBBBBBBBBBB2222"
	tokenBob      = "sk-ant-oat01-bob-token-CCCCCCCCCCCCCCCCCCCCCC3333"
)

var testNow = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func TestParseCredentials(t *testing.T) {
	later := testNow.Add(24 * time.Hour).UnixMilli()
	file := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":null,"expiresAt":%d,"scopes":["user:profile","user:inference"],"subscriptionType":"pro","rateLimitTier":"default_claude_pro"}}`, tokenAlice, later)

	got, err := ParseCredentials(file)
	if err != nil {
		t.Fatalf("ParseCredentials(file) error = %v", err)
	}
	if got.AccessToken != tokenAlice || got.ExpiresAtUnixMS != later || got.SubscriptionType != "pro" || got.RateLimitTier != "default_claude_pro" {
		t.Fatalf("ParseCredentials(file) = %+v", got)
	}
	if strings.Join(got.Scopes, ",") != "user:profile,user:inference" {
		t.Fatalf("scopes = %v", got.Scopes)
	}

	inner, err := ParseCredentials(fmt.Sprintf(`{"accessToken":" %s ","expiresAt":%d}`, tokenAlice, later/1000))
	if err != nil || inner.AccessToken != tokenAlice {
		t.Fatalf("ParseCredentials(inner object) = %+v, error = %v", inner, err)
	}
	if inner.ExpiresAtUnixMS != later/1000*1000 {
		t.Fatalf("an expiry in seconds must be read as seconds: got %d", inner.ExpiresAtUnixMS)
	}

	bare, err := ParseCredentials("  " + tokenAlice + "\n")
	if err != nil || bare.AccessToken != tokenAlice || bare.ExpiresAtUnixMS != 0 {
		t.Fatalf("ParseCredentials(bare token) = %+v, error = %v", bare, err)
	}

	failures := []struct {
		name  string
		input string
		code  string
	}{
		{"empty", "   ", CodeImportEmpty},
		{"too large", `{"claudeAiOauth":{"accessToken":"` + strings.Repeat("a", maxCredentialsBytes) + `"}}`, CodeImportTooLarge},
		{"not JSON", "{not json", CodeImportInvalid},
		{"JSON that is not an object", `"text"`, CodeImportInvalid},
		{"no token", `{"claudeAiOauth":{}}`, CodeImportNoToken},
		{"token of the wrong type", `{"claudeAiOauth":{"accessToken":42}}`, CodeImportNoToken},
		{"claudeAiOauth of the wrong type", `{"claudeAiOauth":"text"}`, CodeImportNoToken},
		{"an API key pasted alone", "sk-ant-api03-abcdef", CodeNotOAuthToken},
		{"an API key inside a file", `{"claudeAiOauth":{"accessToken":"sk-ant-api03-abcdef"}}`, CodeNotOAuthToken},
	}
	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCredentials(tt.input)
			if err == nil || !strings.HasPrefix(err.Error(), tt.code+":") {
				t.Fatalf("ParseCredentials() error = %v, want code %s", err, tt.code)
			}
		})
	}
}

// fakeAnthropic stands in for the Anthropic endpoints the package reads. What
// it answers is keyed by bearer token.
type fakeAnthropic struct {
	mu       sync.Mutex
	profiles map[string]string // token -> profile JSON
	usages   map[string]string // token -> usage JSON
	models   map[string]string // token -> models JSON
	status   map[string]int    // "token path" -> status to answer instead
	down     bool              // answer nothing: close the connection
	calls    []string          // "path token-suffix"
}

func (f *fakeAnthropic) handler(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	f.calls = append(f.calls, r.URL.Path+" "+token[max(0, len(token)-4):])
	down := f.down
	status := f.status[token+" "+r.URL.Path]
	var body string
	switch r.URL.Path {
	case "/api/oauth/profile":
		body = f.profiles[token]
	case "/api/oauth/usage":
		body = f.usages[token]
	case "/v1/models":
		body = f.models[token]
	}
	f.mu.Unlock()

	if down {
		hijacker, _ := w.(http.Hijacker)
		connection, _, _ := hijacker.Hijack()
		_ = connection.Close()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"some_error","message":"refused by the test"}}`))
		return
	}
	if body == "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"}}`))
		return
	}
	_, _ = w.Write([]byte(body))
}

func profileJSON(accountUUID, email, name, organizationType string, credits bool) string {
	return fmt.Sprintf(`{"account":{"uuid":%q,"full_name":%q,"display_name":%q,"email":%q},"organization":{"uuid":%q,"name":%q,"organization_type":%q,"billing_type":"stripe_subscription","rate_limit_tier":"default_claude_ai","has_extra_usage_enabled":%t}}`,
		accountUUID, name, name, email, "org-"+accountUUID, email+"'s Organization", organizationType, credits)
}

const usageJSON = `{"five_hour":{"utilization":12.5,"resets_at":"2026-11-27T17:50:00+00:00"},"seven_day":{"utilization":69,"resets_at":"2026-11-28T20:00:00+00:00"},"seven_day_opus":null,"seven_day_sonnet":{"utilization":140,"resets_at":null},"extra_usage":{"is_enabled":false,"utilization":null,"disabled_reason":null}}`

// usageWithCreditsJSON is the usage of an account that pays for use beyond its
// plan and has spent 40% of its monthly credit limit.
const usageWithCreditsJSON = `{"five_hour":{"utilization":3,"resets_at":"2026-11-27T17:50:00+00:00"},"seven_day":{"utilization":8,"resets_at":"2026-11-28T20:00:00+00:00"},"extra_usage":{"is_enabled":true,"utilization":40,"disabled_reason":null}}`

// usageWithModelLimitsJSON has the form of an answer of the usage endpoint as
// it was on 2026-11-27, where "limits" repeats the plan's windows. Added to it
// are weekly limits for single model families, in the form the Claude CLI
// reads them: one that is new, one that repeats a window the answer has under
// its own key, one without a percentage, and one that names no model.
const usageWithModelLimitsJSON = `{
	"five_hour": {"utilization": 100, "resets_at": "2026-11-27T17:50:00.306593+00:00", "limit_dollars": null, "locked_reason": null},
	"seven_day": {"utilization": 74, "resets_at": "2026-11-28T20:00:00.306615+00:00", "limit_dollars": null, "locked_reason": null},
	"seven_day_opus": null,
	"seven_day_sonnet": {"utilization": 20, "resets_at": "2026-11-28T20:00:00+00:00"},
	"extra_usage": {"is_enabled": false, "utilization": null, "disabled_reason": null, "credits_can_pay": "no"},
	"limits": [
		{"kind": "session", "group": "session", "percent": 100, "severity": "critical", "resets_at": "2026-11-27T17:50:00.306593+00:00", "scope": null, "is_active": true},
		{"kind": "weekly_all", "group": "weekly", "percent": 74, "severity": "normal", "resets_at": "2026-11-28T20:00:00.306615+00:00", "scope": null, "is_active": false},
		{"kind": "weekly_scoped", "group": "weekly", "percent": 55.5, "resets_at": "2026-11-29T08:00:00+00:00", "scope": {"model": {"display_name": "Fable"}}},
		{"kind": "weekly_scoped", "group": "weekly", "percent": 20, "resets_at": "2026-11-28T20:00:00+00:00", "scope": {"model": {"display_name": "Sonnet"}}},
		{"kind": "weekly_scoped", "group": "weekly", "percent": null, "resets_at": null, "scope": {"model": {"display_name": "Haiku"}}},
		{"kind": "weekly_scoped", "group": "weekly", "percent": 10, "resets_at": null, "scope": null}
	]
}`

func TestUsageReadsWeeklyLimitsOfSingleModelFamilies(t *testing.T) {
	manager, fake, _ := newTestManager(t)
	fake.usages[tokenAlice] = usageWithModelLimitsJSON
	account := mustImport(t, manager, tokenAlice).Account
	want := []UsageWindow{
		{ID: WindowSession, UsedPercent: 100, ResetsAt: "2026-11-27T17:50:00.306593+00:00"},
		{ID: WindowWeekly, UsedPercent: 74, ResetsAt: "2026-11-28T20:00:00.306615+00:00"},
		{ID: WindowWeeklySonnet, UsedPercent: 20, ResetsAt: "2026-11-28T20:00:00+00:00"},
		{ID: WindowWeeklyModelPrefix + "fable", Label: "Fable", UsedPercent: 55.5, ResetsAt: "2026-11-29T08:00:00+00:00"},
	}
	if !reflect.DeepEqual(account.UsageWindows, want) {
		t.Fatalf("usage windows = %+v, want %+v", account.UsageWindows, want)
	}

	// A "limits" list in a form this package does not know costs the windows
	// read from it and nothing else.
	fake.usages[tokenAlice] = `{"five_hour": {"utilization": 5, "resets_at": null}, "limits": {"unexpected": "object"}}`
	refreshed, err := manager.Refresh(context.Background(), account.ID)
	if err != nil || refreshed.DetailsError != "" {
		t.Fatalf("Refresh() error = %v, details error = %q", err, refreshed.DetailsError)
	}
	if want := []UsageWindow{{ID: WindowSession, UsedPercent: 5}}; !reflect.DeepEqual(refreshed.UsageWindows, want) {
		t.Fatalf("usage windows = %+v, want %+v", refreshed.UsageWindows, want)
	}
}

func newTestManager(t *testing.T) (*Manager, *fakeAnthropic, string) {
	t.Helper()
	fake := &fakeAnthropic{
		profiles: map[string]string{
			tokenAlice:    profileJSON("acct-alice", "alice@example.test", "Alice", "claude_pro", false),
			tokenAliceNew: profileJSON("acct-alice", "alice@example.test", "Alice", "claude_pro", false),
			tokenBob:      profileJSON("acct-bob", "bob@example.test", "Bob", "claude_max", true),
		},
		usages: map[string]string{tokenAlice: usageJSON, tokenAliceNew: usageJSON, tokenBob: usageWithCreditsJSON},
		models: map[string]string{
			tokenAlice: `{"data":[{"id":"claude-sonnet-4-6","display_name":"Claude Sonnet 4.6"},{"id":"claude-opus-4-8","display_name":"Claude Opus 4.8"}],"has_more":false,"last_id":"claude-opus-4-8"}`,
			tokenBob:   `{"data":[{"id":"claude-haiku-5-5","display_name":"Claude Haiku 5.5"}],"has_more":false,"last_id":"claude-haiku-5-5"}`,
		},
		status: map[string]int{},
	}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	api := NewAPI(server.Client(), "claude-cli/test")
	api.baseURL = server.URL
	dataRoot := t.TempDir()
	manager := NewManager(dataRoot, api)
	manager.now = func() time.Time { return testNow }
	return manager, fake, dataRoot
}

func mustImport(t *testing.T, manager *Manager, text string) ImportResult {
	t.Helper()
	result, err := manager.Import(context.Background(), text, nil)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	return result
}

func credentialsFile(token string, expiresAt time.Time) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":null,"expiresAt":%d,"scopes":["user:profile","user:inference"],"subscriptionType":"pro"}}`, token, expiresAt.UnixMilli())
}

func TestImportSavesAccountWithDetailsAndNoTokenInView(t *testing.T) {
	manager, _, dataRoot := newTestManager(t)
	result := mustImport(t, manager, credentialsFile(tokenAlice, testNow.Add(48*time.Hour)))
	if !result.Created {
		t.Fatal("the first import of an account must create it")
	}
	account := result.Account
	if account.Email != "alice@example.test" || account.DisplayName != "Alice" || account.Plan != "pro" || account.Status != StatusOK {
		t.Fatalf("account = %+v", account)
	}
	if account.UsageCredits == nil || account.UsageCredits.Enabled {
		t.Fatalf("usage credits = %+v, want known and off", account.UsageCredits)
	}
	if account.UsageAtUnixMS != testNow.UnixMilli() || account.DetailsError != "" {
		t.Fatalf("usage read time = %d, details error = %q", account.UsageAtUnixMS, account.DetailsError)
	}
	wantWindows := []UsageWindow{
		{ID: WindowSession, UsedPercent: 12.5, ResetsAt: "2026-11-27T17:50:00+00:00"},
		{ID: WindowWeekly, UsedPercent: 69, ResetsAt: "2026-11-28T20:00:00+00:00"},
		{ID: WindowWeeklySonnet, UsedPercent: 100},
	}
	if fmt.Sprint(account.UsageWindows) != fmt.Sprint(wantWindows) {
		t.Fatalf("usage windows = %+v, want %+v (absent windows dropped, percent clamped)", account.UsageWindows, wantWindows)
	}

	// What goes to the UI holds no token, only a hint of it.
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), tokenAlice) || strings.Contains(string(encoded), "alice-token") {
		t.Fatalf("the import result leaks the token: %s", encoded)
	}
	if account.TokenHint != "sk-ant-oat01-…1111" {
		t.Fatalf("token hint = %q", account.TokenHint)
	}

	// The token is in the accounts file, which only the current user may read.
	path := filepath.Join(dataRoot, storeFileName)
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), tokenAlice) {
		t.Fatalf("accounts file: error = %v, holds the token = %v", err, strings.Contains(string(data), tokenAlice))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != storeFilePerm {
			t.Fatalf("accounts file mode = %v, error = %v, want %v", info.Mode().Perm(), err, storeFilePerm)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(dataRoot, storeFileName+".tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

// TestImportKeepsSeveralAccounts covers more than one account: a credential of
// another account is saved next to the first, and a new credential of a saved
// account replaces that account's token.
func TestImportKeepsSeveralAccounts(t *testing.T) {
	manager, _, _ := newTestManager(t)
	alice := mustImport(t, manager, credentialsFile(tokenAlice, testNow.Add(48*time.Hour)))
	bob := mustImport(t, manager, tokenBob)
	if !bob.Created || bob.Account.ID == alice.Account.ID {
		t.Fatalf("a credential of another account must be saved as a second account: %+v", bob)
	}
	credits := bob.Account.UsageCredits
	if bob.Account.Plan != "max" || credits == nil || !credits.Enabled || credits.UsedPercent == nil || *credits.UsedPercent != 40 {
		t.Fatalf("second account = %+v, usage credits = %+v", bob.Account, credits)
	}

	var seen []string
	replaced, err := manager.Import(context.Background(), tokenAliceNew, func(accountID, previousToken, token string) error {
		seen = append(seen, accountID, previousToken, token)
		return nil
	})
	if err != nil {
		t.Fatalf("Import(new credential of a saved account) error = %v", err)
	}
	if replaced.Created || replaced.Account.ID != alice.Account.ID {
		t.Fatalf("a new credential of a saved account must replace it, not add an account: %+v", replaced)
	}
	if strings.Join(seen, "|") != strings.Join([]string{alice.Account.ID, tokenAlice, tokenAliceNew}, "|") {
		t.Fatalf("beforeSave got %v", seen)
	}

	accounts, err := manager.List()
	if err != nil || len(accounts) != 2 || accounts[0].ID != alice.Account.ID || accounts[1].ID != bob.Account.ID {
		t.Fatalf("List() = %+v, error = %v, want the two accounts, oldest first", accounts, err)
	}
	tokens, err := manager.Tokens()
	if err != nil || len(tokens) != 2 || tokens[0].Token != tokenAliceNew || tokens[1].Token != tokenBob || !tokens[0].Usable || !tokens[1].Usable {
		t.Fatalf("Tokens() = %+v, error = %v", tokens, err)
	}

	// The same token again is the same account.
	again := mustImport(t, manager, tokenBob)
	if again.Created || again.Account.ID != bob.Account.ID {
		t.Fatalf("the same token again must not add an account: %+v", again)
	}
}

func TestImportRefusesTokensThatCannotWork(t *testing.T) {
	manager, fake, _ := newTestManager(t)

	_, err := manager.Import(context.Background(), credentialsFile(tokenAlice, testNow.Add(-time.Minute)), nil)
	if err == nil || !strings.HasPrefix(err.Error(), CodeTokenExpired+":") {
		t.Fatalf("an expired token: error = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("an expired token must not be sent to Anthropic: %v", fake.calls)
	}

	_, err = manager.Import(context.Background(), "sk-ant-oat01-unknown-token-DDDDDDDDDDDDDDDD4444", nil)
	if err == nil || !strings.HasPrefix(err.Error(), CodeTokenRejected+":") {
		t.Fatalf("a token Anthropic refuses: error = %v", err)
	}

	fake.mu.Lock()
	fake.down = true
	fake.mu.Unlock()
	_, err = manager.Import(context.Background(), tokenAlice, nil)
	if err == nil || !strings.HasPrefix(err.Error(), CodeUnreachable+":") {
		t.Fatalf("Anthropic out of reach: error = %v", err)
	}
	if strings.Contains(err.Error(), tokenAlice) {
		t.Fatalf("the error leaks the token: %v", err)
	}

	accounts, err := manager.List()
	if err != nil || len(accounts) != 0 {
		t.Fatalf("List() = %+v, error = %v, want nothing saved", accounts, err)
	}
}

// TestImportWithoutProfileAccess covers a token that works but may not read
// the profile, which the Claude CLI's long-lived tokens can be: it is saved
// without a profile, and the same token again is the same account. Which
// status Anthropic answers such a token with is not known here, so both that
// can mean it are covered.
func TestImportWithoutProfileAccess(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			manager, fake, _ := newTestManager(t)
			fake.status[tokenAlice+" /api/oauth/profile"] = status
			fake.status[tokenAlice+" /api/oauth/usage"] = status

			result := mustImport(t, manager, credentialsFile(tokenAlice, testNow.Add(time.Hour)))
			if !result.Created || result.Account.Email != "" || result.Account.Plan != "pro" || result.Account.Status != StatusOK {
				t.Fatalf("account = %+v, want a usable one without a profile and with the plan of the credentials file", result.Account)
			}
			if !strings.HasPrefix(result.Account.DetailsError, CodeDetailsUnavailable+":") || result.Account.UsageAtUnixMS != 0 || len(result.Account.UsageWindows) != 0 {
				t.Fatalf("details error = %q, usage read time = %d, usage windows = %v", result.Account.DetailsError, result.Account.UsageAtUnixMS, result.Account.UsageWindows)
			}
			if tokens, _ := manager.Tokens(); len(tokens) != 1 || !tokens[0].Usable {
				t.Fatalf("Tokens() = %+v, want the token to count as usable", tokens)
			}
			if again := mustImport(t, manager, tokenAlice); again.Created || again.Account.ID != result.Account.ID {
				t.Fatalf("the same token again must be the same account: %+v", again)
			}

			// A refresh leaves such an account usable too.
			refreshed, err := manager.Refresh(context.Background(), result.Account.ID)
			if err != nil || refreshed.Status != StatusOK || !strings.HasPrefix(refreshed.DetailsError, CodeDetailsUnavailable+":") {
				t.Fatalf("refreshed account = %+v, error = %v", refreshed, err)
			}
		})
	}
}

// TestImportRefusesATokenThatCanDoNothing covers a token that may read neither
// the profile nor the model list: nothing shows that it works, so it is not
// saved.
func TestImportRefusesATokenThatCanDoNothing(t *testing.T) {
	manager, fake, _ := newTestManager(t)
	fake.status[tokenAlice+" /api/oauth/profile"] = http.StatusForbidden
	fake.status[tokenAlice+" /v1/models"] = http.StatusForbidden
	if _, err := manager.Import(context.Background(), tokenAlice, nil); err == nil || !strings.HasPrefix(err.Error(), CodeTokenRejected+":") {
		t.Fatalf("Import() error = %v", err)
	}

	// When the model list cannot be asked for, whether the token works is
	// not known, and that is what the error says.
	fake.status[tokenAlice+" /v1/models"] = http.StatusBadGateway
	if _, err := manager.Import(context.Background(), tokenAlice, nil); err == nil || !strings.HasPrefix(err.Error(), CodeUnreachable+":") {
		t.Fatalf("Import() error = %v", err)
	}
	if accounts, _ := manager.List(); len(accounts) != 0 {
		t.Fatalf("List() = %+v, want nothing saved", accounts)
	}
}

// TestProfileWithoutUsage covers a token that may read the profile and not
// the usage.
func TestProfileWithoutUsage(t *testing.T) {
	manager, fake, _ := newTestManager(t)
	fake.status[tokenAlice+" /api/oauth/usage"] = http.StatusForbidden
	account := mustImport(t, manager, tokenAlice).Account
	if account.Email != "alice@example.test" || account.Status != StatusOK || len(account.UsageWindows) != 0 {
		t.Fatalf("account = %+v", account)
	}
	if !strings.HasPrefix(account.DetailsError, CodeDetailsUnavailable+":") || !strings.Contains(account.DetailsError, "usage") {
		t.Fatalf("details error = %q", account.DetailsError)
	}
}

func TestImportAbandonedByBeforeSave(t *testing.T) {
	manager, _, _ := newTestManager(t)
	stop := errors.New("config_save_failed: disk full")
	_, err := manager.Import(context.Background(), tokenAlice, func(string, string, string) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("Import() error = %v, want the error of beforeSave", err)
	}
	if accounts, _ := manager.List(); len(accounts) != 0 {
		t.Fatalf("an abandoned import saved an account: %+v", accounts)
	}
}

func TestRefreshReportsARefusedTokenAndRecovers(t *testing.T) {
	manager, fake, _ := newTestManager(t)
	account := mustImport(t, manager, tokenAlice).Account

	// Anthropic refuses the token everywhere.
	fake.mu.Lock()
	models := fake.models[tokenAlice]
	delete(fake.profiles, tokenAlice)
	delete(fake.usages, tokenAlice)
	delete(fake.models, tokenAlice)
	fake.mu.Unlock()
	refused, err := manager.Refresh(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("Refresh() error = %v, want the refusal reported in the account", err)
	}
	if refused.Status != StatusRejected || !strings.HasPrefix(refused.DetailsError, CodeTokenRejected+":") {
		t.Fatalf("refused account = %+v", refused)
	}
	if refused.Email != "alice@example.test" || len(refused.UsageWindows) == 0 {
		t.Fatalf("what was known before must be kept: %+v", refused)
	}
	if tokens, _ := manager.Tokens(); len(tokens) != 1 || tokens[0].Usable {
		t.Fatalf("a refused token must not count as usable: %+v", tokens)
	}

	// While Anthropic cannot be asked, a refused account stays refused: that
	// it could not be asked says nothing about the token.
	fake.status[tokenAlice+" /api/oauth/profile"] = http.StatusBadGateway
	unknown, err := manager.Refresh(context.Background(), account.ID)
	if err != nil || unknown.Status != StatusRejected || !strings.HasPrefix(unknown.DetailsError, CodeUnreachable+":") {
		t.Fatalf("account while Anthropic cannot be asked = %+v, error = %v", unknown, err)
	}
	delete(fake.status, tokenAlice+" /api/oauth/profile")

	fake.mu.Lock()
	fake.profiles[tokenAlice] = profileJSON("acct-alice", "alice@example.test", "Alice", "claude_max", true)
	fake.usages[tokenAlice] = usageJSON
	fake.models[tokenAlice] = models
	fake.mu.Unlock()
	manager.now = func() time.Time { return testNow.Add(time.Hour) }
	recovered, err := manager.Refresh(context.Background(), account.ID)
	if err != nil || recovered.Status != StatusOK || recovered.DetailsError != "" || recovered.Plan != "max" {
		t.Fatalf("recovered account = %+v, error = %v", recovered, err)
	}
	if recovered.UsageAtUnixMS != testNow.Add(time.Hour).UnixMilli() {
		t.Fatalf("usage read time = %d, want the time of the refresh", recovered.UsageAtUnixMS)
	}

	// Usage that cannot be read is reported, and the profile is still updated.
	fake.status[tokenAlice+" /api/oauth/usage"] = http.StatusTooManyRequests
	partial, err := manager.Refresh(context.Background(), account.ID)
	if err != nil || partial.Status != StatusOK || !strings.HasPrefix(partial.DetailsError, CodeUnreachable+":") {
		t.Fatalf("account after a failed usage read = %+v, error = %v", partial, err)
	}

	// The same holds while the profile cannot be read: a usable account stays
	// usable, and what was known is kept.
	fake.status[tokenAlice+" /api/oauth/profile"] = http.StatusBadGateway
	outage, err := manager.Refresh(context.Background(), account.ID)
	if err != nil || outage.Status != StatusOK || !strings.HasPrefix(outage.DetailsError, CodeUnreachable+":") || outage.Email != "alice@example.test" {
		t.Fatalf("account during an outage = %+v, error = %v", outage, err)
	}

	if _, err := manager.Refresh(context.Background(), "no-such-account"); err == nil || !strings.HasPrefix(err.Error(), CodeNotFound+":") {
		t.Fatalf("Refresh(unknown account) error = %v", err)
	}
}

func TestStatusExpiredOnceTheExpiryPasses(t *testing.T) {
	manager, _, _ := newTestManager(t)
	account := mustImport(t, manager, credentialsFile(tokenAlice, testNow.Add(time.Hour))).Account
	if account.Status != StatusOK {
		t.Fatalf("status before the expiry = %q", account.Status)
	}
	manager.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	accounts, err := manager.List()
	if err != nil || accounts[0].Status != StatusExpired {
		t.Fatalf("status after the expiry = %+v, error = %v", accounts, err)
	}
	if tokens, _ := manager.Tokens(); tokens[0].Usable {
		t.Fatal("an expired token must not count as usable")
	}
}

func TestRemove(t *testing.T) {
	manager, _, _ := newTestManager(t)
	alice := mustImport(t, manager, tokenAlice).Account
	bob := mustImport(t, manager, tokenBob).Account

	stop := errors.New("config_save_failed: disk full")
	if err := manager.Remove(alice.ID, func(string) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("Remove() error = %v, want the error of beforeRemove", err)
	}
	if accounts, _ := manager.List(); len(accounts) != 2 {
		t.Fatalf("a refused removal removed an account: %+v", accounts)
	}

	var removedToken string
	if err := manager.Remove(alice.ID, func(token string) error { removedToken = token; return nil }); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if removedToken != tokenAlice {
		t.Fatalf("beforeRemove got %q, want the account's token", removedToken)
	}
	accounts, err := manager.List()
	if err != nil || len(accounts) != 1 || accounts[0].ID != bob.ID {
		t.Fatalf("List() = %+v, error = %v, want only the other account", accounts, err)
	}
	if err := manager.Remove(alice.ID, nil); err == nil || !strings.HasPrefix(err.Error(), CodeNotFound+":") {
		t.Fatalf("Remove(removed account) error = %v", err)
	}
}

func TestModelsAsksUsableAccountsInOrder(t *testing.T) {
	manager, fake, _ := newTestManager(t)
	if _, err := manager.Models(context.Background()); err == nil || !strings.HasPrefix(err.Error(), CodeNoUsableAccount+":") {
		t.Fatalf("Models() with no account: error = %v", err)
	}
	mustImport(t, manager, tokenAlice)
	mustImport(t, manager, tokenBob)

	models, err := manager.Models(context.Background())
	if err != nil || len(models) != 2 || models[0] != (Model{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"}) {
		t.Fatalf("Models() = %+v, error = %v, want the list of the first account", models, err)
	}

	fake.status[tokenAlice+" /v1/models"] = http.StatusInternalServerError
	models, err = manager.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "claude-haiku-5-5" {
		t.Fatalf("Models() = %+v, error = %v, want the list of the next account when the first fails", models, err)
	}

	fake.status[tokenBob+" /v1/models"] = http.StatusInternalServerError
	if _, err := manager.Models(context.Background()); err == nil || !strings.HasPrefix(err.Error(), CodeModelsFailed+":") {
		t.Fatalf("Models() with every account failing: error = %v", err)
	}
}

func TestListReportsADamagedAccountsFile(t *testing.T) {
	manager, _, dataRoot := newTestManager(t)
	if err := os.WriteFile(filepath.Join(dataRoot, storeFileName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.List(); err == nil || !strings.HasPrefix(err.Error(), CodeStoreFailed+":") {
		t.Fatalf("List() error = %v", err)
	}
	// A damaged file is not overwritten by an import: the accounts in it may
	// still be recoverable by hand.
	if _, err := manager.Import(context.Background(), tokenAlice, nil); err == nil || !strings.HasPrefix(err.Error(), CodeStoreFailed+":") {
		t.Fatalf("Import() over a damaged file: error = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dataRoot, storeFileName)); string(data) != "{broken" {
		t.Fatalf("the damaged file was changed: %q", data)
	}
}
