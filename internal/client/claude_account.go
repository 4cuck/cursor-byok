// claude_account.go is the Control Center's Claude subscription section: the
// saved Claude accounts and the model rows they serve.
//
// All saved accounts serve the same models. A model row holds the token of
// every account as its key pool (apiKey plus apiKeys), which is the mechanism
// the router already has for several keys on one model: new conversations
// rotate over the keys, a conversation stays on the key it started with, and a
// key the provider refuses is left alone until its cooldown ends while the
// request goes on with the next one.
//
// A row belongs to this section when one of its keys is the token of a saved
// account. Nothing else marks it, so a row the user set up by hand with such a
// token is picked up as well.
package client

import (
	"context"
	"errors"
	"strings"
	"time"

	serverconfig "cursor/internal/backend/server/config"
	"cursor/internal/claudeaccount"
	"cursor/internal/modelchannel"
)

const (
	// claudeAccountGroupName is the group new Claude subscription model rows
	// are put in.
	claudeAccountGroupName = "Claude Subscription"
	// claudeAccountSupplierID is the supplier template of those rows.
	claudeAccountSupplierID = "anthropic"
	// claudeAccountTooltip is the note of those rows.
	claudeAccountTooltip = "Claude subscription"
	// claudeAccountTimeout bounds one exchange with Anthropic about accounts.
	claudeAccountTimeout = 30 * time.Second
	// claudeAccountMaxModelsPerRequest bounds how many models one request adds.
	claudeAccountMaxModelsPerRequest = 200

	codeClaudeServiceUnavailable = "claude_subscription_service_unavailable"
	codeClaudeNoModels           = "claude_subscription_no_models_selected"
	codeClaudeModelNotFound      = "claude_subscription_model_not_found"
)

// ClaudeAccountEntry is one saved Claude account as the Control Center shows it.
type ClaudeAccountEntry struct {
	Account claudeaccount.Account `json:"account"`
	// ModelCount is how many model rows hold this account's token.
	ModelCount int `json:"modelCount"`
}

// ClaudeAccountModel is one model row that saved Claude accounts serve.
type ClaudeAccountModel struct {
	// ID is the id of the model row.
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	ModelID     string `json:"modelId"`
	GroupName   string `json:"groupName,omitempty"`
	Disabled    bool   `json:"disabled"`
	FastMode    bool   `json:"fastMode"`
	// AccountIDs are the saved accounts whose token the row holds, oldest
	// account first.
	AccountIDs []string `json:"accountIds"`
}

// ClaudeAccountsState is everything the Claude subscription section shows.
type ClaudeAccountsState struct {
	Accounts []ClaudeAccountEntry `json:"accounts"`
	Models   []ClaudeAccountModel `json:"models"`
}

// ClaudeAccountImportResult is the outcome of ImportClaudeAccount.
type ClaudeAccountImportResult struct {
	State     ClaudeAccountsState `json:"state"`
	AccountID string              `json:"accountId"`
	// Created is whether the credential added an account. When false it
	// replaced the credential of a saved account.
	Created bool `json:"created"`
	// UpdatedModels is how many model rows got the account's token.
	UpdatedModels int `json:"updatedModels"`
}

// ClaudeAccountRemoveResult is the outcome of RemoveClaudeAccount.
type ClaudeAccountRemoveResult struct {
	State ClaudeAccountsState `json:"state"`
	// UpdatedModels is how many model rows go on with the other accounts.
	UpdatedModels int `json:"updatedModels"`
	// RemovedModels is how many model rows were removed because the account
	// was the only one serving them.
	RemovedModels int `json:"removedModels"`
}

// ClaudeAccountModelOption is one model that can be added for the saved
// accounts.
type ClaudeAccountModelOption struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	// Added is whether a model row for it exists already.
	Added bool `json:"added"`
}

// ClaudeAccountModelCatalog is the models the saved accounts can use.
type ClaudeAccountModelCatalog struct {
	Models []ClaudeAccountModelOption `json:"models"`
}

// ClaudeAccountAddModelsRequest names the models to add.
type ClaudeAccountAddModelsRequest struct {
	Models []ClaudeAccountModelOption `json:"models"`
}

