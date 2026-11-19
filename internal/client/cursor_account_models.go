package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"cursor/gen/aiserverv1"
	serverconfig "cursor/internal/backend/server/config"
	"cursor/internal/backend/server/upstream"
	"cursor/internal/cursoraccount"
	"cursor/internal/logger"
	"cursor/internal/modelcontext"
	"cursor/internal/netproxy"
	legacyruntime "cursor/internal/runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	cursorModelsEndpoint     = "https://api2.cursor.sh/aiserver.v1.AiService/AvailableModels"
	cursorModelsMaxRespBytes = 8 << 20
	cursorModelsTimeout      = 20 * time.Second

	CursorSyncModeMerge   = "merge"
	CursorSyncModeReplace = "replace"
	CursorSyncModeAll     = "all"
	CursorSyncModeRemove  = "remove"

	// sharedCursorAccountLabel is the only model-list group for Cursor accounts.
	// Saved accounts stay as credentials; "Set as current" points this one list
	// at the account that should serve it. Quota failover still walks the rest
	// of the saved account pool, so each account does not need its own copy.
	sharedCursorAccountLabel = "cursor_account"
)

// CursorAccountModel is one model exposed by the signed-in Cursor account.
type CursorAccountModel struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	DefaultOn   bool   `json:"defaultOn"`
	// PickerSpec is the IDE's parameter definitions, max-mode flags, context limits,
	// and the context windows Cursor offers for the model.
	PickerSpec string `json:"pickerSpec,omitempty"`
	// ContextWindowTokens is Cursor's default context window for the model.
	ContextWindowTokens int `json:"contextWindowTokens,omitempty"`
}

// CursorAccountModelCatalog is the non-secret result of listing account models.
type CursorAccountModelCatalog struct {
	Models    []CursorAccountModel `json:"models"`
	AccountID string               `json:"accountId,omitempty"`
	Email     string               `json:"email,omitempty"`
}

// CursorAccountModelSyncRequest configures a backend-native Cursor model sync.
// Performing sync and persistence inside Go avoids round-tripping hundreds of
// kilobytes of model specs through WebView2's IPC request body stream.
type CursorAccountModelSyncRequest struct {
	// AccountID selects which saved Cursor account to sync; empty means current.
	AccountID string `json:"accountId,omitempty"`
	// Mode is "merge" (refresh the shared cursor_account list), "replace" (same
	// list, owned by this account), "all" (refresh that list from the current
	// account), or "remove".
	Mode string `json:"mode,omitempty"`
	// AdapterIDs, with Mode "remove", deletes only those cursor_account adapters.
	AdapterIDs []string `json:"adapterIds,omitempty"`
	// GroupName, with Mode "remove", deletes one cursor_account supplier group.
	GroupName string `json:"groupName,omitempty"`
}

// CursorAccountModelSyncResult reports the outcome of a backend-native sync.
type CursorAccountModelSyncResult struct {
	AccountID      string `json:"accountId,omitempty"`
	Email          string `json:"email,omitempty"`
	OwnerLabel     string `json:"ownerLabel,omitempty"`
	Total          int    `json:"total"`
	Added          int    `json:"added"`
	Updated        int    `json:"updated"`
	Removed        int    `json:"removed"`
	AccountsSynced int    `json:"accountsSynced,omitempty"`
	Unchanged      bool   `json:"unchanged"`
}

// FetchCursorAccountModels lists the models a saved Cursor account may use.
// An empty accountID queries the current account.
func (s *ProxyService) FetchCursorAccountModels(accountID string) (CursorAccountModelCatalog, error) {
	if s == nil || s.cursorAccount == nil {
		return CursorAccountModelCatalog{}, errors.New("cursor_account_service_unavailable: Cursor account service is not initialized")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" && !s.cursorAccount.SignedIn() {
		return CursorAccountModelCatalog{}, errors.New("cursor_account_sign_in_required: sign in to a Cursor account first")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cursorModelsTimeout)
	defer cancel()

	return s.fetchCursorAccountCatalogWithContext(ctx, accountID)
}

func (s *ProxyService) fetchCursorAccountCatalogWithContext(ctx context.Context, accountID string) (CursorAccountModelCatalog, error) {
	authorization, resolvedID, email, err := s.cursorAccountAuthorizationForCatalog(ctx, accountID)
	if err != nil {
		return CursorAccountModelCatalog{}, err
	}

	parsed, err := requestCursorModelCatalog(ctx, authorization)
	if err != nil {
		// Re-exchange or refresh once when Cursor rejects a cached short-lived JWT.
		if catalogAuthRejected(err) {
			if renewed, refreshErr := s.cursorAccount.ForceRefresh(ctx, resolvedID); refreshErr == nil {
				logger.Infof("cursor model catalog retrying after credential refresh account=%s", resolvedID)
				if retried, retryErr := requestCursorModelCatalog(ctx, renewed); retryErr == nil {
					parsed = retried
					err = nil
				} else {
					err = retryErr
				}
			}
		}
		if err != nil {
			logger.Errorf("cursor model catalog failed account=%s err=%v", resolvedID, err)
			return CursorAccountModelCatalog{}, fmt.Errorf("cursor_model_catalog_failed: %w", err)
		}
	}

	catalog := CursorAccountModelCatalog{
		Models:    collapseCursorCatalogModels(parsed.GetModels()),
		AccountID: resolvedID,
		Email:     email,
	}
	sort.Slice(catalog.Models, func(i, j int) bool {
		return catalog.Models[i].Name < catalog.Models[j].Name
	})
	logger.Infof("cursor model catalog resolved account=%s models=%d", resolvedID, len(catalog.Models))
	return catalog, nil
}

