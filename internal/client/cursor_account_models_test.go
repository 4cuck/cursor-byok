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