// ClaudeAccountAddModelsResult is the outcome of AddClaudeAccountModels.
type ClaudeAccountAddModelsResult struct {
	State ClaudeAccountsState `json:"state"`
	Added int                 `json:"added"`
	// Skipped is how many of the requested models had a row already.
	Skipped int `json:"skipped"`
}

// ClaudeAccountRemoveModelsRequest names the model rows to remove.
type ClaudeAccountRemoveModelsRequest struct {
	// IDs are ids of model rows, as in ClaudeAccountModel.ID.
	IDs []string `json:"ids"`
}

// ClaudeAccountRemoveModelsResult is the outcome of RemoveClaudeAccountModels.
type ClaudeAccountRemoveModelsResult struct {
	State   ClaudeAccountsState `json:"state"`
	Removed int                 `json:"removed"`
}

func claudeServiceUnavailable() error {
	return errors.New(codeClaudeServiceUnavailable + ": the Claude account service is not initialized")
}

// ListClaudeAccounts returns the saved Claude accounts and the models they
// serve. It reads what is stored and does not contact Anthropic.
func (s *ProxyService) ListClaudeAccounts() (ClaudeAccountsState, error) {
	if s == nil || s.claudeAccount == nil {
		return ClaudeAccountsState{}, claudeServiceUnavailable()
	}
	return s.claudeAccountsState()
}

// ImportClaudeAccount saves the credential the user submitted: the contents of
// a Claude CLI credentials file, or an access token. A credential of an
// account that is saved already replaces that account's token in the store and
// in every model row; a credential of another account adds an account. Either
// way the token then joins the key pool of every row the saved accounts serve,
// which also covers rows that were added while the account's previous token
// was unusable.
func (s *ProxyService) ImportClaudeAccount(credentials string) (ClaudeAccountImportResult, error) {
	if s == nil || s.claudeAccount == nil {
		return ClaudeAccountImportResult{}, claudeServiceUnavailable()
	}
	// The tokens saved before the import mark the rows the token joins. They
	// are read here because the manager is locked while the callback runs.
	before, err := s.claudeAccount.Tokens()
	if err != nil {
		return ClaudeAccountImportResult{}, err
	}
	saved := claudeTokenSet(before)
	ctx, cancel := context.WithTimeout(context.Background(), claudeAccountTimeout)
	defer cancel()

	updated := 0
	result, err := s.claudeAccount.Import(ctx, credentials, func(_, previousToken, token string) error {
		changed, _, err := s.updateClaudeAccountRows(func(rows []serverconfig.ModelAdapterConfig) ([]serverconfig.ModelAdapterConfig, int, int) {
			replaced := 0
			if previousToken != "" && previousToken != token {
				rows, replaced = replaceClaudeToken(rows, previousToken, token)
			}
			// A row counted as replaced holds the token now, so joining does
			// not count it again.
			rows, joined := joinClaudePool(rows, saved, token)
			return rows, replaced + joined, 0
		})
		updated = changed
		return err
	})
	if err != nil {
		return ClaudeAccountImportResult{}, err
	}
	state, err := s.claudeAccountsState()
	if err != nil {
		return ClaudeAccountImportResult{}, err
	}
	return ClaudeAccountImportResult{State: state, AccountID: result.Account.ID, Created: result.Created, UpdatedModels: updated}, nil
}

// RefreshClaudeAccount reads the account's profile and usage from Anthropic
// again. A failure to read them is reported in the account, not as an error.
func (s *ProxyService) RefreshClaudeAccount(accountID string) (ClaudeAccountsState, error) {
	if s == nil || s.claudeAccount == nil {
		return ClaudeAccountsState{}, claudeServiceUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeAccountTimeout)
	defer cancel()
	if _, err := s.claudeAccount.Refresh(ctx, strings.TrimSpace(accountID)); err != nil {
		return ClaudeAccountsState{}, err
	}
	return s.claudeAccountsState()
}

