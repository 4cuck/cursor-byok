package config

import (
	"context"
	"strings"

	modeladapter "cursor/internal/backend/agent/model"
	"cursor/internal/modelchannel"
	legacyruntime "cursor/internal/runtime"
)

const (
	defaultChannelContextWindowTokens = 200_000
	defaultChannelMaxTokens           = 4_096
	defaultChannelAnthropicMaxTokens  = 65_536
	defaultChannelAnthropicEffort     = "xhigh"
)

func (manager *Manager) SelectChannelForModel(ctx context.Context, modelID string) (*legacyruntime.ResolvedChannel, error) {
	channels, err := manager.SelectChannelsForModel(ctx, modelID)
	if err != nil || len(channels) == 0 {
		return nil, err
	}
	return channels[0], nil
}

// ModelSourceForModel 返回当前配置中模型应使用的来源，仅供缓存等不应跨来源共享
// 的本地元数据域使用。它不推进轮询游标，也不触发任何网络请求。
func (manager *Manager) ModelSourceForModel(_ context.Context, modelID string) string {
	if manager == nil {
		return legacyruntime.ModelSourceThirdParty
	}
	adapters, err := NormalizeModelAdapterConfigs(manager.Current().ModelAdapters)
	if err != nil {
		return legacyruntime.ModelSourceThirdParty
	}
	matches := modelchannel.ResolveAdapterIndexes(
		adapters,
		modelID,
		func(adapter ModelAdapterConfig) string { return adapter.ID },
		func(adapter ModelAdapterConfig) string { return adapter.ModelID },
		func(adapter ModelAdapterConfig) string {
			return modelchannel.BuildLegacyChannelID(adapter.BaseURL, adapter.ModelID, adapter.APIKey, adapter.DisplayName)
		},
		func(adapter ModelAdapterConfig) string {
			return modelchannel.BuildChannelID(adapter.BaseURL, adapter.ModelID, adapter.APIKey, adapter.DisplayName, adapter.OpenAIEndpoint)
		},
	)
	if len(matches) == 0 {
		return legacyruntime.ModelSourceThirdParty
	}
	if source := selectModelSource(adapters, matches, modelID); source != "" {
		return source
	}
	return legacyruntime.ModelSourceThirdParty
}

func (manager *Manager) SelectChannelsForModel(_ context.Context, modelID string) ([]*legacyruntime.ResolvedChannel, error) {
	if manager == nil {
		return nil, legacyruntime.ErrChannelNotAvailable
	}
	adapters, err := NormalizeModelAdapterConfigs(manager.Current().ModelAdapters)
	if err != nil {
		return nil, err
	}
	syncExplicitCompatibilityKindOverrides(adapters)
	matches := modelchannel.ResolveAdapterIndexes(
		adapters,
		modelID,
		func(adapter ModelAdapterConfig) string { return adapter.ID },
		func(adapter ModelAdapterConfig) string { return adapter.ModelID },
		func(adapter ModelAdapterConfig) string {
			return modelchannel.BuildLegacyChannelID(adapter.BaseURL, adapter.ModelID, adapter.APIKey, adapter.DisplayName)
		},
		func(adapter ModelAdapterConfig) string {
			return modelchannel.BuildChannelID(adapter.BaseURL, adapter.ModelID, adapter.APIKey, adapter.DisplayName, adapter.OpenAIEndpoint)
		},
	)
	if len(matches) == 0 {
		return nil, legacyruntime.ErrChannelNotAvailable
	}
	selectedSource := selectModelSource(adapters, matches, modelID)
	if selectedSource == "" {
		return nil, legacyruntime.ErrChannelNotAvailable
	}
	filteredMatches := make([]int, 0, len(matches))
	for _, index := range matches {
		// 已停用渠道不参与路由（测试失败自动停用的模型不再被选中）
		if adapters[index].Disabled {
			continue
		}
		if adapters[index].Source == selectedSource {
			filteredMatches = append(filteredMatches, index)
		}
	}
	if len(filteredMatches) == 0 {
		return nil, legacyruntime.ErrChannelNotAvailable
	}
	// Cursor 账户模型：把同一模型在其他账户下的适配器追加为候选渠道。
	// 一个账户用完某模型的额度后，Cursor 用文字错误拒绝（没有 HTTP 状态码），
	// router 只拿到一个候选渠道时就只能失败；带上同模型的其他账户后，
	// 本轮请求可以直接切到还有额度的账户。
	if selectedSource == legacyruntime.ModelSourceCursorAccount {
		filteredMatches = appendCursorAccountSiblingMatches(adapters, filteredMatches)
	}
	// 展开备用密钥池：带 apiKeys 的 adapter 复制成每把密钥一个独立渠道。
	// 轮换游标在展开后的完整列表上推进，密钥池天然参与轮换；冷却指纹包含
	// apiKey，单把密钥被限流只冷却该克隆渠道，不影响同渠道其他密钥。
	candidates := expandAdapterKeyPools(adapters, filteredMatches)
	manager.selectionMu.Lock()
	if manager.selectionOffsets == nil {
		manager.selectionOffsets = make(map[string]int)
	}
	key := selectedSource + "\x00" + strings.TrimSpace(modelID)
	offset := 0
	// 账户模型的候选顺序必须稳定：首渠道是配置中选定的那个账户，
	// 其余账户只作为失败后的后备。轮换会让"当前选中账户"每轮漂移。
	if selectedSource != legacyruntime.ModelSourceCursorAccount {
		offset = manager.selectionOffsets[key] % len(candidates)
		manager.selectionOffsets[key] = (offset + 1) % len(candidates)
	}
	manager.selectionMu.Unlock()
	channels := make([]*legacyruntime.ResolvedChannel, 0, len(candidates))
	for index := 0; index < len(candidates); index++ {
		adapter := candidates[(offset+index)%len(candidates)]
		channel, resolveErr := resolveModelAdapterChannel([]ModelAdapterConfig{adapter}, adapter.ID)
		if resolveErr != nil {
			return nil, resolveErr
		}
		channels = append(channels, channel)
	}
	return channels, nil
}

