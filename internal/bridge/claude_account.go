package bridge

import "cursor/internal/client"

type ClaudeAccountsState = client.ClaudeAccountsState
type ClaudeAccountEntry = client.ClaudeAccountEntry
type ClaudeAccountModel = client.ClaudeAccountModel
type ClaudeAccountImportResult = client.ClaudeAccountImportResult
type ClaudeAccountRemoveResult = client.ClaudeAccountRemoveResult
type ClaudeAccountModelOption = client.ClaudeAccountModelOption
type ClaudeAccountModelCatalog = client.ClaudeAccountModelCatalog
type ClaudeAccountAddModelsRequest = client.ClaudeAccountAddModelsRequest
type ClaudeAccountAddModelsResult = client.ClaudeAccountAddModelsResult
type ClaudeAccountRemoveModelsRequest = client.ClaudeAccountRemoveModelsRequest
type ClaudeAccountRemoveModelsResult = client.ClaudeAccountRemoveModelsResult

// ListClaudeAccounts returns the saved Claude subscription accounts and the
// models they serve. Nothing in it is a token. It does not contact Anthropic.
func (s *ProxyService) ListClaudeAccounts() (ClaudeAccountsState, error) {
	return s.core.ListClaudeAccounts()
}

// ImportClaudeAccount saves the credential the user submitted: the contents of
// a Claude CLI credentials file, or an access token. The app never looks for
// such a file itself. A credential of a saved account replaces that account's
// token; a credential of another account adds an account.
func (s *ProxyService) ImportClaudeAccount(credentials string) (ClaudeAccountImportResult, error) {
	return s.core.ImportClaudeAccount(credentials)
}

// RefreshClaudeAccount reads one account's profile and usage from Anthropic
// again and returns the section's state.
func (s *ProxyService) RefreshClaudeAccount(accountID string) (ClaudeAccountsState, error) {
	return s.core.RefreshClaudeAccount(accountID)
}

// RemoveClaudeAccount deletes one saved account and takes its token out of the
// model rows.
func (s *ProxyService) RemoveClaudeAccount(accountID string) (ClaudeAccountRemoveResult, error) {
	return s.core.RemoveClaudeAccount(accountID)
}

// FetchClaudeAccountModels lists the models the saved accounts can use.
func (s *ProxyService) FetchClaudeAccountModels() (ClaudeAccountModelCatalog, error) {
	return s.core.FetchClaudeAccountModels()
}

// AddClaudeAccountModels creates model rows, served by all saved accounts, for
// the requested models that have none yet.
func (s *ProxyService) AddClaudeAccountModels(req ClaudeAccountAddModelsRequest) (ClaudeAccountAddModelsResult, error) {
	return s.core.AddClaudeAccountModels(req)
}

// RemoveClaudeAccountModels deletes model rows the saved accounts serve.
func (s *ProxyService) RemoveClaudeAccountModels(req ClaudeAccountRemoveModelsRequest) (ClaudeAccountRemoveModelsResult, error) {
	return s.core.RemoveClaudeAccountModels(req)
}

// SetClaudeAccountModelFastMode turns Fast mode on or off for one model row
// the saved accounts serve, and returns the section's state.
func (s *ProxyService) SetClaudeAccountModelFastMode(modelRowID string, enabled bool) (ClaudeAccountsState, error) {
	return s.core.SetClaudeAccountModelFastMode(modelRowID, enabled)
}