// RemoveClaudeAccount deletes a saved account and takes its token out of every
// model row. A row other accounts serve too goes on with them; a row only this
// account served is removed, because a row cannot be without a key.
func (s *ProxyService) RemoveClaudeAccount(accountID string) (ClaudeAccountRemoveResult, error) {
	if s == nil || s.claudeAccount == nil {
		return ClaudeAccountRemoveResult{}, claudeServiceUnavailable()
	}
	var updated, removed int
	err := s.claudeAccount.Remove(strings.TrimSpace(accountID), func(token string) error {
		var err error
		updated, removed, err = s.updateClaudeAccountRows(func(rows []serverconfig.ModelAdapterConfig) ([]serverconfig.ModelAdapterConfig, int, int) {
			return removeClaudeToken(rows, token)
		})
		return err
	})
	if err != nil {
		return ClaudeAccountRemoveResult{}, err
	}
	state, err := s.claudeAccountsState()
	if err != nil {
		return ClaudeAccountRemoveResult{}, err
	}
	return ClaudeAccountRemoveResult{State: state, UpdatedModels: updated, RemovedModels: removed}, nil
}

// FetchClaudeAccountModels lists the models the saved accounts can use and
// marks the ones that have a model row already.
func (s *ProxyService) FetchClaudeAccountModels() (ClaudeAccountModelCatalog, error) {
	if s == nil || s.claudeAccount == nil {
		return ClaudeAccountModelCatalog{}, claudeServiceUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeAccountTimeout)
	defer cancel()
	models, err := s.claudeAccount.Models(ctx)
	if err != nil {
		return ClaudeAccountModelCatalog{}, err
	}
	tokens, err := s.claudeAccount.Tokens()
	if err != nil {
		return ClaudeAccountModelCatalog{}, err
	}
	cfg, err := s.LoadUserConfig()
	if err != nil {
		return ClaudeAccountModelCatalog{}, err
	}
	served := claudeServedModelIDs(cfg.ModelAdapters, claudeTokenSet(tokens))
	catalog := ClaudeAccountModelCatalog{Models: make([]ClaudeAccountModelOption, 0, len(models))}
	for _, model := range models {
		_, added := served[strings.ToLower(model.ID)]
		displayName := model.DisplayName
		if displayName == "" {
			displayName = model.ID
		}
		catalog.Models = append(catalog.Models, ClaudeAccountModelOption{ID: model.ID, DisplayName: displayName, Added: added})
	}
	return catalog, nil
}

// AddClaudeAccountModels creates a model row for each requested model that has
// none yet. Every row gets the tokens of all accounts whose token is usable.
func (s *ProxyService) AddClaudeAccountModels(req ClaudeAccountAddModelsRequest) (ClaudeAccountAddModelsResult, error) {
	if s == nil || s.claudeAccount == nil {
		return ClaudeAccountAddModelsResult{}, claudeServiceUnavailable()
	}
	requested := normalizeClaudeModelRequest(req.Models)
	if len(requested) == 0 {
		return ClaudeAccountAddModelsResult{}, errors.New(codeClaudeNoModels + ": no model was selected")
	}
	tokens, err := s.claudeAccount.Tokens()
	if err != nil {
		return ClaudeAccountAddModelsResult{}, err
	}
	var usable []string
	for _, item := range tokens {
		if item.Usable {
			usable = append(usable, item.Token)
		}
	}
	if len(usable) == 0 {
		return ClaudeAccountAddModelsResult{}, errors.New(claudeaccount.CodeNoUsableAccount + ": no saved Claude account has a usable token")
	}

	skipped := 0
	added, _, err := s.updateClaudeAccountRows(func(rows []serverconfig.ModelAdapterConfig) ([]serverconfig.ModelAdapterConfig, int, int) {
		served := claudeServedModelIDs(rows, claudeTokenSet(tokens))
		next := rows
		count := 0
		for _, model := range requested {
			if _, exists := served[strings.ToLower(model.ID)]; exists {
				skipped++
				continue
			}
			next = append(next, newClaudeAccountRow(model, usable))
			served[strings.ToLower(model.ID)] = struct{}{}
			count++
		}
		return next, count, 0
	})
	if err != nil {
		return ClaudeAccountAddModelsResult{}, err
	}
	state, err := s.claudeAccountsState()
	if err != nil {
		return ClaudeAccountAddModelsResult{}, err
	}
	return ClaudeAccountAddModelsResult{State: state, Added: added, Skipped: skipped}, nil
}