// appendCursorAccountSiblingMatches adds adapters that expose the same model
// under a different Cursor account, and places non-exhausted accounts ahead of
// accounts currently in per-model quota cooldown.
func appendCursorAccountSiblingMatches(adapters []ModelAdapterConfig, matches []int) []int {
	if len(matches) == 0 {
		return matches
	}
	wanted := make(map[string]struct{}, len(matches))
	for _, index := range matches {
		name := strings.ToLower(strings.TrimSpace(adapters[index].ModelID))
		if name == "" {
			continue
		}
		wanted[name] = struct{}{}
	}
	if len(wanted) == 0 {
		return matches
	}
	seen := make(map[int]struct{}, len(matches))
	for _, index := range matches {
		seen[index] = struct{}{}
	}
	extended := append([]int(nil), matches...)
	for index := range adapters {
		if _, ok := seen[index]; ok {
			continue
		}
		adapter := adapters[index]
		if legacyruntime.NormalizeModelSource(adapter.Source) != legacyruntime.ModelSourceCursorAccount {
			continue
		}
		if adapter.Disabled {
			continue
		}
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(adapter.ModelID))]; !ok {
			continue
		}
		extended = append(extended, index)
	}
	if len(extended) <= 1 {
		return extended
	}
	ready := make([]int, 0, len(extended))
	exhausted := make([]int, 0, len(extended))
	for _, index := range extended {
		adapter := adapters[index]
		if modeladapter.CursorAccountModelExhausted(adapter.CursorAccountID, adapter.ModelID) {
			exhausted = append(exhausted, index)
		} else {
			ready = append(ready, index)
		}
	}
	return append(ready, exhausted...)
}

// expandAdapterKeyPools 把命中的 adapter 列表按备用密钥池展开成逐密钥的候选渠道。
func expandAdapterKeyPools(adapters []ModelAdapterConfig, matches []int) []ModelAdapterConfig {
	capacity := 0
	for _, index := range matches {
		capacity += 1 + len(adapters[index].APIKeys)
	}
	candidates := make([]ModelAdapterConfig, 0, capacity)
	for _, index := range matches {
		adapter := adapters[index]
		if len(adapter.APIKeys) == 0 {
			candidates = append(candidates, adapter)
			continue
		}
		// 主 apiKey 渠道保持首位，再跟备用密钥克隆
		candidates = append(candidates, adapter)
		for _, apiKey := range adapter.APIKeys {
			clone := adapter
			clone.APIKey = apiKey
			candidates = append(candidates, clone)
		}
	}
	return candidates
}

func selectModelSource(adapters []ModelAdapterConfig, matches []int, requestedModel string) string {
	requestedModel = strings.TrimSpace(requestedModel)
	for _, index := range matches {
		if strings.TrimSpace(adapters[index].ID) == requestedModel {
			return adapters[index].Source
		}
	}
	for _, index := range matches {
		if adapters[index].Source == legacyruntime.ModelSourceThirdParty {
			return legacyruntime.ModelSourceThirdParty
		}
	}
	return adapters[matches[0]].Source
}