// SyncCursorAccountModels fetches the upstream Cursor model catalog and updates
// UserConfig.ModelAdapters atomically in Go without sending large config bodies
// across the WebView2 runtime bridge.
func (s *ProxyService) SyncCursorAccountModels(req CursorAccountModelSyncRequest) (CursorAccountModelSyncResult, error) {
	if s == nil || s.cursorAccount == nil {
		return CursorAccountModelSyncResult{}, errors.New("cursor_account_service_unavailable: Cursor account service is not initialized")
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = CursorSyncModeMerge
	}
	accountID := strings.TrimSpace(req.AccountID)

	accounts, err := s.cursorAccount.ListAccounts()
	if err != nil {
		return CursorAccountModelSyncResult{}, fmt.Errorf("cursor_account_service_unavailable: %w", err)
	}
	knownAccounts := make(map[string]cursoraccount.CursorAccountSummary, len(accounts))
	var currentSummary *cursoraccount.CursorAccountSummary
	for i := range accounts {
		item := accounts[i]
		knownAccounts[item.ID] = item
		if item.IsCurrent {
			currentSummary = &accounts[i]
		}
	}

	if mode == CursorSyncModeRemove {
		return s.removeCursorAccountAdapters(accountID, strings.TrimSpace(req.GroupName), req.AdapterIDs, knownAccounts)
	}

	// "Sync all" used to copy the full catalog onto every account. The model
	// list is shared, so this refreshes that one list from the current account.
	if mode == CursorSyncModeAll {
		if currentSummary != nil {
			accountID = currentSummary.ID
		} else if accountID == "" && len(accounts) > 0 {
			accountID = accounts[0].ID
		}
		mode = CursorSyncModeMerge
	}

	if accountID == "" {
		if currentSummary != nil {
			accountID = currentSummary.ID
		} else if len(accounts) == 1 {
			accountID = accounts[0].ID
		}
	}
	if accountID == "" {
		return CursorAccountModelSyncResult{}, errors.New("cursor_account_sign_in_required: select or import a Cursor account before syncing models")
	}
	summary, exists := knownAccounts[accountID]
	if !exists {
		return CursorAccountModelSyncResult{}, errors.New("account_not_found: Cursor account not found")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cursorModelsTimeout)
	defer cancel()

	catalog, err := s.fetchCursorAccountCatalogWithContext(ctx, accountID)
	if err != nil {
		return CursorAccountModelSyncResult{}, err
	}
	if len(catalog.Models) == 0 {
		return CursorAccountModelSyncResult{}, errors.New("cursor_model_catalog_failed: Cursor returned an empty model list for this account")
	}
	email := strings.TrimSpace(summary.Email)
	if email == "" {
		email = strings.TrimSpace(catalog.Email)
	}
	ownerID := accountID
	ownerEmail := email
	// The shared list follows the current account. Syncing some other account
	// only refreshes model names and specs. "Set as current" is what switches
	// which account serves the list. Replace mode is the explicit exception:
	// that account becomes the owner immediately.
	if mode != CursorSyncModeReplace && currentSummary != nil && strings.TrimSpace(currentSummary.ID) != "" {
		ownerID = currentSummary.ID
		if strings.TrimSpace(currentSummary.Email) != "" {
			ownerEmail = strings.TrimSpace(currentSummary.Email)
		}
	}

	s.configMu.Lock()
	defer s.configMu.Unlock()

	cfg, err := s.LoadUserConfig()
	if err != nil {
		return CursorAccountModelSyncResult{}, fmt.Errorf("config_save_failed: load config: %w", err)
	}
	nextAdapters, ownerLabel, added, updated, removed := mergeSyncedCursorAccountAdapters(
		cfg.ModelAdapters,
		catalog.Models,
		ownerID,
		ownerEmail,
	)

	if added == 0 && updated == 0 && removed == 0 {
		return CursorAccountModelSyncResult{
			AccountID:      accountID,
			Email:          email,
			OwnerLabel:     ownerLabel,
			Total:          len(catalog.Models),
			AccountsSynced: 1,
			Unchanged:      true,
		}, nil
	}

	cfg.ModelAdapters = nextAdapters
	if err := s.persistSyncedModelAdaptersLocked(cfg); err != nil {
		return CursorAccountModelSyncResult{}, err
	}
	return CursorAccountModelSyncResult{
		AccountID:      accountID,
		Email:          email,
		OwnerLabel:     ownerLabel,
		Total:          len(catalog.Models),
		Added:          added,
		Updated:        updated,
		Removed:        removed,
		AccountsSynced: 1,
		Unchanged:      false,
	}, nil
}

func (s *ProxyService) removeCursorAccountAdapters(
	accountID string,
	groupName string,
	adapterIDs []string,
	knownAccounts map[string]cursoraccount.CursorAccountSummary,
) (CursorAccountModelSyncResult, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()

	cfg, err := s.LoadUserConfig()
	if err != nil {
		return CursorAccountModelSyncResult{}, fmt.Errorf("config_save_failed: load config: %w", err)
	}
	kept, removed := filterCursorAccountAdapters(cfg.ModelAdapters, accountID, groupName, adapterIDs, knownAccounts)
	if removed == 0 {
		return CursorAccountModelSyncResult{AccountID: accountID, OwnerLabel: sharedCursorAccountLabel, Unchanged: true}, nil
	}
	cfg.ModelAdapters = kept
	if err := s.persistSyncedModelAdaptersLocked(cfg); err != nil {
		return CursorAccountModelSyncResult{}, err
	}
	return CursorAccountModelSyncResult{
		AccountID:  accountID,
		OwnerLabel: sharedCursorAccountLabel,
		Removed:    removed,
		Unchanged:  false,
	}, nil
}