// SetClaudeAccountModelFastMode turns Fast mode on or off for one model row
// the saved accounts serve.
func (s *ProxyService) SetClaudeAccountModelFastMode(modelRowID string, enabled bool) (ClaudeAccountsState, error) {
	if s == nil || s.claudeAccount == nil {
		return ClaudeAccountsState{}, claudeServiceUnavailable()
	}
	tokens, err := s.claudeAccount.Tokens()
	if err != nil {
		return ClaudeAccountsState{}, err
	}
	saved := claudeTokenSet(tokens)
	modelRowID = strings.TrimSpace(modelRowID)
	found := false
	_, _, err = s.updateClaudeAccountRows(func(rows []serverconfig.ModelAdapterConfig) ([]serverconfig.ModelAdapterConfig, int, int) {
		for index := range rows {
			if modelRowID == "" || rows[index].ID != modelRowID || !holdsAnyKey(rows[index], saved) {
				continue
			}
			found = true
			if rows[index].FastMode == enabled {
				return rows, 0, 0
			}
			rows[index].FastMode = enabled
			return rows, 1, 0
		}
		return rows, 0, 0
	})
	if err != nil {
		return ClaudeAccountsState{}, err
	}
	if !found {
		return ClaudeAccountsState{}, errors.New(codeClaudeModelNotFound + ": no saved Claude account serves this model row")
	}
	return s.claudeAccountsState()
}

// RemoveClaudeAccountModels deletes model rows the saved accounts serve. Ids
// of rows they do not serve are left alone.
func (s *ProxyService) RemoveClaudeAccountModels(req ClaudeAccountRemoveModelsRequest) (ClaudeAccountRemoveModelsResult, error) {
	if s == nil || s.claudeAccount == nil {
		return ClaudeAccountRemoveModelsResult{}, claudeServiceUnavailable()
	}
	wanted := make(map[string]struct{}, len(req.IDs))
	for _, id := range req.IDs {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return ClaudeAccountRemoveModelsResult{}, errors.New(codeClaudeNoModels + ": no model was selected")
	}
	tokens, err := s.claudeAccount.Tokens()
	if err != nil {
		return ClaudeAccountRemoveModelsResult{}, err
	}
	saved := claudeTokenSet(tokens)
	removed, _, err := s.updateClaudeAccountRows(func(rows []serverconfig.ModelAdapterConfig) ([]serverconfig.ModelAdapterConfig, int, int) {
		kept := make([]serverconfig.ModelAdapterConfig, 0, len(rows))
		for _, row := range rows {
			if _, selected := wanted[row.ID]; selected && holdsAnyKey(row, saved) {
				continue
			}
			kept = append(kept, row)
		}
		return kept, len(rows) - len(kept), 0
	})
	if err != nil {
		return ClaudeAccountRemoveModelsResult{}, err
	}
	if removed == 0 {
		return ClaudeAccountRemoveModelsResult{}, errors.New(codeClaudeModelNotFound + ": no saved Claude account serves the selected model rows")
	}
	state, err := s.claudeAccountsState()
	if err != nil {
		return ClaudeAccountRemoveModelsResult{}, err
	}
	return ClaudeAccountRemoveModelsResult{State: state, Removed: removed}, nil
}

// claudeAccountsState builds the section's state from the account store and
// the model rows.
func (s *ProxyService) claudeAccountsState() (ClaudeAccountsState, error) {
	accounts, err := s.claudeAccount.List()
	if err != nil {
		return ClaudeAccountsState{}, err
	}
	tokens, err := s.claudeAccount.Tokens()
	if err != nil {
		return ClaudeAccountsState{}, err
	}
	cfg, err := s.LoadUserConfig()
	if err != nil {
		return ClaudeAccountsState{}, err
	}
	owner := make(map[string]string, len(tokens))
	order := make(map[string]int, len(tokens))
	for index, item := range tokens {
		owner[item.Token] = item.AccountID
		order[item.AccountID] = index
	}

	state := ClaudeAccountsState{
		Accounts: make([]ClaudeAccountEntry, 0, len(accounts)),
		Models:   []ClaudeAccountModel{},
	}
	counts := make(map[string]int, len(accounts))
	for _, row := range cfg.ModelAdapters {
		if isCursorAccountModelAdapter(row) {
			continue
		}
		accountIDs := make([]string, len(tokens))
		found := 0
		for _, key := range adapterKeys(row) {
			if id, ok := owner[key]; ok && accountIDs[order[id]] == "" {
				accountIDs[order[id]] = id
				found++
			}
		}
		if found == 0 {
			continue
		}
		model := ClaudeAccountModel{
			ID:          row.ID,
			DisplayName: row.DisplayName,
			ModelID:     row.ModelID,
			GroupName:   row.GroupName,
			Disabled:    row.Disabled,
			FastMode:    row.FastMode,
			AccountIDs:  make([]string, 0, found),
		}
		for _, id := range accountIDs {
			if id != "" {
				model.AccountIDs = append(model.AccountIDs, id)
				counts[id]++
			}
		}
		state.Models = append(state.Models, model)
	}
	for _, account := range accounts {
		state.Accounts = append(state.Accounts, ClaudeAccountEntry{Account: account, ModelCount: counts[account.ID]})
	}
	return state, nil
}

