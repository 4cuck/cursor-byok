package bridge

import (
	"cursor/internal/agentops"
	"cursor/internal/client"
	"cursor/internal/configprofile"
	"cursor/internal/controlcenter"
	"cursor/internal/cursoraccount"
	"cursor/internal/requestlab"
	"cursor/internal/routing"
)

type ControlCenterOverview = controlcenter.ControlCenterOverview
type ControlCenterDomainStatus = controlcenter.ControlCenterDomainStatus
type PreparedOperation = controlcenter.PreparedOperation
type OperationResult = controlcenter.OperationResult
type SanitizedExport = controlcenter.SanitizedExport
type CursorAccountSummary = cursoraccount.CursorAccountSummary
type CursorAccountImportRequest = cursoraccount.CursorAccountImportRequest
type CursorAccountRecoveryExportRequest = cursoraccount.CursorAccountRecoveryExportRequest
type CursorAccountRecoveryExportResult = cursoraccount.CursorAccountRecoveryExportResult
type CursorAccountDeleteRequest = cursoraccount.CursorAccountDeleteRequest
type CursorAccountLoginSession = cursoraccount.CursorAccountLoginSession
type CursorAccountLoginStatus = cursoraccount.CursorAccountLoginStatus
type CursorAccountSwitchPreparation = cursoraccount.CursorAccountSwitchPreparation
type CursorAccountSwitchResult = cursoraccount.CursorAccountSwitchResult
type CursorAccountSwitchOptions = cursoraccount.CursorAccountSwitchOptions
type CursorAccountModel = client.CursorAccountModel
type CursorAccountModelCatalog = client.CursorAccountModelCatalog
type CursorAccountModelSyncRequest = client.CursorAccountModelSyncRequest
type CursorAccountModelSyncResult = client.CursorAccountModelSyncResult
type CursorAccountHealth = client.CursorAccountHealth
type CursorAccountPlan = client.CursorAccountPlan
type CursorAccountSpendLimit = client.CursorAccountSpendLimit
type CursorAccountSpendLimitRequest = client.CursorAccountSpendLimitRequest
type CursorAccountApiKey = client.CursorAccountApiKey
type CursorAccountApiKeyList = client.CursorAccountApiKeyList
type CreateCursorAccountApiKeyRequest = client.CreateCursorAccountApiKeyRequest
type CreateCursorAccountApiKeyResult = client.CreateCursorAccountApiKeyResult
type RequestSourceQuery = requestlab.RequestSourceQuery
type RequestSourcePage = requestlab.RequestSourcePage
type RequestComparisonRequest = requestlab.RequestComparisonRequest
type RequestComparison = requestlab.RequestComparison
type RoutingPolicy = routing.Policy
type RoutingPreviewRequest = routing.PreviewRequest
type RoutingDecisionPreview = routing.DecisionPreview
type RoutingDecisionQuery = routing.DecisionQuery
type RoutingDecisionPage = routing.DecisionPage
type AgentRunQuery = agentops.RunQuery
type AgentRunPage = agentops.RunPage
type AgentRunDetail = agentops.RunDetail
type AgentRetryPreparation = agentops.RetryPreparation
type AgentRetryResult = agentops.RetryResult
type ConfigProfileSummary = configprofile.Summary
type SaveConfigProfileRequest = configprofile.SaveRequest
type ConfigProfilePreview = configprofile.Preview
type ConfigProfileApplyPreparation = configprofile.ApplyPreparation

func WireCursorRuntime(service *ProxyService, windows *WindowService) {
	if service == nil || service.core == nil {
		return
	}
	service.core.AttachCursorRuntime(newCursorProcessRuntime(windows))
}

func (s *ProxyService) GetControlCenterOverview() (ControlCenterOverview, error) {
	return s.core.GetControlCenterOverview()
}

func (s *ProxyService) ListCursorAccounts() ([]CursorAccountSummary, error) {
	return s.core.ListCursorAccounts()
}

func (s *ProxyService) ImportCursorAccount(req CursorAccountImportRequest) (CursorAccountSummary, error) {
	return s.core.ImportCursorAccount(req)
}

func (s *ProxyService) PrepareCursorAccountRecoveryExport(req CursorAccountRecoveryExportRequest) (PreparedOperation, error) {
	return s.core.PrepareCursorAccountRecoveryExport(req)
}