// filterCursorAccountAdapters drops cursor_account adapters without sending the
// remaining config back through the desktop bridge. adapter IDs win, then a
// supplier group name, then the account id ("*" or empty removes every Cursor
// model). Third-party adapters are always kept.
func filterCursorAccountAdapters(
	existing []serverconfig.ModelAdapterConfig,
	accountID string,
	groupName string,
	adapterIDs []string,
	knownAccounts map[string]cursoraccount.CursorAccountSummary,
) (kept []serverconfig.ModelAdapterConfig, removed int) {
	accountID = strings.TrimSpace(accountID)
	groupName = strings.TrimSpace(groupName)
	idSet := make(map[string]struct{}, len(adapterIDs))
	for _, id := range adapterIDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			idSet[trimmed] = struct{}{}
		}
	}
	kept = make([]serverconfig.ModelAdapterConfig, 0, len(existing))
	for _, adapter := range existing {
		if legacyruntime.NormalizeModelSource(adapter.Source) != legacyruntime.ModelSourceCursorAccount {
			kept = append(kept, adapter)
			continue
		}
		owner := strings.TrimSpace(adapter.CursorAccountID)
		drop := false
		switch {
		case len(idSet) > 0:
			_, drop = idSet[strings.TrimSpace(adapter.ID)]
		case groupName != "":
			drop = strings.TrimSpace(adapter.GroupName) == groupName || strings.TrimSpace(adapter.SupplierID) == groupName
		case accountID == "*" || accountID == "":
			drop = true
		case owner == accountID:
			drop = true
		case owner != "" && len(knownAccounts) > 0:
			if _, exists := knownAccounts[owner]; !exists {
				drop = true
			}
		}
		if drop {
			removed++
			continue
		}
		kept = append(kept, adapter)
	}
	return kept, removed
}

func (s *ProxyService) persistSyncedModelAdaptersLocked(cfg UserConfig) error {
	ctx := context.Background()
	if app := application.Get(); app != nil {
		ctx = app.Context()
	}
	var (
		normalized UserConfig
		err        error
	)
	if host := s.currentBackendHost(); host != nil {
		normalized, err = host.SaveConfig(ctx, cfg)
	} else if s.store != nil {
		normalized, err = s.store.Save(ctx, cfg)
	} else {
		return errors.New("config_save_failed: config store is not initialized")
	}
	if err != nil {
		logger.Errorf("cursor model sync config save failed adapters=%d err=%v", len(cfg.ModelAdapters), err)
		return fmt.Errorf("config_save_failed: %w", err)
	}
	s.invalidateModelCatalogCaches()
	s.emitUserConfigChanged(normalized)
	return nil
}

// reconcileCursorAccountAdaptersAfterDelete reassigns cursor_account adapters
// from deleted accounts to a surviving replacement account (keeping adapter IDs
// stable so open Cursor IDE chats continue seamlessly), or removes them if no
// Cursor accounts remain.
func (s *ProxyService) reconcileCursorAccountAdaptersAfterDelete(deletedIDs []string, replacementID string) {
	if s == nil || len(deletedIDs) == 0 {
		return
	}
	deletedSet := make(map[string]struct{}, len(deletedIDs))
	for _, id := range deletedIDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			deletedSet[trimmed] = struct{}{}
		}
	}
	if len(deletedSet) == 0 {
		return
	}

	replacementID = strings.TrimSpace(replacementID)
	replacementEmail := ""
	if s.cursorAccount != nil {
		if replacementID == "" {
			replacementID = strings.TrimSpace(s.cursorAccount.CurrentAccountID())
		}
		if accounts, err := s.cursorAccount.ListAccounts(); err == nil {
			if replacementID == "" && len(accounts) > 0 {
				replacementID = accounts[0].ID
			}
			for _, acc := range accounts {
				if acc.ID == replacementID {
					replacementEmail = strings.TrimSpace(acc.Email)
					break
				}
			}
		}
	}
	if _, deletedReplacement := deletedSet[replacementID]; deletedReplacement {
		replacementID = ""
	}

	s.configMu.Lock()
	defer s.configMu.Unlock()

	cfg, err := s.LoadUserConfig()
	if err != nil || len(cfg.ModelAdapters) == 0 {
		return
	}

	// Collect models already owned by replacementID so reassignment never collides.
	existingReplacementModels := map[string]struct{}{}
	if replacementID != "" {
		for _, adapter := range cfg.ModelAdapters {
			if legacyruntime.NormalizeModelSource(adapter.Source) == legacyruntime.ModelSourceCursorAccount &&
				strings.TrimSpace(adapter.CursorAccountID) == replacementID {
				existingReplacementModels[strings.ToLower(strings.TrimSpace(adapter.ModelID))] = struct{}{}
			}
		}
	}

	changed := false
	next := make([]serverconfig.ModelAdapterConfig, 0, len(cfg.ModelAdapters))
	for _, adapter := range cfg.ModelAdapters {
		if legacyruntime.NormalizeModelSource(adapter.Source) != legacyruntime.ModelSourceCursorAccount {
			next = append(next, adapter)
			continue
		}
		owner := strings.TrimSpace(adapter.CursorAccountID)
		if _, isDeleted := deletedSet[owner]; !isDeleted {
			next = append(next, adapter)
			continue
		}
		changed = true
		modelKey := strings.ToLower(strings.TrimSpace(adapter.ModelID))
		if replacementID == "" {
			continue
		}
		if _, alreadyCovered := existingReplacementModels[modelKey]; alreadyCovered {
			continue
		}
		existingReplacementModels[modelKey] = struct{}{}
		adapter.CursorAccountID = replacementID
		modelName := strings.TrimSpace(adapter.ModelID)
		if modelName != "" {
			adapter.ID = sharedCursorAccountLabel + ":" + modelName
		}
		adapter.GroupName = sharedCursorAccountLabel
		adapter.SupplierID = sharedCursorAccountLabel
		if replacementEmail != "" {
			adapter.TooltipData = replacementEmail + " · " + sharedCursorAccountLabel
		} else {
			adapter.TooltipData = sharedCursorAccountLabel
		}
		next = append(next, adapter)
	}
	if !changed {
		return
	}
	cfg.ModelAdapters = next
	if saveErr := s.persistSyncedModelAdaptersLocked(cfg); saveErr != nil {
		logger.Warnf("reconcile cursor account adapters after delete failed: %v", saveErr)
	}
}

