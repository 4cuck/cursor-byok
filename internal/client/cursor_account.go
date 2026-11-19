package client

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"cursor/internal/appdata"
	"cursor/internal/controlcenter"
	"cursor/internal/cursoraccount"
)

type CursorAccountStatus = cursoraccount.Status

func (s *ProxyService) GetCursorAccountStatus() CursorAccountStatus {
	if s == nil || s.cursorAccount == nil {
		return CursorAccountStatus{State: cursoraccount.StateSignedOut}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.cursorAccount.EnsureEmail(ctx)
	return s.cursorAccount.Status()
}

func (s *ProxyService) StartCursorAccountLogin() (CursorAccountStatus, error) {
	if s == nil || s.cursorAccount == nil {
		return CursorAccountStatus{State: cursoraccount.StateError}, fmt.Errorf("Cursor account service is not initialized")
	}
	return s.cursorAccount.StartLogin()
}

func (s *ProxyService) DisconnectCursorAccount() (CursorAccountStatus, error) {
	if s == nil || s.cursorAccount == nil {
		return CursorAccountStatus{State: cursoraccount.StateSignedOut}, nil
	}
	return s.cursorAccount.Disconnect()
}

func (s *ProxyService) ListCursorAccounts() ([]cursoraccount.CursorAccountSummary, error) {
	if s == nil || s.cursorAccount == nil {
		return []cursoraccount.CursorAccountSummary{}, fmt.Errorf("Cursor account service is not initialized")
	}
	return s.cursorAccount.ListAccounts()
}

func (s *ProxyService) ImportCursorAccount(req cursoraccount.CursorAccountImportRequest) (cursoraccount.CursorAccountSummary, error) {
	if s == nil || s.cursorAccount == nil {
		return cursoraccount.CursorAccountSummary{}, fmt.Errorf("Cursor account service is not initialized")
	}
	var (
		summary cursoraccount.CursorAccountSummary
		err     error
	)
	switch strings.TrimSpace(req.Mode) {
	case "local_cursor":
		summary, err = s.cursorAccount.ImportFromLocal()
	case "token":
		if strings.TrimSpace(req.Token) == "" {
			return cursoraccount.CursorAccountSummary{}, fmt.Errorf("import token is empty")
		}
		if len(req.Token) > 8*1024 {
			return cursoraccount.CursorAccountSummary{}, fmt.Errorf("import token exceeds 8 kib")
		}
		summary, err = s.cursorAccount.ImportToken(context.Background(), req.Token)
	case "recovery_json":
		if strings.TrimSpace(req.JSONContent) == "" {
			return cursoraccount.CursorAccountSummary{}, fmt.Errorf("import json is empty")
		}
		if len(req.JSONContent) > 1<<20 {
			return cursoraccount.CursorAccountSummary{}, fmt.Errorf("import json exceeds 1 mib")
		}
		var summaries []cursoraccount.CursorAccountSummary
		summaries, err = s.cursorAccount.ImportJSON(req.JSONContent)
		if err != nil {
			return cursoraccount.CursorAccountSummary{}, err
		}
		if len(summaries) == 0 {
			return cursoraccount.CursorAccountSummary{}, fmt.Errorf("import json is empty")
		}
		summary = summaries[0]
	default:
		return cursoraccount.CursorAccountSummary{}, fmt.Errorf("unsupported import mode")
	}
	if err == nil {
		s.triggerProviderBalanceSyncAfterAccountChange()
	}
	return summary, err
}

func (s *ProxyService) PrepareCursorAccountRecoveryExport(req cursoraccount.CursorAccountRecoveryExportRequest) (controlcenter.PreparedOperation, error) {
	if s == nil || s.cursorAccount == nil {
		return controlcenter.PreparedOperation{}, fmt.Errorf("Cursor account service is not initialized")
	}
	for _, id := range req.AccountIDs {
		if strings.TrimSpace(id) == "" {
			return controlcenter.PreparedOperation{}, fmt.Errorf("account id is empty")
		}
	}
	return s.cursorAccount.PrepareRecoveryExport(req)
}

func (s *ProxyService) ExecuteCursorAccountRecoveryExport(confirmationToken, destinationPath string) (cursoraccount.CursorAccountRecoveryExportResult, error) {
	if s == nil || s.cursorAccount == nil {
		return cursoraccount.CursorAccountRecoveryExportResult{}, fmt.Errorf("Cursor account service is not initialized")
	}
	dest := strings.TrimSpace(destinationPath)
	if dest == "" {
		dest = filepath.Join(appdata.DataRootPath(), "cursor-account-recovery.json")
	}
	return s.cursorAccount.ExecuteRecoveryExport(confirmationToken, dest)
}

func (s *ProxyService) SetCurrentCursorAccount(accountID string) (cursoraccount.CursorAccountSummary, error) {
	if s == nil || s.cursorAccount == nil {
		return cursoraccount.CursorAccountSummary{}, fmt.Errorf("Cursor account service is not initialized")
	}
	if strings.TrimSpace(accountID) == "" {
		return cursoraccount.CursorAccountSummary{}, fmt.Errorf("account id is empty")
	}
	summary, err := s.cursorAccount.SetCurrent(accountID)
	if err != nil {
		return summary, err
	}
	// Point the single cursor_account model list at this account. Duplicate
	// per-account copies from an older "Sync all" are folded in at the same time.
	s.collapseSharedCursorAccountModels(summary.ID, summary.Email)
	s.triggerProviderBalanceSyncAfterAccountChange()
	return summary, nil
}

func (s *ProxyService) UpdateCursorAccountTags(accountID string, tags []string) (cursoraccount.CursorAccountSummary, error) {
	if s == nil || s.cursorAccount == nil {
		return cursoraccount.CursorAccountSummary{}, fmt.Errorf("Cursor account service is not initialized")
	}
	if strings.TrimSpace(accountID) == "" {
		return cursoraccount.CursorAccountSummary{}, fmt.Errorf("account id is empty")
	}
	return s.cursorAccount.UpdateTags(accountID, tags)
}

// ClearCursorAccountLock ends a device-limit cooldown early. This backs the
// explicit "try anyway" action in the UI.
func (s *ProxyService) ClearCursorAccountLock(accountID string) (cursoraccount.CursorAccountSummary, error) {
	if s == nil || s.cursorAccount == nil {
		return cursoraccount.CursorAccountSummary{}, fmt.Errorf("Cursor account service is not initialized")
	}
	if strings.TrimSpace(accountID) == "" {
		return cursoraccount.CursorAccountSummary{}, fmt.Errorf("account id is empty")
	}
	return s.cursorAccount.ClearAccountLockByID(accountID)
}

func (s *ProxyService) DeleteCursorAccounts(req cursoraccount.CursorAccountDeleteRequest) error {
	if s == nil || s.cursorAccount == nil {
		return fmt.Errorf("Cursor account service is not initialized")
	}
	for _, id := range req.AccountIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("account id is empty")
		}
	}
	if err := s.cursorAccount.Delete(req); err != nil {
		return err
	}
	s.reconcileCursorAccountAdaptersAfterDelete(req.AccountIDs, req.ReplacementID)
	s.triggerProviderBalanceSyncAfterAccountChange()
	return nil
}