func (s *ProxyService) ExecuteCursorAccountRecoveryExport(confirmationToken, destinationPath string) (CursorAccountRecoveryExportResult, error) {
	return s.core.ExecuteCursorAccountRecoveryExport(confirmationToken, destinationPath)
}

func (s *ProxyService) SetCurrentCursorAccount(accountID string) (CursorAccountSummary, error) {
	return s.core.SetCurrentCursorAccount(accountID)
}

func (s *ProxyService) UpdateCursorAccountTags(accountID string, tags []string) (CursorAccountSummary, error) {
	return s.core.UpdateCursorAccountTags(accountID, tags)
}

// ClearCursorAccountLock ends a device-limit cooldown early ("try anyway").
func (s *ProxyService) ClearCursorAccountLock(accountID string) (CursorAccountSummary, error) {
	return s.core.ClearCursorAccountLock(accountID)
}

// CheckCursorAccountHealth probes one saved account end to end and reports
// whether it can actually serve model calls. An empty accountID probes the
// current account. The card must pass the clicked account so only that row
// is marked as checking.
func (s *ProxyService) CheckCursorAccountHealth(accountID string) (CursorAccountHealth, error) {
	return s.core.CheckCursorAccountHealth(accountID)
}

// GetCursorAccountPlan reads the plan name and how much included usage is left
// for one saved account. An empty accountID uses the current account.
func (s *ProxyService) GetCursorAccountPlan(accountID string) (CursorAccountPlan, error) {
	return s.core.GetCursorAccountPlan(accountID)
}

// GetCursorAccountSpendLimit reads the current on-demand spend limit for one
// saved Cursor account. An empty accountID uses the current account.
func (s *ProxyService) GetCursorAccountSpendLimit(accountID string) (CursorAccountSpendLimit, error) {
	return s.core.GetCursorAccountSpendLimit(accountID)
}

// SetCursorAccountSpendLimit updates the on-demand spend limit for one saved
// Cursor account to disabled ($0), a fixed dollar cap, or unlimited.
func (s *ProxyService) SetCursorAccountSpendLimit(req CursorAccountSpendLimitRequest) (CursorAccountSpendLimit, error) {
	return s.core.SetCursorAccountSpendLimit(req)
}

// ListCursorAccountApiKeys reads the personal user API keys (crsr_/grok_) on one
// saved Cursor account. An empty accountID uses the current account.
func (s *ProxyService) ListCursorAccountApiKeys(accountID string) (CursorAccountApiKeyList, error) {
	return s.core.ListCursorAccountApiKeys(accountID)
}

// CreateCursorAccountApiKey mints one user API key on a saved Cursor account.
// It only runs on an explicit user request (never automatically) and returns the
// full secret once. The name is either caller-supplied or a privacy-safe random
// one that mimics the account's existing keys.
func (s *ProxyService) CreateCursorAccountApiKey(req CreateCursorAccountApiKeyRequest) (CreateCursorAccountApiKeyResult, error) {
	return s.core.CreateCursorAccountApiKey(req)
}

func (s *ProxyService) DeleteCursorAccounts(req CursorAccountDeleteRequest) error {
	return s.core.DeleteCursorAccounts(req)
}

func (s *ProxyService) PrepareCursorClientAccountSwitch(accountID string) (CursorAccountSwitchPreparation, error) {
	return s.core.PrepareCursorClientAccountSwitch(accountID)
}

func (s *ProxyService) PrepareCursorClientAccountSwitchWithOptions(accountID string, options CursorAccountSwitchOptions) (CursorAccountSwitchPreparation, error) {
	return s.core.PrepareCursorClientAccountSwitchWithOptions(accountID, options)
}

// FetchCursorAccountModels lists the models one saved Cursor account may use,
// so an imported API-key account can be materialized as BYOK model adapters.
// An empty accountID queries the current account.
func (s *ProxyService) FetchCursorAccountModels(accountID string) (CursorAccountModelCatalog, error) {
	return s.core.FetchCursorAccountModels(accountID)
}

// SyncCursorAccountModels fetches and persists Cursor account models atomically
// on the backend without sending large config payloads through WebView2 IPC.
func (s *ProxyService) SyncCursorAccountModels(req CursorAccountModelSyncRequest) (CursorAccountModelSyncResult, error) {
	return s.core.SyncCursorAccountModels(req)
}

// ClearCursorAccountQuotaCooldown clears active per-(account, model) quota
// cooldowns for an account (or all accounts when accountID is empty).
func (s *ProxyService) ClearCursorAccountQuotaCooldown(accountID string) (int, error) {
	return s.core.ClearCursorAccountQuotaCooldown(accountID)
}