func mergeSyncedCursorAccountAdapters(
	existing []serverconfig.ModelAdapterConfig,
	models []CursorAccountModel,
	accountID string,
	email string,
) (next []serverconfig.ModelAdapterConfig, ownerLabel string, added int, updated int, removed int) {
	accountID = strings.TrimSpace(accountID)
	email = strings.TrimSpace(email)
	ownerLabel = sharedCursorAccountLabel

	// Carry over user-customized per-model fields (chosen context window, effort,
	// fast mode, disabled state, pricing) when updating or reassigning models.
	// Copies under cursor_account1 / cursor_account2 collapse onto the same model.
	previousByModel := make(map[string]serverconfig.ModelAdapterConfig, len(existing))
	for _, item := range existing {
		if legacyruntime.NormalizeModelSource(item.Source) != legacyruntime.ModelSourceCursorAccount {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(item.ModelID))
		if key == "" {
			continue
		}
		if _, has := previousByModel[key]; !has || strings.TrimSpace(item.CursorAccountID) == accountID {
			previousByModel[key] = item
		}
	}

	desired := make([]serverconfig.ModelAdapterConfig, 0, len(models))
	desiredByModel := make(map[string]serverconfig.ModelAdapterConfig, len(models))
	desiredOrder := make([]string, 0, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, dup := desiredByModel[key]; dup {
			continue
		}
		displayName := strings.TrimSpace(model.DisplayName)
		if displayName == "" {
			displayName = name
		}
		note := ownerLabel
		if email != "" {
			note = email + " · " + ownerLabel
		}
		tooltip := note
		if model.DefaultOn {
			tooltip = note + " (default)"
		}
		spec := modelcontext.CompactCursorModelSpecJSON(model.PickerSpec)
		windowTokens := model.ContextWindowTokens
		var (
			reasoningEffort string
			maxCompletion   int
			pricing         *serverconfig.ModelPricing
			fastMode        bool
			disabled        bool
		)
		if prev, ok := previousByModel[key]; ok {
			// Preserve user-selected window if Cursor offers it on this model.
			windowTokens = modelcontext.ResolveCursorAccountTokens(name, spec, prev.ContextWindowTokens)
			reasoningEffort = strings.TrimSpace(prev.ReasoningEffort)
			maxCompletion = prev.MaxCompletionTokens
			pricing = prev.Pricing
			fastMode = prev.FastMode
			disabled = prev.Disabled
		} else if windowTokens <= 0 {
			windowTokens = modelcontext.ResolveCursorAccountTokens(name, spec, 0)
		}

		built := serverconfig.ModelAdapterConfig{
			ID:                  ownerLabel + ":" + name,
			Source:              legacyruntime.ModelSourceCursorAccount,
			CredentialScope:     legacyruntime.CredentialScopeCursorAccount,
			DisplayName:         displayName,
			GroupName:           ownerLabel,
			Type:                legacyruntime.ModelSourceCursorAccount,
			SupplierID:          ownerLabel,
			CursorAccountID:     accountID,
			TooltipData:         tooltip,
			ModelID:             name,
			ReasoningEffort:     reasoningEffort,
			ContextWindowTokens: windowTokens,
			MaxCompletionTokens: maxCompletion,
			Pricing:             pricing,
			FastMode:            fastMode,
			CursorModelSpec:     spec,
			Disabled:            disabled,
		}
		desired = append(desired, built)
		desiredByModel[key] = built
		desiredOrder = append(desiredOrder, key)
	}

	cursorCopies := 0
	for _, item := range existing {
		if legacyruntime.NormalizeModelSource(item.Source) != legacyruntime.ModelSourceCursorAccount {
			continue
		}
		if strings.TrimSpace(item.ModelID) == "" {
			continue
		}
		cursorCopies++
	}

	next = make([]serverconfig.ModelAdapterConfig, 0, len(existing)+len(desired))
	placed := false
	for _, item := range existing {
		if legacyruntime.NormalizeModelSource(item.Source) != legacyruntime.ModelSourceCursorAccount {
			next = append(next, item)
			continue
		}
		if placed {
			continue
		}
		next = append(next, desired...)
		placed = true
	}
	if !placed {
		next = append(next, desired...)
	}

	keptModels := 0
	for _, key := range desiredOrder {
		prev, existed := previousByModel[key]
		if !existed {
			added++
			continue
		}
		keptModels++
		if !cursorAdapterEqual(prev, desiredByModel[key]) {
			updated++
		}
	}
	// Each duplicate slot copy is a row the shared list no longer keeps.
	removed = cursorCopies - keptModels
	if removed < 0 {
		removed = 0
	}
	return next, ownerLabel, added, updated, removed
}