// updateClaudeAccountRows loads the model rows, lets mutate change them, and
// saves the result when mutate reports a change. mutate returns the new rows
// and two counts, which are passed through; the rows are saved when either
// count is above zero.
//
// It takes the config lock. It runs inside callbacks of the account manager,
// so it must not call the manager.
func (s *ProxyService) updateClaudeAccountRows(mutate func([]serverconfig.ModelAdapterConfig) ([]serverconfig.ModelAdapterConfig, int, int)) (int, int, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	cfg, err := s.LoadUserConfig()
	if err != nil {
		return 0, 0, errors.New("config_save_failed: load config: " + err.Error())
	}
	next, first, second := mutate(append([]serverconfig.ModelAdapterConfig(nil), cfg.ModelAdapters...))
	if first == 0 && second == 0 {
		return 0, 0, nil
	}
	cfg.ModelAdapters = next
	if err := s.persistSyncedModelAdaptersLocked(cfg); err != nil {
		return 0, 0, err
	}
	return first, second, nil
}

// overviewClaudeAccounts is the Claude subscription tab's counter in the
// Control Center: how many accounts are saved, and a warning when one of them
// has a token that no longer works.
func (s *ProxyService) overviewClaudeAccounts() (state string, count int, warningCode string) {
	if s == nil || s.claudeAccount == nil {
		return "error", 0, codeClaudeServiceUnavailable
	}
	accounts, err := s.claudeAccount.List()
	if err != nil {
		return "error", 0, claudeaccount.CodeStoreFailed
	}
	if len(accounts) == 0 {
		return "empty", 0, ""
	}
	for _, account := range accounts {
		if account.Status != claudeaccount.StatusOK {
			return "warning", len(accounts), claudeaccount.CodeTokenRejected
		}
	}
	return "ready", len(accounts), ""
}