func (s *ProxyService) ExecuteCursorClientAccountSwitch(confirmationToken string) (CursorAccountSwitchResult, error) {
	return s.core.ExecuteCursorClientAccountSwitch(confirmationToken)
}

func (s *ProxyService) BeginCursorAccountLogin() (CursorAccountLoginSession, error) {
	return s.core.BeginCursorAccountLogin()
}

func (s *ProxyService) GetCursorAccountLoginStatus(sessionID string) (CursorAccountLoginStatus, error) {
	return s.core.GetCursorAccountLoginStatus(sessionID)
}

func (s *ProxyService) CancelCursorAccountLogin(sessionID string) (OperationResult, error) {
	return s.core.CancelCursorAccountLogin(sessionID)
}

func (s *ProxyService) ListRequestSources(query RequestSourceQuery) (RequestSourcePage, error) {
	return s.core.ListRequestSources(query)
}

func (s *ProxyService) BuildRequestComparison(request RequestComparisonRequest) (RequestComparison, error) {
	return s.core.BuildRequestComparison(request)
}

func (s *ProxyService) ExportSanitizedRequestComparison(comparisonID string) (SanitizedExport, error) {
	return s.core.ExportSanitizedRequestComparison(comparisonID)
}

func (s *ProxyService) GetRoutingPolicy() (RoutingPolicy, error) {
	return s.core.GetRoutingPolicy()
}

func (s *ProxyService) SaveRoutingPolicy(policy RoutingPolicy) (RoutingPolicy, error) {
	return s.core.SaveRoutingPolicy(policy)
}

func (s *ProxyService) PreviewRoutingDecision(request RoutingPreviewRequest) (RoutingDecisionPreview, error) {
	return s.core.PreviewRoutingDecision(request)
}

func (s *ProxyService) GetRoutingDecisionHistory(query RoutingDecisionQuery) (RoutingDecisionPage, error) {
	return s.core.GetRoutingDecisionHistory(query)
}

func (s *ProxyService) GetAgentRuns(query AgentRunQuery) (AgentRunPage, error) {
	return s.core.GetAgentRuns(query)
}

func (s *ProxyService) GetAgentRun(runID string) (AgentRunDetail, error) {
	return s.core.GetAgentRun(runID)
}

func (s *ProxyService) CancelAgentRun(runID string) (OperationResult, error) {
	return s.core.CancelAgentRun(runID)
}

func (s *ProxyService) PrepareAgentRunRetry(runID string) (AgentRetryPreparation, error) {
	return s.core.PrepareAgentRunRetry(runID)
}

func (s *ProxyService) ExecuteAgentRunRetry(confirmationToken string) (AgentRetryResult, error) {
	return s.core.ExecuteAgentRunRetry(confirmationToken)
}

func (s *ProxyService) ExportSanitizedAgentRunReport(runID string) (SanitizedExport, error) {
	return s.core.ExportSanitizedAgentRunReport(runID)
}

func (s *ProxyService) ListConfigProfiles() ([]ConfigProfileSummary, error) {
	return s.core.ListConfigProfiles()
}

func (s *ProxyService) SaveCurrentConfigProfile(request SaveConfigProfileRequest) (ConfigProfileSummary, error) {
	return s.core.SaveCurrentConfigProfile(request)
}

func (s *ProxyService) DeleteConfigProfile(profileID string) (OperationResult, error) {
	return s.core.DeleteConfigProfile(profileID)
}

func (s *ProxyService) PreviewConfigProfile(profileID string) (ConfigProfilePreview, error) {
	return s.core.PreviewConfigProfile(profileID)
}

func (s *ProxyService) PrepareConfigProfileApply(profileID string) (ConfigProfileApplyPreparation, error) {
	return s.core.PrepareConfigProfileApply(profileID)
}

func (s *ProxyService) ExecuteConfigProfileApply(confirmationToken string) (OperationResult, error) {
	return s.core.ExecuteConfigProfileApply(confirmationToken)
}

func (s *ProxyService) ExportConfigProfile(profileID string) (SanitizedExport, error) {
	return s.core.ExportConfigProfile(profileID)
}

func (s *ProxyService) ImportConfigProfile(content string) (ConfigProfilePreview, error) {
	return s.core.ImportConfigProfile(content)
}