// collapseCursorAccountCatalog folds cursor_account1 / cursor_account2 copies
// into one cursor_account list owned by accountID. An empty accountID keeps
// whichever copy is already selected for that model.
func collapseCursorAccountCatalog(existing []serverconfig.ModelAdapterConfig, accountID, email string) (next []serverconfig.ModelAdapterConfig, removed int, changed bool) {
	accountID = strings.TrimSpace(accountID)
	email = strings.TrimSpace(email)
	type picked struct {
		item serverconfig.ModelAdapterConfig
	}
	best := make(map[string]picked, len(existing))
	order := make([]string, 0, len(existing))
	cursorCopies := 0
	for _, item := range existing {
		if legacyruntime.NormalizeModelSource(item.Source) != legacyruntime.ModelSourceCursorAccount {
			continue
		}
		cursorCopies++
		key := strings.ToLower(strings.TrimSpace(item.ModelID))
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(item.ID))
		}
		if key == "" {
			continue
		}
		prev, ok := best[key]
		if !ok {
			best[key] = picked{item: item}
			order = append(order, key)
			continue
		}
		if preferCursorAdapter(item, prev.item, accountID) {
			best[key] = picked{item: item}
		}
	}
	collapsed := make([]serverconfig.ModelAdapterConfig, 0, len(order))
	for _, key := range order {
		chosen := best[key].item
		normalized := normalizeSharedCursorAdapter(chosen, accountID, email)
		if !cursorAdapterEqual(chosen, normalized) {
			changed = true
		}
		collapsed = append(collapsed, normalized)
	}
	removed = cursorCopies - len(collapsed)
	if removed > 0 {
		changed = true
	}
	next = make([]serverconfig.ModelAdapterConfig, 0, len(existing))
	placed := false
	for _, item := range existing {
		if legacyruntime.NormalizeModelSource(item.Source) != legacyruntime.ModelSourceCursorAccount {
			next = append(next, item)
			continue
		}
		if placed {
			continue
		}
		next = append(next, collapsed...)
		placed = true
	}
	return next, removed, changed
}

func preferCursorAdapter(candidate, current serverconfig.ModelAdapterConfig, accountID string) bool {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false
	}
	candidateOwner := strings.TrimSpace(candidate.CursorAccountID)
	currentOwner := strings.TrimSpace(current.CursorAccountID)
	return candidateOwner == accountID && currentOwner != accountID
}

func normalizeSharedCursorAdapter(item serverconfig.ModelAdapterConfig, accountID, email string) serverconfig.ModelAdapterConfig {
	modelID := strings.TrimSpace(item.ModelID)
	if modelID != "" {
		item.ID = sharedCursorAccountLabel + ":" + modelID
	}
	item.GroupName = sharedCursorAccountLabel
	item.SupplierID = sharedCursorAccountLabel
	item.Source = legacyruntime.ModelSourceCursorAccount
	item.CredentialScope = legacyruntime.CredentialScopeCursorAccount
	item.Type = legacyruntime.ModelSourceCursorAccount
	if strings.TrimSpace(accountID) != "" {
		item.CursorAccountID = strings.TrimSpace(accountID)
	}
	item.TooltipData = sharedCursorTooltip(email, item.TooltipData)
	return item
}

func sharedCursorTooltip(email, previous string) string {
	email = strings.TrimSpace(email)
	base := sharedCursorAccountLabel
	if email != "" {
		base = email + " · " + sharedCursorAccountLabel
	} else {
		head, _, found := strings.Cut(previous, " · ")
		head = strings.TrimSpace(head)
		if found && head != "" && !strings.HasPrefix(head, sharedCursorAccountLabel) {
			base = head + " · " + sharedCursorAccountLabel
		}
	}
	if strings.Contains(previous, "(default)") {
		return base + " (default)"
	}
	return base
}

