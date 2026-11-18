package client

import (
	"strings"
	"testing"

	"cursor/gen/aiserverv1"

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
