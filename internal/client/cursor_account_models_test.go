package client

import (
	"strings"
	"testing"

	"cursor/gen/aiserverv1"
	serverconfig "cursor/internal/backend/server/config"
	"cursor/internal/cursoraccount"

	"google.golang.org/protobuf/proto"
)

func strPtr(value string) *string { return &value }

func TestCollapseCursorCatalogModelsKeepsOneFamily(t *testing.T) {
	models := collapseCursorCatalogModels([]*aiserverv1.AvailableModelsResponse_AvailableModel{
		{Name: "cursor-grok-4.6-low"},
		{Name: "cursor-grok-4.6-low-fast"},
		{Name: "cursor-grok-4.6-medium"},
		{Name: "cursor-grok-4.6-high"},
		{Name: "cursor-grok-4.6-xhigh"},
		{Name: "grok-4.7", ClientDisplayName: strPtr("Grok 4.7")},
	})
	if len(models) != 2 {
		t.Fatalf("models = %#v, want grok 4.6 once and grok 4.7 once", models)
	}
	if models[0].Name != "cursor-grok-4.6" || models[0].DisplayName != "Grok 4.6" {
		t.Fatalf("grok 4.6 = %#v", models[0])
	}
	if models[1].Name != "grok-4.7" || models[1].DisplayName != "Grok 4.7" {
		t.Fatalf("grok 4.7 = %#v", models[1])
	}
}

func TestCollapseCursorCatalogModelsKeepsPickerParameters(t *testing.T) {
	models := collapseCursorCatalogModels([]*aiserverv1.AvailableModelsResponse_AvailableModel{
		{
			Name:              "claude-fable-5-2",
			ClientDisplayName: strPtr("Claude Fable 5.2"),
			SupportsMaxMode:   proto.Bool(true),
			ParameterDefinitions: []*aiserverv1.ModelParameterDefinition{{
				Id:   "fast",
				Name: "Fast",
				ParameterType: &aiserverv1.ModelParameterDefinition_ModelParameterType{
					BooleanParameter: &aiserverv1.ModelParameterDefinition_BooleanParameterDefinition{},
				},
			}},
		},
	})
	if len(models) != 1 {
		t.Fatalf("models = %#v", models)
	}
	if !strings.Contains(models[0].PickerSpec, `"supportsMaxMode":true`) || !strings.Contains(models[0].PickerSpec, `"id":"fast"`) {
		t.Fatalf("picker spec = %s", models[0].PickerSpec)
	}
}

func TestCollapseCursorCatalogModelsListsContextWindows(t *testing.T) {
	contextVariant := func(value string, maxMode bool, defaultNonMax bool) *aiserverv1.AvailableModelsResponse_ModelVariantConfig {
		variant := &aiserverv1.AvailableModelsResponse_ModelVariantConfig{
			ParameterValues: []*aiserverv1.RequestedModel_ModelParameterValue{{Id: "context", Value: value}},
			IsMaxMode:       maxMode,
		}
		if defaultNonMax {
			variant.IsDefaultNonMaxConfig = proto.Bool(true)
		}
		return variant
	}
	models := collapseCursorCatalogModels([]*aiserverv1.AvailableModelsResponse_AvailableModel{
		{
			Name:                        "claude-fable-5-2",
			SupportsMaxMode:             proto.Bool(true),
			SupportsNonMaxMode:          proto.Bool(true),
			ContextTokenLimit:           proto.Int32(300_000),
			ContextTokenLimitForMaxMode: proto.Int32(1_000_000),
			ParameterDefinitions: []*aiserverv1.ModelParameterDefinition{{
				Id: "context",
				ParameterType: &aiserverv1.ModelParameterDefinition_ModelParameterType{
					EnumParameter: &aiserverv1.ModelParameterDefinition_EnumParameterDefinition{
						Values: []*aiserverv1.ModelParameterDefinition_EnumParameterDefinition_EnumParameterValue{{Value: "300k"}, {Value: "1m"}},
					},
				},
			}},
			Variants: []*aiserverv1.AvailableModelsResponse_ModelVariantConfig{
				contextVariant("300k", false, true),
				contextVariant("1m", true, false),
			},
		},
		{
			Name:                        "gemini-4-hydrogen",
			ContextTokenLimit:           proto.Int32(1_000_000),
			ContextTokenLimitForMaxMode: proto.Int32(1_000_000),
		},
		{Name: "default"},
	})
	if len(models) != 3 {
		t.Fatalf("models = %#v", models)
	}
	byName := map[string]CursorAccountModel{}
	for _, model := range models {
		byName[model.Name] = model
	}
	fable := byName["claude-fable-5-2"]
	if fable.ContextWindowTokens != 300_000 {
		t.Fatalf("fable default window = %d, want 300000", fable.ContextWindowTokens)
	}
	if want := `"contextWindows":[{"tokens":300000,"value":"300k","default":true},{"tokens":1000000,"value":"1m","maxMode":true}]`; !strings.Contains(fable.PickerSpec, want) {
		t.Fatalf("fable picker spec = %s, want %s", fable.PickerSpec, want)
	}
	if hydrogen := byName["gemini-4-hydrogen"]; hydrogen.ContextWindowTokens != 1_000_000 {
		t.Fatalf("hydrogen default window = %d, want 1000000", hydrogen.ContextWindowTokens)
	}
	if auto := byName["default"]; auto.ContextWindowTokens != 0 || strings.Contains(auto.PickerSpec, "contextWindows") {
		t.Fatalf("auto model = %#v, want no context windows", auto)
	}
}