// collapseSharedCursorAccountModels rewrites any per-account Cursor model copies
// into the single cursor_account list. It persists only when something changed,
// so a config that is already shared is left untouched.
func (s *ProxyService) collapseSharedCursorAccountModels(accountID, email string) {
	if s == nil {
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()

	cfg, err := s.LoadUserConfig()
	if err != nil {
		logger.Warnf("collapse cursor account models: load config: %v", err)
		return
	}
	next, removed, changed := collapseCursorAccountCatalog(cfg.ModelAdapters, accountID, email)
	if !changed {
		return
	}
	cfg.ModelAdapters = next
	if err := s.persistSyncedModelAdaptersLocked(cfg); err != nil {
		logger.Warnf("collapse cursor account models failed: %v", err)
		return
	}
	logger.Infof("collapsed cursor account models into %s removed=%d", sharedCursorAccountLabel, removed)
}

func cursorAdapterEqual(a, b serverconfig.ModelAdapterConfig) bool {
	return strings.TrimSpace(a.ID) == strings.TrimSpace(b.ID) &&
		strings.TrimSpace(a.DisplayName) == strings.TrimSpace(b.DisplayName) &&
		strings.TrimSpace(a.GroupName) == strings.TrimSpace(b.GroupName) &&
		strings.TrimSpace(a.SupplierID) == strings.TrimSpace(b.SupplierID) &&
		strings.TrimSpace(a.CursorAccountID) == strings.TrimSpace(b.CursorAccountID) &&
		strings.TrimSpace(a.TooltipData) == strings.TrimSpace(b.TooltipData) &&
		strings.TrimSpace(a.ModelID) == strings.TrimSpace(b.ModelID) &&
		strings.TrimSpace(a.ReasoningEffort) == strings.TrimSpace(b.ReasoningEffort) &&
		a.ContextWindowTokens == b.ContextWindowTokens &&
		a.MaxCompletionTokens == b.MaxCompletionTokens &&
		a.FastMode == b.FastMode &&
		a.Disabled == b.Disabled &&
		strings.TrimSpace(a.CursorModelSpec) == strings.TrimSpace(b.CursorModelSpec) &&
		reflect.DeepEqual(a.Pricing, b.Pricing)
}

// cursorAccountAuthorizationForCatalog resolves a bearer token for the requested
// account. Unlike the earlier fallback implementation, when a non-empty accountID
// is explicitly requested, it NEVER silently substitutes another account's token.
func (s *ProxyService) cursorAccountAuthorizationForCatalog(ctx context.Context, accountID string) (string, string, string, error) {
	accountID = strings.TrimSpace(accountID)
	resolved := accountID
	if resolved == "" {
		resolved = strings.TrimSpace(s.cursorAccount.CurrentAccountID())
	}
	email := s.cursorAccountEmailByID(resolved)

	authorization, err := s.cursorAccount.AuthorizationFor(ctx, accountID)
	if err == nil {
		if resolved == "" {
			resolved = strings.TrimSpace(s.cursorAccount.CurrentAccountID())
		}
		return authorization, resolved, email, nil
	}

	// Attempt a forced refresh/re-exchange on the SAME account before failing.
	if resolved != "" {
		if renewed, refreshErr := s.cursorAccount.ForceRefresh(ctx, resolved); refreshErr == nil {
			return renewed, resolved, email, nil
		}
	}
	target := email
	if target == "" {
		target = resolved
	}
	if target != "" {
		return "", "", "", fmt.Errorf("cursor_account_sign_in_required: could not authorize Cursor account %s: %w", target, err)
	}
	return "", "", "", fmt.Errorf("cursor_account_sign_in_required: resolve Cursor account authorization: %w", err)
}

func (s *ProxyService) cursorAccountEmailByID(accountID string) string {
	if s == nil || s.cursorAccount == nil || strings.TrimSpace(accountID) == "" {
		return ""
	}
	accounts, err := s.cursorAccount.ListAccounts()
	if err != nil {
		return ""
	}
	for _, acc := range accounts {
		if acc.ID == accountID {
			return strings.TrimSpace(acc.Email)
		}
	}
	return ""
}

// catalogAuthRejected reports a credential rejection that a forced key
// re-exchange or OAuth refresh can fix.
func catalogAuthRejected(err error) bool {
	var statusErr *cursorCatalogStatusError
	if errors.As(err, &statusErr) && statusErr != nil {
		return statusErr.authRejected()
	}
	lower := strings.ToLower(fmt.Sprint(err))
	return strings.Contains(lower, "not logged in") ||
		strings.Contains(lower, "error_not_logged_in") ||
		strings.Contains(lower, "error_auth_token_expired") ||
		strings.Contains(lower, "unauthenticated")
}

// cursorModelsHTTPClient talks to Cursor's control plane using the app's own
// proxy-aware transport, falling back to cursorModelsDirectHTTPClient if a
// stale local/system proxy refuses the connection.
var cursorModelsHTTPClient = &http.Client{
	Timeout:   cursorModelsTimeout,
	Transport: netproxy.NewTransport(nil),
}

var cursorModelsDirectHTTPClient = &http.Client{
	Timeout:   cursorModelsTimeout,
	Transport: directTransport(),
}

func directTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		clone := transport.Clone()
		clone.Proxy = nil
		return clone
	}
	return &http.Transport{Proxy: nil}
}

func cursorCatalogTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *cursorCatalogStatusError
	return !errors.As(err, &statusErr)
}

type cursorCatalogStatusError struct {
	status int
	detail string
}

func (e *cursorCatalogStatusError) Error() string {
	base := "Cursor refused the model catalog request (HTTP " + strconv.Itoa(e.status) + ")"
	if e.detail == "" {
		return base
	}
	return base + ": " + e.detail
}

func (e *cursorCatalogStatusError) authRejected() bool {
	return e.status == http.StatusUnauthorized || e.status == http.StatusForbidden
}