func resolveModelAdapterChannel(adapters []ModelAdapterConfig, requestedModel string) (*legacyruntime.ResolvedChannel, error) {
	matchIndexes := modelchannel.ResolveAdapterIndexes(
		adapters,
		requestedModel,
		func(adapter ModelAdapterConfig) string { return adapter.ID },
		func(adapter ModelAdapterConfig) string { return adapter.ModelID },
		func(adapter ModelAdapterConfig) string {
			return modelchannel.BuildLegacyChannelID(adapter.BaseURL, adapter.ModelID, adapter.APIKey, adapter.DisplayName)
		},
		func(adapter ModelAdapterConfig) string {
			return modelchannel.BuildChannelID(adapter.BaseURL, adapter.ModelID, adapter.APIKey, adapter.DisplayName, adapter.OpenAIEndpoint)
		},
	)
	if len(matchIndexes) == 0 {
		return nil, legacyruntime.ErrChannelNotAvailable
	}
	matchIndex := matchIndexes[0]
	matched := adapters[matchIndex]

	resolved := &legacyruntime.ResolvedChannel{
		ID:                          strings.TrimSpace(matched.ID),
		Source:                      strings.TrimSpace(matched.Source),
		CredentialScope:             strings.TrimSpace(matched.CredentialScope),
		Name:                        strings.TrimSpace(matched.DisplayName),
		GroupName:                   strings.TrimSpace(matched.GroupName),
		CursorAccountID:             strings.TrimSpace(matched.CursorAccountID),
		CursorModelSpec:             strings.TrimSpace(matched.CursorModelSpec),
		Provider:                    strings.TrimSpace(matched.Type),
		ProtocolMode:                strings.TrimSpace(matched.ProtocolMode),
		ProtocolGroup:               strings.TrimSpace(matched.ProtocolGroup),
		BaseURL:                     strings.TrimSpace(matched.BaseURL),
		APIKey:                      strings.TrimSpace(matched.APIKey),
		Model:                       strings.TrimSpace(matched.ModelID),
		OpenAIEndpoint:              strings.TrimSpace(matched.OpenAIEndpoint),
		OpenAIRequestGroup:          strings.TrimSpace(matched.OpenAIRequestGroup),
		OpenAIExtraParamsEnabled:    matched.OpenAIExtraParamsEnabled,
		OpenAIExtraParamsJSON:       strings.TrimSpace(matched.OpenAIExtraParamsJSON),
		CustomHeadersEnabled:        matched.CustomHeadersEnabled,
		CustomHeadersJSON:           strings.TrimSpace(matched.CustomHeadersJSON),
		AnthropicExtraParamsEnabled: matched.AnthropicExtraParamsEnabled,
		AnthropicExtraParamsJSON:    strings.TrimSpace(matched.AnthropicExtraParamsJSON),
		AnthropicAuthMode:           strings.TrimSpace(matched.AnthropicAuthMode),
		ContextWindowTokens:         defaultChannelContextWindowTokens,
		MaxTokens:                   defaultChannelMaxTokens,
		ReasoningEffort:             strings.TrimSpace(matched.ReasoningEffort),
		CustomThinkingEfforts:       matched.CustomThinkingEfforts,
		AnthropicMaxTokens:          defaultChannelAnthropicMaxTokens,
		AnthropicThinkingEffort:     defaultChannelAnthropicEffort,
		FastMode:                    matched.FastMode,
		OpenAIServiceTier:           strings.TrimSpace(matched.OpenAIServiceTier),
		ToolCallMode:                strings.TrimSpace(matched.ToolCallMode),
		MaxConcurrencyPerKey:        matched.MaxConcurrencyPerKey,
	}
	if matched.ContextWindowTokens > 0 {
		resolved.ContextWindowTokens = matched.ContextWindowTokens
	}
	if matched.MaxCompletionTokens > 0 {
		resolved.MaxTokens = matched.MaxCompletionTokens
	}
	if matched.AnthropicMaxTokens > 0 {
		resolved.AnthropicMaxTokens = matched.AnthropicMaxTokens
	}
	if matched.ThinkingBudgetTokens > 0 {
		resolved.ThinkingBudgetTokens = matched.ThinkingBudgetTokens
	}
	if strings.TrimSpace(matched.AnthropicThinkingEffort) != "" {
		resolved.AnthropicThinkingEffort = strings.TrimSpace(matched.AnthropicThinkingEffort)
	}
	return resolved, nil
}

// syncExplicitCompatibilityKindOverrides 把当前配置中的显式 compatibilityKind
// 全量同步到 modeladapter 运行时覆盖表。每次渠道选择都执行，保证热加载/删除渠道后
// 覆盖表自动收敛（用户清空字段或删除适配器不会残留旧值）。键为 baseURL+modelID，
// 与 classify 请求侧可用的信号一致。
func syncExplicitCompatibilityKindOverrides(adapters []ModelAdapterConfig) {
	overrides := make(map[string]string, len(adapters))
	for _, adapter := range adapters {
		if strings.TrimSpace(adapter.CompatibilityKind) == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(adapter.BaseURL)) + "\n" + strings.ToLower(strings.TrimSpace(adapter.ModelID))
		overrides[key] = adapter.CompatibilityKind
	}
	modeladapter.SetExplicitCompatibilityKindOverrides(overrides)
}