func (s *ProxyService) PrepareCursorClientAccountSwitch(accountID string) (cursoraccount.CursorAccountSwitchPreparation, error) {
	return s.PrepareCursorClientAccountSwitchWithOptions(accountID, cursoraccount.CursorAccountSwitchOptions{})
}

func (s *ProxyService) PrepareCursorClientAccountSwitchWithOptions(accountID string, options cursoraccount.CursorAccountSwitchOptions) (cursoraccount.CursorAccountSwitchPreparation, error) {
	if s == nil || s.cursorAccount == nil {
		return cursoraccount.CursorAccountSwitchPreparation{}, fmt.Errorf("Cursor account service is not initialized")
	}
	if strings.TrimSpace(accountID) == "" {
		return cursoraccount.CursorAccountSwitchPreparation{}, fmt.Errorf("account id is empty")
	}
	return s.cursorAccount.PrepareCursorClientAccountSwitchWithOptions(accountID, options)
}

func (s *ProxyService) ExecuteCursorClientAccountSwitch(confirmationToken string) (cursoraccount.CursorAccountSwitchResult, error) {
	if s == nil || s.cursorAccount == nil {
		return cursoraccount.CursorAccountSwitchResult{}, fmt.Errorf("Cursor account service is not initialized")
	}
	result, err := s.cursorAccount.ExecuteCursorClientAccountSwitch(confirmationToken)
	if err == nil && result.State == "succeeded" {
		s.triggerProviderBalanceSyncAfterAccountChange()
	}
	return result, err
}

func (s *ProxyService) AttachCursorRuntime(runtime cursoraccount.CursorRuntime) {
	if s == nil || s.cursorAccount == nil || runtime == nil {
		return
	}
	s.cursorAccount.SetCursorRuntime(runtime)
}