// requestCursorModelCatalog sends the same AvailableModels request as Cursor IDE 3.30+:
// one model per family, with effort, fast/speed, max-mode, and context windows.
func requestCursorModelCatalog(ctx context.Context, authorization string) (*aiserverv1.AvailableModelsResponse, error) {
	version := upstream.CursorClientVersion()
	body, err := proto.Marshal(&aiserverv1.AvailableModelsRequest{
		IsNightly:                strings.Contains(strings.ToLower(version), "nightly"),
		IncludeLongContextModels: true,
		ExcludeMaxNamedModels:    true,
		UseModelParameters:       proto.Bool(true),
		UseReactModelPicker:      proto.Bool(true),
	})
	if err != nil {
		return nil, err
	}

	parsed, err := doCursorCatalogRequest(ctx, cursorModelsHTTPClient, body, authorization)
	if err == nil {
		return parsed, nil
	}
	var statusErr *cursorCatalogStatusError
	if errors.As(err, &statusErr) && statusErr != nil {
		return nil, err
	}
	if !cursorCatalogTransportFailure(err) {
		return nil, err
	}
	logger.Warnf("cursor model catalog failed through the configured proxy (%v); retrying directly", err)
	direct, directErr := doCursorCatalogRequest(ctx, cursorModelsDirectHTTPClient, body, authorization)
	if directErr != nil {
		return nil, directErr
	}
	return direct, nil
}

func doCursorCatalogRequest(ctx context.Context, client *http.Client, body []byte, authorization string) (*aiserverv1.AvailableModelsResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorModelsEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	upstream.ApplyCursorClientHeaders(req, authorization, "application/proto")
	req.Header.Set("accept", "application/proto")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Cursor model catalog: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, cursorModelsMaxRespBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &cursorCatalogStatusError{
			status: resp.StatusCode,
			detail: cursorCatalogErrorDetail(payload),
		}
	}
	parsed := &aiserverv1.AvailableModelsResponse{}
	if err := proto.Unmarshal(payload, parsed); err != nil {
		if detail := cursorCatalogErrorDetail(payload); detail != "" {
			return nil, errors.New("Cursor refused the model catalog request: " + detail)
		}
		return nil, fmt.Errorf("decode Cursor model catalog: %w", err)
	}
	return parsed, nil
}

func cursorCatalogErrorDetail(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	text := strings.TrimSpace(string(payload))
	if detail := cursorQuotedField(text, `"detail":"`); detail != "" {
		return detail
	}
	if title := cursorQuotedField(text, `"title":"`); title != "" {
		return title
	}
	if code := cursorQuotedField(text, `"error":"`); code != "" {
		if message := cursorQuotedField(text, `"message":"`); message != "" && message != code {
			return code + ": " + message
		}
		return code
	}
	if message := cursorQuotedField(text, `"message":"`); message != "" {
		return message
	}
	if len(text) > 240 || !isMostlyPrintable(text) {
		return ""
	}
	return text
}

func cursorQuotedField(text, marker string) string {
	index := strings.Index(text, marker)
	if index < 0 {
		return ""
	}
	rest := text[index+len(marker):]
	escaped := false
	for i := 0; i < len(rest); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch rest[i] {
		case '\\':
			escaped = true
		case '"':
			var decoded string
			if err := json.Unmarshal([]byte(`"`+rest[:i]+`"`), &decoded); err != nil {
				return rest[:i]
			}
			return decoded
		}
	}
	return ""
}

func isMostlyPrintable(text string) bool {
	if text == "" {
		return false
	}
	printable := 0
	for _, r := range text {
		if r == '\n' || r == '\r' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			printable++
		}
	}
	return printable*10 >= len([]rune(text))*9
}

func collapseCursorCatalogModels(models []*aiserverv1.AvailableModelsResponse_AvailableModel) []CursorAccountModel {
	type family struct {
		order int
		model CursorAccountModel
	}
	families := map[string]*family{}
	order := 0
	for _, model := range models {
		if model.GetIsHidden() {
			continue
		}
		name := strings.TrimSpace(model.GetServerModelName())
		if name == "" {
			name = strings.TrimSpace(model.GetName())
		}
		if name == "" {
			continue
		}
		displayName := strings.TrimSpace(model.GetClientDisplayName())
		if displayName == "" {
			displayName = name
		}
		base, _, _, variant := splitCursorCatalogVariant(name)
		key := name
		storedName := name
		if variant {
			key = base
			storedName = base
			if pretty := prettyCursorCatalogName(base); pretty != "" {
				displayName = pretty
			}
		}
		spec := cursorPickerSpecJSON(model)
		current, exists := families[key]
		if !exists {
			families[key] = &family{order: order, model: CursorAccountModel{Name: storedName, DisplayName: displayName, DefaultOn: model.GetDefaultOn(), PickerSpec: spec}}
			order++
			continue
		}
		if spec != "" && (current.model.PickerSpec == "" || !variant) {
			current.model.PickerSpec = spec
		}
		if model.GetDefaultOn() {
			current.model.DefaultOn = true
		}
		if !variant {
			current.model.Name = name
			if strings.TrimSpace(model.GetClientDisplayName()) != "" {
				current.model.DisplayName = displayName
			}
		}
	}
	catalog := make([]CursorAccountModel, 0, len(families))
	ranked := make([]*family, 0, len(families))
	for _, item := range families {
		ranked = append(ranked, item)
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].order < ranked[j].order })
	for _, item := range ranked {
		if window, ok := modelcontext.ResolveCursorContextWindow(item.model.PickerSpec, 0); ok {
			item.model.ContextWindowTokens = window.Tokens
		}
		catalog = append(catalog, item.model)
	}
	return catalog
}