// adapterKeys returns the keys of a model row: its primary key, then its key
// pool.
func adapterKeys(row serverconfig.ModelAdapterConfig) []string {
	keys := make([]string, 0, 1+len(row.APIKeys))
	if key := strings.TrimSpace(row.APIKey); key != "" {
		keys = append(keys, key)
	}
	for _, key := range row.APIKeys {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// setAdapterKeys gives a model row keys: the first is its primary key and the
// rest its key pool. Repeated keys are dropped.
func setAdapterKeys(row *serverconfig.ModelAdapterConfig, keys []string) {
	seen := make(map[string]struct{}, len(keys))
	unique := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, repeated := seen[key]; repeated || key == "" {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	row.APIKey = ""
	row.APIKeys = nil
	if len(unique) > 0 {
		row.APIKey = unique[0]
		row.APIKeys = append([]string(nil), unique[1:]...)
	}
}

func claudeTokenSet(tokens []claudeaccount.AccountToken) map[string]struct{} {
	set := make(map[string]struct{}, len(tokens))
	for _, item := range tokens {
		set[item.Token] = struct{}{}
	}
	return set
}

// holdsAnyKey reports whether one of the row's keys is in keys.
func holdsAnyKey(row serverconfig.ModelAdapterConfig, keys map[string]struct{}) bool {
	if isCursorAccountModelAdapter(row) {
		return false
	}
	for _, key := range adapterKeys(row) {
		if _, ok := keys[key]; ok {
			return true
		}
	}
	return false
}

// claudeServedModelIDs returns the lower-cased ids of the models that rows
// holding one of tokens serve.
func claudeServedModelIDs(rows []serverconfig.ModelAdapterConfig, tokens map[string]struct{}) map[string]struct{} {
	served := make(map[string]struct{})
	for _, row := range rows {
		if holdsAnyKey(row, tokens) {
			served[strings.ToLower(strings.TrimSpace(row.ModelID))] = struct{}{}
		}
	}
	return served
}

// joinClaudePool adds token to the key pool of every row that holds one of
// poolTokens, and returns the rows and how many it changed.
func joinClaudePool(rows []serverconfig.ModelAdapterConfig, poolTokens map[string]struct{}, token string) ([]serverconfig.ModelAdapterConfig, int) {
	changed := 0
	for index := range rows {
		if !holdsAnyKey(rows[index], poolTokens) {
			continue
		}
		keys := adapterKeys(rows[index])
		if containsString(keys, token) {
			continue
		}
		setAdapterKeys(&rows[index], append(keys, token))
		changed++
	}
	return rows, changed
}

// replaceClaudeToken puts token in the place of previous in every row that
// holds previous, and returns the rows and how many it changed.
func replaceClaudeToken(rows []serverconfig.ModelAdapterConfig, previous string, token string) ([]serverconfig.ModelAdapterConfig, int) {
	changed := 0
	for index := range rows {
		if isCursorAccountModelAdapter(rows[index]) {
			continue
		}
		keys := adapterKeys(rows[index])
		if !containsString(keys, previous) {
			continue
		}
		for position, key := range keys {
			if key == previous {
				keys[position] = token
			}
		}
		setAdapterKeys(&rows[index], keys)
		changed++
	}
	return rows, changed
}

// removeClaudeToken takes token out of every row that holds it. A row that
// has other keys keeps them, with the first as its primary key; a row left
// without a key is dropped. It returns the rows, how many it kept with fewer
// keys, and how many it dropped.
func removeClaudeToken(rows []serverconfig.ModelAdapterConfig, token string) ([]serverconfig.ModelAdapterConfig, int, int) {
	kept := make([]serverconfig.ModelAdapterConfig, 0, len(rows))
	updated, removed := 0, 0
	for _, row := range rows {
		keys := adapterKeys(row)
		if isCursorAccountModelAdapter(row) || !containsString(keys, token) {
			kept = append(kept, row)
			continue
		}
		remaining := make([]string, 0, len(keys))
		for _, key := range keys {
			if key != token {
				remaining = append(remaining, key)
			}
		}
		if len(remaining) == 0 {
			removed++
			continue
		}
		setAdapterKeys(&row, remaining)
		kept = append(kept, row)
		updated++
	}
	return kept, updated, removed
}

// newClaudeAccountRow builds the model row for model, served by tokens. The
// config normalizer fills in the rest (protocol, context window, id) on save.
func newClaudeAccountRow(model ClaudeAccountModelOption, tokens []string) serverconfig.ModelAdapterConfig {
	row := serverconfig.ModelAdapterConfig{
		DisplayName:       model.DisplayName,
		GroupName:         claudeAccountGroupName,
		Type:              "anthropic",
		SupplierID:        claudeAccountSupplierID,
		BaseURL:           claudeaccount.DefaultBaseURL,
		TooltipData:       claudeAccountTooltip,
		ModelID:           model.ID,
		AnthropicAuthMode: modelchannel.AnthropicAuthModeOAuth,
		BalanceProfile:    balanceProfileNone,
	}
	setAdapterKeys(&row, tokens)
	return row
}

// normalizeClaudeModelRequest trims the requested models, drops empty and
// repeated ids, and gives a model without a display name its id as one.
func normalizeClaudeModelRequest(models []ClaudeAccountModelOption) []ClaudeAccountModelOption {
	seen := make(map[string]struct{}, len(models))
	normalized := make([]ClaudeAccountModelOption, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" || len(id) > 200 {
			continue
		}
		if _, repeated := seen[strings.ToLower(id)]; repeated {
			continue
		}
		seen[strings.ToLower(id)] = struct{}{}
		displayName := strings.TrimSpace(model.DisplayName)
		if displayName == "" || len(displayName) > 200 {
			displayName = id
		}
		normalized = append(normalized, ClaudeAccountModelOption{ID: id, DisplayName: displayName})
		if len(normalized) == claudeAccountMaxModelsPerRequest {
			break
		}
	}
	return normalized
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