func TestPruneCursorParameterDefinitionsPreservesLabelsAndHotkeysWhileStrippingTooltips(t *testing.T) {
	models := collapseCursorCatalogModels([]*aiserverv1.AvailableModelsResponse_AvailableModel{
		{
			Name:              "claude-fable-5-2",
			ClientDisplayName: strPtr("Claude Fable 5.2"),
			ParameterDefinitions: []*aiserverv1.ModelParameterDefinition{{
				Id:                  "effort",
				Name:                "Effort",
				MarkdownTooltip:     strPtr("Very long tooltip that bloats config payloads"),
				IsCycleableByHotkey: proto.Bool(true),
				ParameterType: &aiserverv1.ModelParameterDefinition_ModelParameterType{
					EnumParameter: &aiserverv1.ModelParameterDefinition_EnumParameterDefinition{
						Values: []*aiserverv1.ModelParameterDefinition_EnumParameterDefinition_EnumParameterValue{{
							Value:              "xhigh",
							DisplayName:        strPtr("Extra High"),
							IncreasesModelCost: proto.Bool(true),
							MarkdownTooltip:    strPtr("Per-option tooltip bloat"),
						}},
					},
				},
			}},
		},
	})
	if len(models) != 1 {
		t.Fatalf("models = %#v", models)
	}
	spec := models[0].PickerSpec
	for _, mustKeep := range []string{`"name":"Effort"`, `"isCycleableByHotkey":true`, `"displayName":"Extra High"`, `"increasesModelCost":true`} {
		if !strings.Contains(spec, mustKeep) {
			t.Errorf("pruned spec dropped %s: %s", mustKeep, spec)
		}
	}
	if strings.Contains(spec, "markdownTooltip") {
		t.Errorf("pruned spec retained markdownTooltip bloat: %s", spec)
	}
}

func TestMergeSyncedCursorAccountAdaptersSupportsMergeReplaceAndOrphanCleanup(t *testing.T) {
	known := map[string]cursoraccount.CursorAccountSummary{
		"acct-1": {ID: "acct-1", Email: "one@example.com", IsCurrent: true},
		"acct-2": {ID: "acct-2", Email: "two@example.com"},
	}
	fableSpec := `{"supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":300000,"contextTokenLimitForMaxMode":1000000,"contextWindows":[{"tokens":300000,"value":"300k","default":true},{"tokens":1000000,"value":"1m","maxMode":true}]}`
	catalog := []CursorAccountModel{
		{Name: "claude-fable-5-2", DisplayName: "Claude Fable 5.2", DefaultOn: true, PickerSpec: fableSpec, ContextWindowTokens: 300_000},
		{Name: "gemini-4-carbon", DisplayName: "Gemini 4 Carbon", DefaultOn: false, ContextWindowTokens: 200_000},
	}

	// Sync acct-1 from an initial list containing a third-party model and an orphaned deleted-account model.
	initial := []serverconfig.ModelAdapterConfig{
		{ID: "tp:gpt-5", Source: "third_party", DisplayName: "GPT-5", ModelID: "gpt-5"},
		{ID: "cursor_account9:old-model", Source: "cursor_account", SupplierID: "cursor_account9", GroupName: "cursor_account9", CursorAccountID: "deleted-acct", ModelID: "old-model", DisplayName: "Old"},
	}
	afterAcct1, slot1, added1, _, removed1 := mergeSyncedCursorAccountAdapters(initial, catalog, "acct-1", "one@example.com", known, false)
	if slot1 != "cursor_account1" || added1 != 2 || removed1 != 1 {
		t.Fatalf("acct-1 sync = slot=%s added=%d removed=%d adapters=%+v", slot1, added1, removed1, afterAcct1)
	}

	// User customizes Claude Fable 5.2 to 1M Max window on acct-1.
	for i := range afterAcct1 {
		if afterAcct1[i].ModelID == "claude-fable-5-2" {
			afterAcct1[i].ContextWindowTokens = 1_000_000
		}
	}

	// Sync acct-2 in default merge mode: acct-1 adapters stay intact, acct-2 gets cursor_account2.
	afterAcct2, slot2, added2, _, removed2 := mergeSyncedCursorAccountAdapters(afterAcct1, catalog, "acct-2", "two@example.com", known, false)
	if slot2 != "cursor_account2" || added2 != 2 || removed2 != 0 || len(afterAcct2) != 5 {
		t.Fatalf("acct-2 merge sync = slot=%s added=%d removed=%d len=%d", slot2, added2, removed2, len(afterAcct2))
	}

	// Sync acct-2 in replace mode: collapses onto cursor_account1 IDs, reassigns owner to acct-2, AND preserves user's 1M context window!
	replaced, slotReplace, _, _, _ := mergeSyncedCursorAccountAdapters(afterAcct2, catalog, "acct-2", "two@example.com", known, true)
	if slotReplace != "cursor_account1" || len(replaced) != 3 {
		t.Fatalf("replace sync = slot=%s len=%d adapters=%+v", slotReplace, len(replaced), replaced)
	}
	for _, item := range replaced {
		if item.Source == "cursor_account" {
			if item.CursorAccountID != "acct-2" {
				t.Fatalf("expected cursorAccountID acct-2 after replace, got %q", item.CursorAccountID)
			}
			if item.ModelID == "claude-fable-5-2" && item.ContextWindowTokens != 1_000_000 {
				t.Fatalf("replace lost user's chosen 1M context window: %d", item.ContextWindowTokens)
			}
		}
	}
}