func cursorPickerSpecJSON(model *aiserverv1.AvailableModelsResponse_AvailableModel) string {
	if model == nil {
		return ""
	}
	spec := map[string]any{}
	if model.SupportsMaxMode != nil {
		spec["supportsMaxMode"] = model.GetSupportsMaxMode()
	}
	if model.SupportsNonMaxMode != nil {
		spec["supportsNonMaxMode"] = model.GetSupportsNonMaxMode()
	}
	if model.GetContextTokenLimit() > 0 {
		spec["contextTokenLimit"] = model.GetContextTokenLimit()
	}
	if model.GetContextTokenLimitForMaxMode() > 0 {
		spec["contextTokenLimitForMaxMode"] = model.GetContextTokenLimitForMaxMode()
	}
	if defs := marshalCursorProtoList(pruneCursorParameterDefinitions(model.GetParameterDefinitions())); len(defs) > 0 {
		spec["parameterDefinitions"] = defs
	}
	if variants := marshalCursorProtoList(pruneCursorVariants(model.GetVariants())); len(variants) > 0 {
		spec["variants"] = variants
	}
	if len(spec) == 0 {
		return ""
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	windows := modelcontext.CursorContextWindows(string(raw))
	if len(windows) == 0 {
		return string(raw)
	}
	spec["contextWindows"] = windows
	if raw, err = json.Marshal(spec); err != nil {
		return ""
	}
	return string(raw)
}

func marshalCursorProtoList[T proto.Message](items []T) []json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		raw, err := (protojson.MarshalOptions{EmitUnpopulated: false}).Marshal(item)
		if err != nil || len(raw) == 0 {
			continue
		}
		out = append(out, append(json.RawMessage(nil), raw...))
	}
	return out
}

// pruneCursorParameterDefinitions strips verbose markdown tooltips, badges, and
// admin-allowlist flags from parameter definitions while preserving Name,
// IsCycleableByHotkey, option DisplayName, and IncreasesModelCost so Cursor IDE's
// React model picker renders parameter labels and hotkey cycling identically to
// official upstream.
func pruneCursorParameterDefinitions(defs []*aiserverv1.ModelParameterDefinition) []*aiserverv1.ModelParameterDefinition {
	if len(defs) == 0 {
		return defs
	}
	out := make([]*aiserverv1.ModelParameterDefinition, 0, len(defs))
	for _, def := range defs {
		if def == nil {
			continue
		}
		clone := proto.Clone(def).(*aiserverv1.ModelParameterDefinition)
		clone.MarkdownTooltip = nil
		if pt := clone.GetParameterType(); pt != nil {
			if enumParam := pt.GetEnumParameter(); enumParam != nil {
				for _, val := range enumParam.GetValues() {
					if val == nil {
						continue
					}
					val.MarkdownTooltip = nil
					val.ModelPickerBadges = nil
					val.BlockedByAdminAllowlist = nil
				}
			}
			if boolParam := pt.GetBooleanParameter(); boolParam != nil {
				for _, val := range boolParam.GetValues() {
					if val == nil {
						continue
					}
					val.DefaultBlockedInAdminAllowlist = nil
					val.HideFromUserPickerWhenAdminBlocked = nil
					val.BlockedByAdminAllowlist = nil
				}
			}
		}
		out = append(out, clone)
	}
	return out
}

// pruneCursorVariants strips display-only fields Cursor attaches to each variant;
// upstream/mocks.go regenerates displayName and variantStringRepresentation for
// each channel ID at AvailableModels time.
func pruneCursorVariants(variants []*aiserverv1.AvailableModelsResponse_ModelVariantConfig) []*aiserverv1.AvailableModelsResponse_ModelVariantConfig {
	if len(variants) == 0 {
		return variants
	}
	out := make([]*aiserverv1.AvailableModelsResponse_ModelVariantConfig, 0, len(variants))
	for _, variant := range variants {
		if variant == nil {
			continue
		}
		clone := proto.Clone(variant).(*aiserverv1.AvailableModelsResponse_ModelVariantConfig)
		clone.DisplayName = ""
		clone.DisplayNameOutsidePicker = nil
		clone.TooltipData = nil
		clone.Tagline = nil
		clone.VariantStringRepresentation = nil
		clone.ConfirmationDialogue = nil
		clone.LegacySlug = nil
		out = append(out, clone)
	}
	return out
}

func splitCursorCatalogVariant(modelID string) (base string, effort string, fast bool, ok bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if strings.HasSuffix(id, "-fast") {
		fast = true
		id = strings.TrimSuffix(id, "-fast")
	}
	for _, suffix := range []string{"-xhigh", "-medium", "-high", "-low", "-max"} {
		if strings.HasSuffix(id, suffix) {
			return id[:len(id)-len(suffix)], strings.TrimPrefix(suffix, "-"), fast, true
		}
	}
	if fast {
		return id, "medium", true, true
	}
	return "", "", false, false
}

func prettyCursorCatalogName(base string) string {
	parts := strings.Split(strings.TrimSpace(base), "-")
	if len(parts) > 0 && parts[0] == "cursor" {
		parts = parts[1:]
	}
	end := len(parts)
	for end > 0 && isCatalogDigits(parts[end-1]) {
		end--
	}
	words := make([]string, 0, len(parts))
	for _, part := range parts[:end] {
		if part == "" {
			continue
		}
		words = append(words, strings.ToUpper(part[:1])+part[1:])
	}
	name := strings.Join(words, " ")
	if end < len(parts) {
		if name != "" {
			name += " "
		}
		name += strings.Join(parts[end:], ".")
	}
	return name
}

func isCatalogDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
