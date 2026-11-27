package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	serverconfig "cursor/internal/backend/server/config"
	"cursor/internal/claudeaccount"
	"cursor/internal/modelchannel"
	legacyruntime "cursor/internal/runtime"
)

const (
	claudeTestTokenAlice    = "sk-ant-oat01-alice-token-AAAAAAAAAAAAAAAAAAAA1111"
	claudeTestTokenAliceNew = "sk-ant-oat01-alice-token-BBBBBBBBBBBBBBBBBBBB2222"
	claudeTestTokenBob      = "sk-ant-oat01-bob-token-CCCCCCCCCCCCCCCCCCCCCC3333"
)

// newClaudeTestService returns a service with a config store and an account
// store in temporary directories, and a local server in the place of Anthropic
// that knows three tokens: two of one account and one of another.
func newClaudeTestService(t *testing.T) *ProxyService {
	t.Helper()
	service, _ := newClaudeTestServiceWithRevoke(t)
	return service
}

// newClaudeTestServiceWithRevoke is newClaudeTestService, and also returns a
// function that makes the server refuse a token from then on.
func newClaudeTestServiceWithRevoke(t *testing.T) (*ProxyService, func(token string)) {
	t.Helper()
	accountOf := map[string]string{
		claudeTestTokenAlice:    "alice",
		claudeTestTokenAliceNew: "alice",
		claudeTestTokenBob:      "bob",
	}
	var mu sync.Mutex
	revoked := make(map[string]bool)
	revoke := func(token string) {
		mu.Lock()
		defer mu.Unlock()
		revoked[token] = true
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		name, known := accountOf[token]
		mu.Lock()
		known = known && !revoked[token]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !known {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"}}`))
			return
		}
		switch r.URL.Path {
		case "/api/oauth/profile":
			_, _ = fmt.Fprintf(w, `{"account":{"uuid":"acct-%[1]s","display_name":%[1]q,"email":"%[1]s@example.test"},"organization":{"uuid":"org-%[1]s","name":"%[1]s's Organization","organization_type":"claude_pro","has_extra_usage_enabled":false}}`, name)
		case "/api/oauth/usage":
			_, _ = w.Write([]byte(`{"five_hour":{"utilization":10,"resets_at":"2026-11-27T17:50:00+00:00"},"seven_day":{"utilization":69,"resets_at":"2026-11-28T20:00:00+00:00"},"extra_usage":{"is_enabled":false}}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-6","display_name":"Claude Sonnet 4.6"},{"id":"claude-opus-4-8","display_name":"Claude Opus 4.8"}],"has_more":false}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	service := &ProxyService{
		store:         serverconfig.NewStore(filepath.Join(t.TempDir(), "config.yml"), t.TempDir()),
		claudeAccount: claudeaccount.NewManager(t.TempDir(), claudeaccount.NewAPIAt(server.URL, server.Client(), "claude-cli/test")),
	}
	cfg := serverconfig.DefaultConfig()
	cfg.ModelAdapters = []serverconfig.ModelAdapterConfig{
		{DisplayName: "Relay GPT", Type: "openai", BaseURL: "https://relay.example.com/v1", APIKey: "sk-relay", TooltipData: "relay", ModelID: "gpt-x", ReasoningEffort: "medium", OpenAIEndpoint: "/v1/chat/completions", OpenAIRequestGroup: "chat_completions"},
		{DisplayName: "Claude by API key", Type: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-ant-api03-key", APIKeys: []string{"sk-ant-api03-backup"}, TooltipData: "api", ModelID: "claude-sonnet-4-6"},
	}
	if err := service.SaveUserConfig(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return service, revoke
}

func claudeTestRows(t *testing.T, service *ProxyService) []serverconfig.ModelAdapterConfig {
	t.Helper()
	cfg, err := service.LoadUserConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg.ModelAdapters
}

// claudeTestKeysByModel returns, for the rows of the Claude subscription
// group, the keys of each model's row.
func claudeTestKeysByModel(t *testing.T, service *ProxyService) map[string][]string {
	t.Helper()
	keys := make(map[string][]string)
	for _, row := range claudeTestRows(t, service) {
		if row.GroupName == claudeAccountGroupName {
			keys[row.ModelID] = adapterKeys(row)
		}
	}
	return keys
}

func claudeTestOptions(ids ...string) ClaudeAccountAddModelsRequest {
	req := ClaudeAccountAddModelsRequest{}
	for _, id := range ids {
		req.Models = append(req.Models, ClaudeAccountModelOption{ID: id, DisplayName: "Display " + id})
	}
	return req
}

// TestClaudeAccountsServeModelsTogether walks through the life of the section
// with two accounts: models are added for the first, the second joins them, a
// new credential of the first replaces its token everywhere, and removing the
// accounts takes their tokens out again.
func TestClaudeAccountsServeModelsTogether(t *testing.T) {
	service := newClaudeTestService(t)
	unrelatedBefore := claudeTestRows(t, service)

	state, err := service.ListClaudeAccounts()
	if err != nil || len(state.Accounts) != 0 || len(state.Models) != 0 {
		t.Fatalf("state before any account = %+v, error = %v", state, err)
	}
	if _, err := service.AddClaudeAccountModels(claudeTestOptions("claude-sonnet-4-6")); err == nil || !strings.HasPrefix(err.Error(), claudeaccount.CodeNoUsableAccount+":") {
		t.Fatalf("adding models without an account: error = %v", err)
	}

	// The first account.
	alice, err := service.ImportClaudeAccount(claudeTestTokenAlice)
	if err != nil || !alice.Created || alice.UpdatedModels != 0 || len(alice.State.Accounts) != 1 {
		t.Fatalf("import of the first account = %+v, error = %v", alice, err)
	}
	if entry := alice.State.Accounts[0]; entry.Account.Email != "alice@example.test" || entry.Account.Plan != "pro" || entry.ModelCount != 0 || len(entry.Account.UsageWindows) != 2 {
		t.Fatalf("first account = %+v", entry)
	}

	catalog, err := service.FetchClaudeAccountModels()
	if err != nil || len(catalog.Models) != 2 || catalog.Models[0] != (ClaudeAccountModelOption{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"}) {
		t.Fatalf("model catalog = %+v, error = %v", catalog, err)
	}

	added, err := service.AddClaudeAccountModels(ClaudeAccountAddModelsRequest{Models: catalog.Models})
	if err != nil || added.Added != 2 || added.Skipped != 0 || len(added.State.Models) != 2 {
		t.Fatalf("adding models = %+v, error = %v", added, err)
	}
	for _, model := range added.State.Models {
		if !reflect.DeepEqual(model.AccountIDs, []string{alice.AccountID}) || model.GroupName != claudeAccountGroupName {
			t.Fatalf("model = %+v, want it served by the first account", model)
		}
	}
	var sonnet serverconfig.ModelAdapterConfig
	for _, row := range claudeTestRows(t, service) {
		if row.GroupName == claudeAccountGroupName && row.ModelID == "claude-sonnet-4-6" {
			sonnet = row
		}
	}
	if sonnet.Type != "anthropic" || sonnet.BaseURL != claudeaccount.DefaultBaseURL || sonnet.AnthropicAuthMode != modelchannel.AnthropicAuthModeOAuth ||
		sonnet.DisplayName != "Claude Sonnet 4.6" || sonnet.ID == "" || sonnet.ProtocolGroup != modelchannel.ProtocolGroupAnthropicMessages ||
		legacyruntime.NormalizeModelSource(sonnet.Source) != legacyruntime.ModelSourceThirdParty {
		t.Fatalf("new model row = %+v", sonnet)
	}

	// A catalog read now marks the models as added, and adding them again
	// creates nothing.
	if catalog, _ = service.FetchClaudeAccountModels(); !catalog.Models[0].Added || !catalog.Models[1].Added {
		t.Fatalf("model catalog after adding = %+v, want both marked as added", catalog)
	}
	if again, err := service.AddClaudeAccountModels(ClaudeAccountAddModelsRequest{Models: catalog.Models}); err != nil || again.Added != 0 || again.Skipped != 2 {
		t.Fatalf("adding the same models again = %+v, error = %v", again, err)
	}

	// The second account joins the rows of the first.
	bob, err := service.ImportClaudeAccount(claudeTestTokenBob)
	if err != nil || !bob.Created || bob.UpdatedModels != 2 || len(bob.State.Accounts) != 2 {
		t.Fatalf("import of the second account = %+v, error = %v", bob, err)
	}
	wantPool := map[string][]string{
		"claude-sonnet-4-6": {claudeTestTokenAlice, claudeTestTokenBob},
		"claude-opus-4-8":   {claudeTestTokenAlice, claudeTestTokenBob},
	}
	if got := claudeTestKeysByModel(t, service); !reflect.DeepEqual(got, wantPool) {
		t.Fatalf("keys after the second account joined = %v, want %v", got, wantPool)
	}
	for _, model := range bob.State.Models {
		if !reflect.DeepEqual(model.AccountIDs, []string{alice.AccountID, bob.AccountID}) {
			t.Fatalf("model = %+v, want it served by both accounts, oldest first", model)
		}
	}
	for _, entry := range bob.State.Accounts {
		if entry.ModelCount != 2 {
			t.Fatalf("account = %+v, want it to serve both models", entry)
		}
	}

	// At request time the row is one channel per account.
	manager, err := serverconfig.NewManager(context.Background(), service.store)
	if err != nil {
		t.Fatalf("config manager: %v", err)
	}
	channels, err := manager.SelectChannelsForModel(context.Background(), sonnet.ID)
	if err != nil || len(channels) != 2 {
		t.Fatalf("channels for the model row = %d, error = %v, want one per account", len(channels), err)
	}
	channelKeys := []string{channels[0].APIKey, channels[1].APIKey}
	sort.Strings(channelKeys)
	if !reflect.DeepEqual(channelKeys, []string{claudeTestTokenAlice, claudeTestTokenBob}) || channels[0].AnthropicAuthMode != modelchannel.AnthropicAuthModeOAuth {
		t.Fatalf("channel keys = %v, auth mode = %q", channelKeys, channels[0].AnthropicAuthMode)
	}

	// A new credential of the first account replaces its token in every row.
	renewed, err := service.ImportClaudeAccount(claudeTestTokenAliceNew)
	if err != nil || renewed.Created || renewed.AccountID != alice.AccountID || renewed.UpdatedModels != 2 || len(renewed.State.Accounts) != 2 {
		t.Fatalf("new credential of the first account = %+v, error = %v", renewed, err)
	}
	wantPool = map[string][]string{
		"claude-sonnet-4-6": {claudeTestTokenAliceNew, claudeTestTokenBob},
		"claude-opus-4-8":   {claudeTestTokenAliceNew, claudeTestTokenBob},
	}
	if got := claudeTestKeysByModel(t, service); !reflect.DeepEqual(got, wantPool) {
		t.Fatalf("keys after the new credential = %v, want %v", got, wantPool)
	}
	for _, row := range claudeTestRows(t, service) {
		if row.GroupName == claudeAccountGroupName && row.ModelID == "claude-sonnet-4-6" && row.ID != sonnet.ID {
			t.Fatalf("the row's id changed with its token: %q -> %q", sonnet.ID, row.ID)
		}
	}

	// Removing the first account leaves the rows to the second.
	removed, err := service.RemoveClaudeAccount(alice.AccountID)
	if err != nil || removed.UpdatedModels != 2 || removed.RemovedModels != 0 || len(removed.State.Accounts) != 1 || len(removed.State.Models) != 2 {
		t.Fatalf("removing the first account = %+v, error = %v", removed, err)
	}
	wantPool = map[string][]string{"claude-sonnet-4-6": {claudeTestTokenBob}, "claude-opus-4-8": {claudeTestTokenBob}}
	if got := claudeTestKeysByModel(t, service); !reflect.DeepEqual(got, wantPool) {
		t.Fatalf("keys after removing the first account = %v, want %v", got, wantPool)
	}

	// Removing the last account removes the rows only it served.
	last, err := service.RemoveClaudeAccount(bob.AccountID)
	if err != nil || last.UpdatedModels != 0 || last.RemovedModels != 2 || len(last.State.Accounts) != 0 || len(last.State.Models) != 0 {
		t.Fatalf("removing the last account = %+v, error = %v", last, err)
	}
	if got := claudeTestRows(t, service); !reflect.DeepEqual(got, unrelatedBefore) {
		t.Fatalf("rows that hold no account token changed:\n got  %+v\n want %+v", got, unrelatedBefore)
	}
	if _, err := service.RemoveClaudeAccount(bob.AccountID); err == nil || !strings.HasPrefix(err.Error(), claudeaccount.CodeNotFound+":") {
		t.Fatalf("removing a removed account: error = %v", err)
	}
}

// TestClaudeAccountsPickUpRowsMadeByHand covers a row the user set up in the
// model editor with an account's token: it counts as served by that account,
// and a second account joins it.
func TestClaudeAccountsPickUpRowsMadeByHand(t *testing.T) {
	service := newClaudeTestService(t)
	cfg, _ := service.LoadUserConfig()
	cfg.ModelAdapters = append(cfg.ModelAdapters, serverconfig.ModelAdapterConfig{
		DisplayName: "My Claude", GroupName: "Mine", Type: "anthropic", BaseURL: "https://api.anthropic.com",
		APIKey: claudeTestTokenAlice, TooltipData: "by hand", ModelID: "claude-haiku-5-5", AnthropicAuthMode: "auto",
	})
	if err := service.SaveUserConfig(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	alice, err := service.ImportClaudeAccount(claudeTestTokenAlice)
	if err != nil || len(alice.State.Models) != 1 || alice.State.Models[0].DisplayName != "My Claude" || alice.State.Accounts[0].ModelCount != 1 {
		t.Fatalf("state = %+v, error = %v, want the hand-made row served by the account", alice.State, err)
	}
	bob, err := service.ImportClaudeAccount(claudeTestTokenBob)
	if err != nil || bob.UpdatedModels != 1 {
		t.Fatalf("import of the second account = %+v, error = %v", bob, err)
	}
	for _, row := range claudeTestRows(t, service) {
		if row.DisplayName == "My Claude" && !reflect.DeepEqual(adapterKeys(row), []string{claudeTestTokenAlice, claudeTestTokenBob}) {
			t.Fatalf("keys of the hand-made row = %v", adapterKeys(row))
		}
	}
}

// TestClaudeAccountRejoinsModelsWithNewCredential covers an account whose
// token stopped working: models added meanwhile are served by the other
// account only, and the account's new credential puts it back on every model.
func TestClaudeAccountRejoinsModelsWithNewCredential(t *testing.T) {
	service, revoke := newClaudeTestServiceWithRevoke(t)
	alice, err := service.ImportClaudeAccount(claudeTestTokenAlice)
	if err != nil {
		t.Fatalf("import of the first account: %v", err)
	}
	if _, err := service.ImportClaudeAccount(claudeTestTokenBob); err != nil {
		t.Fatalf("import of the second account: %v", err)
	}
	if _, err := service.AddClaudeAccountModels(claudeTestOptions("claude-sonnet-4-6")); err != nil {
		t.Fatalf("add the first model: %v", err)
	}

	// Anthropic refuses the first account's token; a refresh finds out.
	revoke(claudeTestTokenAlice)
	state, err := service.RefreshClaudeAccount(alice.AccountID)
	if err != nil || state.Accounts[0].Account.Status != claudeaccount.StatusRejected || state.Accounts[1].Account.Status != claudeaccount.StatusOK {
		t.Fatalf("state after the refresh = %+v, error = %v, want the first account refused", state.Accounts, err)
	}
	if status, count, _ := service.overviewClaudeAccounts(); status != "warning" || count != 2 {
		t.Fatalf("overview with a refused account = %q, %d", status, count)
	}
	// The refused token stays in the row it was in: the router skips a key
	// the provider refuses, and the row keeps its tie to the account.
	if _, err := service.AddClaudeAccountModels(claudeTestOptions("claude-opus-4-8")); err != nil {
		t.Fatalf("add the second model: %v", err)
	}
	wantPool := map[string][]string{
		"claude-sonnet-4-6": {claudeTestTokenAlice, claudeTestTokenBob},
		"claude-opus-4-8":   {claudeTestTokenBob},
	}
	if got := claudeTestKeysByModel(t, service); !reflect.DeepEqual(got, wantPool) {
		t.Fatalf("keys while the first account is refused = %v, want %v", got, wantPool)
	}

	renewed, err := service.ImportClaudeAccount(claudeTestTokenAliceNew)
	if err != nil || renewed.Created || renewed.AccountID != alice.AccountID || renewed.UpdatedModels != 2 {
		t.Fatalf("new credential of the first account = %+v, error = %v", renewed, err)
	}
	wantPool = map[string][]string{
		"claude-sonnet-4-6": {claudeTestTokenAliceNew, claudeTestTokenBob},
		"claude-opus-4-8":   {claudeTestTokenBob, claudeTestTokenAliceNew},
	}
	if got := claudeTestKeysByModel(t, service); !reflect.DeepEqual(got, wantPool) {
		t.Fatalf("keys after the new credential = %v, want %v", got, wantPool)
	}
	for _, entry := range renewed.State.Accounts {
		if entry.Account.Status != claudeaccount.StatusOK || entry.ModelCount != 2 {
			t.Fatalf("account after the new credential = %+v, want it usable and serving both models", entry)
		}
	}

	// Submitting the same credential again changes nothing.
	again, err := service.ImportClaudeAccount(claudeTestTokenAliceNew)
	if err != nil || again.Created || again.UpdatedModels != 0 {
		t.Fatalf("the same credential again = %+v, error = %v", again, err)
	}
	if got := claudeTestKeysByModel(t, service); !reflect.DeepEqual(got, wantPool) {
		t.Fatalf("keys after the same credential again = %v, want %v", got, wantPool)
	}
}

// TestClaudeAccountModelRowsCanBeChanged covers the two changes the section
// makes to single model rows: Fast mode, and removal.
func TestClaudeAccountModelRowsCanBeChanged(t *testing.T) {
	service := newClaudeTestService(t)
	unrelatedBefore := claudeTestRows(t, service)
	relayID := unrelatedBefore[0].ID
	if relayID == "" {
		t.Fatal("the unrelated row has no id")
	}
	if _, err := service.ImportClaudeAccount(claudeTestTokenAlice); err != nil {
		t.Fatalf("import: %v", err)
	}
	added, err := service.AddClaudeAccountModels(claudeTestOptions("claude-sonnet-4-6", "claude-opus-4-8"))
	if err != nil || len(added.State.Models) != 2 {
		t.Fatalf("add models = %+v, error = %v", added, err)
	}
	sonnetID, opusID := added.State.Models[0].ID, added.State.Models[1].ID

	// Fast mode is stored in the row.
	state, err := service.SetClaudeAccountModelFastMode(sonnetID, true)
	if err != nil || !state.Models[0].FastMode || state.Models[1].FastMode {
		t.Fatalf("Fast mode on = %+v, error = %v, want it on for the first model only", state.Models, err)
	}
	for _, row := range claudeTestRows(t, service) {
		if row.FastMode != (row.ID == sonnetID) {
			t.Fatalf("row %q (%s): FastMode = %v", row.DisplayName, row.ModelID, row.FastMode)
		}
	}
	if state, err = service.SetClaudeAccountModelFastMode(sonnetID, true); err != nil || !state.Models[0].FastMode {
		t.Fatalf("Fast mode on again = %+v, error = %v", state.Models, err)
	}
	if state, err = service.SetClaudeAccountModelFastMode(sonnetID, false); err != nil || state.Models[0].FastMode {
		t.Fatalf("Fast mode off = %+v, error = %v", state.Models, err)
	}
	// A row no saved account serves is refused and left as it was.
	for _, id := range []string{relayID, "no-such-row", ""} {
		if _, err := service.SetClaudeAccountModelFastMode(id, true); err == nil || !strings.HasPrefix(err.Error(), codeClaudeModelNotFound+":") {
			t.Fatalf("Fast mode for row %q: error = %v", id, err)
		}
	}

	// Removal takes the served rows among the selected ones and no other.
	removed, err := service.RemoveClaudeAccountModels(ClaudeAccountRemoveModelsRequest{IDs: []string{opusID, relayID, "no-such-row"}})
	if err != nil || removed.Removed != 1 || len(removed.State.Models) != 1 || removed.State.Models[0].ID != sonnetID {
		t.Fatalf("removing one model = %+v, error = %v", removed, err)
	}
	if rows := claudeTestRows(t, service); len(rows) != len(unrelatedBefore)+1 || !reflect.DeepEqual(rows[:len(unrelatedBefore)], unrelatedBefore) {
		t.Fatalf("rows after removing one model = %+v", rows)
	}
	if _, err := service.RemoveClaudeAccountModels(ClaudeAccountRemoveModelsRequest{IDs: []string{relayID}}); err == nil || !strings.HasPrefix(err.Error(), codeClaudeModelNotFound+":") {
		t.Fatalf("removing a row no account serves: error = %v", err)
	}
	if _, err := service.RemoveClaudeAccountModels(ClaudeAccountRemoveModelsRequest{IDs: []string{" "}}); err == nil || !strings.HasPrefix(err.Error(), codeClaudeNoModels+":") {
		t.Fatalf("removing nothing: error = %v", err)
	}

	// The account stays when its last model row goes.
	last, err := service.RemoveClaudeAccountModels(ClaudeAccountRemoveModelsRequest{IDs: []string{sonnetID}})
	if err != nil || last.Removed != 1 || len(last.State.Models) != 0 || len(last.State.Accounts) != 1 || last.State.Accounts[0].ModelCount != 0 {
		t.Fatalf("removing the last model = %+v, error = %v", last, err)
	}
	if rows := claudeTestRows(t, service); !reflect.DeepEqual(rows, unrelatedBefore) {
		t.Fatalf("rows that hold no account token changed:\n got  %+v\n want %+v", rows, unrelatedBefore)
	}
}

func TestClaudeAccountsStateHoldsNoToken(t *testing.T) {
	service := newClaudeTestService(t)
	imported, err := service.ImportClaudeAccount(claudeTestTokenAlice)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	added, err := service.AddClaudeAccountModels(claudeTestOptions("claude-sonnet-4-6"))
	if err != nil {
		t.Fatalf("add models: %v", err)
	}
	removed, err := service.RemoveClaudeAccount(imported.AccountID)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	for name, value := range map[string]any{"import result": imported, "add result": added, "remove result": removed} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "alice-token") {
			t.Fatalf("the %s leaks the token: %s", name, encoded)
		}
	}
}

func TestClaudeAccountsRefuseBadInput(t *testing.T) {
	service := newClaudeTestService(t)
	if _, err := service.ImportClaudeAccount("sk-ant-oat01-unknown-token-DDDDDDDDDDDDDDDD4444"); err == nil || !strings.HasPrefix(err.Error(), claudeaccount.CodeTokenRejected+":") {
		t.Fatalf("a token Anthropic refuses: error = %v", err)
	}
	if _, err := service.ImportClaudeAccount("sk-ant-api03-key"); err == nil || !strings.HasPrefix(err.Error(), claudeaccount.CodeNotOAuthToken+":") {
		t.Fatalf("an API key: error = %v", err)
	}
	if _, err := service.FetchClaudeAccountModels(); err == nil || !strings.HasPrefix(err.Error(), claudeaccount.CodeNoUsableAccount+":") {
		t.Fatalf("the model catalog without an account: error = %v", err)
	}
	if _, err := service.ImportClaudeAccount(claudeTestTokenAlice); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := service.AddClaudeAccountModels(ClaudeAccountAddModelsRequest{Models: []ClaudeAccountModelOption{{ID: "  "}}}); err == nil || !strings.HasPrefix(err.Error(), codeClaudeNoModels+":") {
		t.Fatalf("adding no model: error = %v", err)
	}

	var unavailable *ProxyService
	if _, err := unavailable.ListClaudeAccounts(); err == nil || !strings.HasPrefix(err.Error(), codeClaudeServiceUnavailable+":") {
		t.Fatalf("a service without the account manager: error = %v", err)
	}
	if _, err := (&ProxyService{}).ImportClaudeAccount(claudeTestTokenAlice); err == nil || !strings.HasPrefix(err.Error(), codeClaudeServiceUnavailable+":") {
		t.Fatalf("a service without the account manager: error = %v", err)
	}
}

func TestOverviewCountsClaudeAccounts(t *testing.T) {
	service := newClaudeTestService(t)
	if state, count, _ := service.overviewClaudeAccounts(); state != "empty" || count != 0 {
		t.Fatalf("overview without accounts = %q, %d", state, count)
	}
	if _, err := service.ImportClaudeAccount(claudeTestTokenAlice); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := service.ImportClaudeAccount(claudeTestTokenBob); err != nil {
		t.Fatalf("import: %v", err)
	}
	if state, count, _ := service.overviewClaudeAccounts(); state != "ready" || count != 2 {
		t.Fatalf("overview with two accounts = %q, %d", state, count)
	}
	if state, _, _ := (&ProxyService{}).overviewClaudeAccounts(); state != "error" {
		t.Fatalf("overview without the account manager = %q", state)
	}
}

func TestClaudeTokenRowHelpers(t *testing.T) {
	row := func(primary string, pool ...string) serverconfig.ModelAdapterConfig {
		return serverconfig.ModelAdapterConfig{Type: "anthropic", ModelID: "m", APIKey: primary, APIKeys: pool}
	}
	cursorRow := serverconfig.ModelAdapterConfig{Source: legacyruntime.ModelSourceCursorAccount, ModelID: "m", APIKey: "a"}

	rows, changed := joinClaudePool([]serverconfig.ModelAdapterConfig{row("a"), row("x"), row("b", "a"), row("a", "c"), cursorRow}, map[string]struct{}{"a": {}}, "c")
	if changed != 2 || !reflect.DeepEqual(adapterKeys(rows[0]), []string{"a", "c"}) || !reflect.DeepEqual(adapterKeys(rows[1]), []string{"x"}) ||
		!reflect.DeepEqual(adapterKeys(rows[2]), []string{"b", "a", "c"}) || !reflect.DeepEqual(adapterKeys(rows[3]), []string{"a", "c"}) || !reflect.DeepEqual(rows[4], cursorRow) {
		t.Fatalf("joinClaudePool: changed = %d, rows = %+v", changed, rows)
	}

	rows, changed = replaceClaudeToken([]serverconfig.ModelAdapterConfig{row("a", "b"), row("b", "a"), row("x"), row("a", "n"), cursorRow}, "a", "n")
	if changed != 3 || !reflect.DeepEqual(adapterKeys(rows[0]), []string{"n", "b"}) || !reflect.DeepEqual(adapterKeys(rows[1]), []string{"b", "n"}) ||
		!reflect.DeepEqual(adapterKeys(rows[2]), []string{"x"}) || !reflect.DeepEqual(adapterKeys(rows[3]), []string{"n"}) || !reflect.DeepEqual(rows[4], cursorRow) {
		t.Fatalf("replaceClaudeToken: changed = %d, rows = %+v", changed, rows)
	}

	rows, updated, removed := removeClaudeToken([]serverconfig.ModelAdapterConfig{row("a", "b", "c"), row("b", "a"), row("a"), row("x"), cursorRow}, "a")
	if updated != 2 || removed != 1 || len(rows) != 4 || !reflect.DeepEqual(adapterKeys(rows[0]), []string{"b", "c"}) ||
		!reflect.DeepEqual(adapterKeys(rows[1]), []string{"b"}) || !reflect.DeepEqual(adapterKeys(rows[2]), []string{"x"}) || !reflect.DeepEqual(rows[3], cursorRow) {
		t.Fatalf("removeClaudeToken: updated = %d, removed = %d, rows = %+v", updated, removed, rows)
	}

	requested := normalizeClaudeModelRequest([]ClaudeAccountModelOption{{ID: " claude-a ", DisplayName: " A "}, {ID: "CLAUDE-A"}, {ID: ""}, {ID: "claude-b"}})
	if !reflect.DeepEqual(requested, []ClaudeAccountModelOption{{ID: "claude-a", DisplayName: "A"}, {ID: "claude-b", DisplayName: "claude-b"}}) {
		t.Fatalf("normalizeClaudeModelRequest = %+v", requested)
	}
}
