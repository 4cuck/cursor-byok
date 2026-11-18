package client

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cursor/gen/aiserverv1"
	"cursor/internal/cursoraccount"
	"cursor/internal/modelcontext"
)

// TestLiveCursorCatalogContextWindows lists the context windows the signed-in
// account's catalog offers for every model.
func TestLiveCursorCatalogContextWindows(t *testing.T) {
	if os.Getenv("CURSOR_CATALOG_LIVE") != "1" {
		t.Skip("set CURSOR_CATALOG_LIVE=1 to list the live Cursor catalog context windows")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// go test sandboxes appdata, so load the real account store directly.
	root := filepath.Join(home, ".cursor-local-assistant-v2", "data")
	manager := cursoraccount.NewManager(root, filepath.Join(root, "cursor-account.json"), &http.Client{Timeout: 30 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	authorization, err := manager.Authorization(ctx)
	if err != nil {
		t.Fatalf("cursor account is not signed in: %v", err)
	}
	parsed, err := requestCursorModelCatalog(ctx, authorization)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range parsed.GetModels() {
		if model.GetIsHidden() {
			continue
		}
		t.Logf("%s limit=%d max=%d auto=%d autoExt=%d supportsMax=%v supportsNonMax=%v context=%s variants=%s",
			model.GetName(),
			model.GetContextTokenLimit(),
			model.GetContextTokenLimitForMaxMode(),
			model.GetAutoContextMaxTokens(),
			model.GetAutoContextExtendedMaxTokens(),
			model.GetSupportsMaxMode(),
			model.GetSupportsNonMaxMode(),
			liveContextParameterValues(model),
			liveVariantSummary(model),
		)
	}
	for _, model := range collapseCursorCatalogModels(parsed.GetModels()) {
		windows := []string{}
		for _, window := range modelcontext.CursorContextWindows(model.PickerSpec) {
			label := strconv.Itoa(window.Tokens)
			if window.MaxMode {
				label += "+max"
			}
			if window.Default {
				label += "*"
			}
			windows = append(windows, label)
		}
		t.Logf("synced %s default=%d windows=[%s]", model.Name, model.ContextWindowTokens, strings.Join(windows, ","))
	}
}

func liveContextParameterValues(model *aiserverv1.AvailableModelsResponse_AvailableModel) string {
	for _, definition := range model.GetParameterDefinitions() {
		if definition.GetId() != "context" {
			continue
		}
		values := []string{}
		for _, value := range definition.GetParameterType().GetEnumParameter().GetValues() {
			values = append(values, value.GetValue())
		}
		return "[" + strings.Join(values, ",") + "]"
	}
	return "-"
}

func liveVariantSummary(model *aiserverv1.AvailableModelsResponse_AvailableModel) string {
	parts := []string{}
	for _, variant := range model.GetVariants() {
		context := ""
		for _, value := range variant.GetParameterValues() {
			if value.GetId() == "context" {
				context = value.GetValue()
			}
		}
		if context == "" && !variant.GetIsMaxMode() {
			continue
		}
		flag := ""
		if variant.GetIsMaxMode() {
			flag = "+max"
		}
		parts = append(parts, context+flag)
	}
	return "[" + strings.Join(dedupeLiveStrings(parts), ",") + "]"
}

func dedupeLiveStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
