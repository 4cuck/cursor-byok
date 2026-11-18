package upstream

import (
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"

	"cursor/internal/modelcontext"
	legacyruntime "cursor/internal/runtime"
)

// applyCursorContextWindowLimits exposes both the normal and max-mode limits so
// the Cursor IDE model picker and auto-context logic know the full range offered
// by the model while individual turns size checkpoints from the active window.
func applyCursorContextWindowLimits(entry map[string]any, windows []modelcontext.CursorContextWindow) {
	if entry == nil || len(windows) == 0 {
		return
	}
	normalWindow := defaultNonMaxCursorWindow(windows)
	maxWindow := windows[len(windows)-1]
	entry["contextTokenLimit"] = normalWindow.Tokens
	entry["contextTokenLimitForMaxMode"] = maxWindow.Tokens
	entry["autoContextMaxTokens"] = maxWindow.Tokens
	entry["supportsAutoContext"] = true
}

func defaultNonMaxCursorWindow(windows []modelcontext.CursorContextWindow) modelcontext.CursorContextWindow {
	for _, window := range windows {
		if !window.MaxMode && window.Default {
			return window
		}
	}
	for _, window := range windows {
		if !window.MaxMode {
			return window
		}
	}
	for _, window := range windows {
		if window.Default {
			return window
		}
	}
	return windows[0]
}

// prepareCursorContextParameters preserves all context windows Cursor offers in
// parameterDefinitions and variants (such as 300k and 1m) so the user can switch
// between them directly inside the Cursor IDE model picker. When a spec reports
// distinct normal/max limits without an explicit context parameter, a context
// enum parameter and per-window variants are synthesized so the IDE picker still
// exposes both windows under token-based pricing.
func prepareCursorContextParameters(
	entry map[string]any,
	definitions []map[string]any,
	variants []map[string]any,
	windows []modelcontext.CursorContextWindow,
) ([]map[string]any, []map[string]any) {
	if len(windows) > 1 && !hasCursorContextDefinition(definitions) {
		definitions = append([]map[string]any{buildSynthesizedCursorContextDefinition(windows)}, definitions...)
		variants = expandVariantsAcrossCursorWindows(variants, windows)
	} else {
		ensureCursorContextOptionDisplayNames(definitions)
	}
	if len(variants) == 0 {
		return definitions, variants
	}
	hasMax, hasNonMax := false, false
	for _, variant := range variants {
		if variant["isMaxMode"] == true {
			hasMax = true
		} else {
			hasNonMax = true
		}
	}
	if hasNonMax {
		ensureCursorDefaultVariant(variants, variants, "isDefaultNonMaxConfig", false)
	}
	if hasMax {
		ensureCursorDefaultVariant(variants, variants, "isDefaultMaxConfig", true)
	}
	entry["supportsMaxMode"] = hasMax || entry["supportsMaxMode"] == true
	entry["supportsNonMaxMode"] = hasNonMax
	return definitions, variants
}

func hasCursorContextDefinition(definitions []map[string]any) bool {
	for _, definition := range definitions {
		id, _ := definition["id"].(string)
		if strings.EqualFold(strings.TrimSpace(id), modelcontext.CursorContextParameterID) {
			return true
		}
	}
	return false
}

func buildSynthesizedCursorContextDefinition(windows []modelcontext.CursorContextWindow) map[string]any {
	values := make([]any, 0, len(windows))
	for _, window := range windows {
		val := strings.TrimSpace(window.Value)
		if val == "" {
			val = modelcontext.FormatCursorContextValue(window.Tokens)
		}
		item := map[string]any{
			"value":       val,
			"displayName": formatCursorWindowLabel(window.Tokens),
		}
		if window.MaxMode {
			item["increasesModelCost"] = true
		}
		values = append(values, item)
	}
	return map[string]any{
		"id":              modelcontext.CursorContextParameterID,
		"name":            "Context",
		"markdownTooltip": "Context size the model has available.",
		"parameterType": map[string]any{
			"enumParameter": map[string]any{
				"values": values,
			},
		},
	}
}

func expandVariantsAcrossCursorWindows(variants []map[string]any, windows []modelcontext.CursorContextWindow) []map[string]any {
	baseVariants := variants
	if len(baseVariants) == 0 {
		baseVariants = []map[string]any{{
			"parameterValues":       []any{},
			"isDefaultNonMaxConfig": true,
		}}
	}
	expanded := make([]map[string]any, 0, len(baseVariants)*len(windows))
	for _, window := range windows {
		val := strings.TrimSpace(window.Value)
		if val == "" {
			val = modelcontext.FormatCursorContextValue(window.Tokens)
		}
		for _, base := range baseVariants {
			baseIsMax := base["isMaxMode"] == true
			if !window.MaxMode && baseIsMax && len(baseVariants) > 1 {
				// Avoid duplicating max-only base variants onto the non-max window when
				// a separate non-max base variant already exists.
				hasNonMaxBase := false
				for _, candidate := range baseVariants {
					if candidate["isMaxMode"] != true {
						hasNonMaxBase = true
						break
					}
				}
				if hasNonMaxBase && len(cursorVariantParameterPairs(base)) == 0 {
					continue
				}
			}
			cloned := cloneCursorSpecMap(base)
			delete(cloned, "isDefaultNonMaxConfig")
			delete(cloned, "isDefaultMaxConfig")
			pairs := make([]any, 0, len(cursorVariantParameterPairs(base))+1)
			pairs = append(pairs, map[string]any{
				"id":    modelcontext.CursorContextParameterID,
				"value": val,
			})
			for _, pair := range cursorVariantParameterPairs(base) {
				id, _ := pair["id"].(string)
				if !strings.EqualFold(strings.TrimSpace(id), modelcontext.CursorContextParameterID) {
					pairs = append(pairs, cloneCursorSpecMap(pair))
				}
			}
			cloned["parameterValues"] = pairs
			isMax := window.MaxMode || baseIsMax
			if isMax {
				cloned["isMaxMode"] = true
			} else {
				delete(cloned, "isMaxMode")
			}
			if !isMax && base["isDefaultNonMaxConfig"] == true {
				cloned["isDefaultNonMaxConfig"] = true
			}
			if isMax && window.MaxMode && (base["isDefaultMaxConfig"] == true || base["isDefaultNonMaxConfig"] == true) {
				cloned["isDefaultMaxConfig"] = true
			}
			expanded = append(expanded, cloned)
		}
	}
	return expanded
}

func cloneCursorSpecMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for k, v := range source {
		cloned[k] = v
	}
	return cloned
}

func ensureCursorContextOptionDisplayNames(definitions []map[string]any) {
	for _, definition := range definitions {
		id, _ := definition["id"].(string)
		if !strings.EqualFold(strings.TrimSpace(id), modelcontext.CursorContextParameterID) {
			continue
		}
		if name, _ := definition["name"].(string); strings.TrimSpace(name) == "" {
			definition["name"] = "Context"
		}
		parameterType, _ := definition["parameterType"].(map[string]any)
		enumParam, ok := parameterType["enumParameter"].(map[string]any)
		if !ok {
			continue
		}
		values, ok := enumParam["values"].([]any)
		if !ok {
			continue
		}
		for _, item := range values {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			display, _ := object["displayName"].(string)
			if strings.TrimSpace(display) != "" {
				continue
			}
			rawVal, _ := object["value"].(string)
			if tokens := modelcontext.ParseCursorContextValue(rawVal); tokens > 0 {
				object["displayName"] = formatCursorWindowLabel(tokens)
			} else if strings.TrimSpace(rawVal) != "" {
				object["displayName"] = strings.ToUpper(strings.TrimSpace(rawVal))
			}
		}
	}
}

func ensureCursorDefaultVariant(kept []map[string]any, original []map[string]any, flag string, maxMode bool) {
	for _, variant := range kept {
		if variant[flag] == true && (variant["isMaxMode"] == true) == maxMode {
			return
		}
	}
	for _, variant := range kept {
		delete(variant, flag)
	}
	var reference map[string]string
	for _, variant := range original {
		if variant[flag] == true {
			reference = cursorVariantValues(variant)
			break
		}
	}
	candidates := make([]map[string]any, 0, len(kept))
	for _, variant := range kept {
		if (variant["isMaxMode"] == true) == maxMode {
			candidates = append(candidates, variant)
		}
	}
	if len(candidates) == 0 {
		return
	}
	best, bestScore := candidates[0], -1
	for _, variant := range candidates {
		score := 0
		values := cursorVariantValues(variant)
		for id, value := range reference {
			if !strings.EqualFold(id, modelcontext.CursorContextParameterID) && strings.EqualFold(values[id], value) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = variant, score
		}
	}
	best[flag] = true
}

func cursorVariantParameterPairs(variant map[string]any) []map[string]any {
	switch typed := variant["parameterValues"].(type) {
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return typed
	default:
		return nil
	}
}

func cursorVariantValues(variant map[string]any) map[string]string {
	values := map[string]string{}
	for _, object := range cursorVariantParameterPairs(variant) {
		id, _ := object["id"].(string)
		value, _ := object["value"].(string)
		if id = strings.TrimSpace(id); id != "" {
			values[id] = strings.TrimSpace(value)
		}
	}
	return values
}

// formatCursorVariantRepresentation builds Cursor IDE's native modelId[k=v,...]
// string (matching model-id-params-string.ts in workbench.desktop.main.js).
func formatCursorVariantRepresentation(channelID string, variant map[string]any) string {
	channelID = strings.TrimSpace(channelID)
	pairs := cursorVariantParameterPairs(variant)
	if len(pairs) == 0 {
		return channelID + "[]"
	}
	segments := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		id, _ := pair["id"].(string)
		val, _ := pair["value"].(string)
		id = strings.TrimSpace(id)
		val = strings.TrimSpace(val)
		if id == "" || val == "" {
			continue
		}
		segments = append(segments, id+"="+val)
	}
	if len(segments) == 0 {
		return channelID + "[]"
	}
	return channelID + "[" + strings.Join(segments, ",") + "]"
}

func enrichCursorVariantDisplayNames(variants []map[string]any, modelID string, modelDisplayName string, multiWindow bool) {
	baseName := strings.TrimSpace(modelDisplayName)
	if baseName == "" {
		baseName = strings.TrimSpace(modelID)
	}
	if baseName == "" {
		return
	}
	for _, variant := range variants {
		formatted := buildCursorSpecVariantDisplayName(baseName, variant, multiWindow)
		if shouldReplaceCursorVariantName(variant["displayName"], modelID) {
			variant["displayName"] = formatted
		}
		if shouldReplaceCursorVariantName(variant["displayNameOutsidePicker"], modelID) {
			variant["displayNameOutsidePicker"] = formatted
		}
	}
}

func shouldReplaceCursorVariantName(raw any, modelID string) bool {
	text, _ := raw.(string)
	trimmed := strings.TrimSpace(text)
	return trimmed == "" || strings.EqualFold(trimmed, strings.TrimSpace(modelID))
}

func buildCursorSpecVariantDisplayName(modelDisplayName string, variant map[string]any, showContext bool) string {
	escapedModel := html.EscapeString(strings.TrimSpace(modelDisplayName))
	values := cursorVariantValues(variant)
	effort := firstNonEmptyVariantEffort(values)
	var parts []string
	hasBrain := false
	if effort != "" && !isDisabledThinkingEffortValue(effort) {
		parts = append(parts, html.EscapeString(cursorSpecEffortLabel(effort)))
		hasBrain = true
	}
	if showContext {
		if rawCtx := strings.TrimSpace(values[modelcontext.CursorContextParameterID]); rawCtx != "" {
			label := strings.ToUpper(rawCtx)
			if tokens := modelcontext.ParseCursorContextValue(rawCtx); tokens > 0 {
				label = formatCursorWindowLabel(tokens)
			}
			parts = append(parts, html.EscapeString(label))
		}
	}
	if strings.EqualFold(values["fast"], "true") || strings.EqualFold(values["speed"], "fast") {
		parts = append(parts, "Fast")
	}
	if len(parts) == 0 {
		return escapedModel
	}
	prefix := ""
	if hasBrain {
		prefix = ":icon-brain: "
	}
	detail := prefix + strings.Join(parts, " · ")
	return escapedModel + ` <span class="ui-model-picker__item-tagline" style="color: var(--cursor-text-secondary); white-space: nowrap;">` + detail + `</span>`
}

func firstNonEmptyVariantEffort(values map[string]string) string {
	for _, key := range []string{"reasoning_effort", "reasoning", "effort"} {
		if val := strings.TrimSpace(values[key]); val != "" {
			return val
		}
	}
	return ""
}

func cursorSpecEffortLabel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none":
		return "None"
	case "minimal":
		return "Minimal"
	case "extra-high", "extra_high", "extrahigh":
		return "Extra High"
	default:
		return thinkingEffortDisplayName(value)
	}
}

func formatCursorWindowLabel(tokens int) string {
	if tokens <= 0 {
		return ""
	}
	if tokens >= 1_000_000 {
		if tokens%1_000_000 == 0 {
			return strconv.Itoa(tokens/1_000_000) + "M"
		}
		text := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", float64(tokens)/1_000_000), "0"), ".")
		return text + "M"
	}
	if tokens >= 1_000 {
		return strconv.Itoa(int(math.Round(float64(tokens)/1_000))) + "K"
	}
	return strconv.Itoa(tokens)
}

func buildCursorSpecTooltipMarkdown(remark string, adapter legacyruntime.ModelAdapterConfig, windows []modelcontext.CursorContextWindow, forMaxMode bool) string {
	if len(windows) <= 1 {
		tokens := 0
		if len(windows) == 1 {
			tokens = windows[0].Tokens
		}
		return buildModelTooltipMarkdown(remark, adapter, tokens)
	}
	if forMaxMode {
		return buildModelTooltipMarkdown(remark, adapter, windows[len(windows)-1].Tokens)
	}
	parts := make([]string, 0, len(windows))
	for _, window := range windows {
		item := formatTokenCount(window.Tokens)
		if window.MaxMode {
			item += " (Max)"
		}
		parts = append(parts, item)
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("**上下文窗口：** %s tokens", strings.Join(parts, " / ")))
	if maxOutput := resolveModelMaxOutputTokens(adapter); maxOutput > 0 {
		lines = append(lines, fmt.Sprintf("**最大输出：** %s tokens", formatTokenCount(maxOutput)))
	}
	if price := resolveAvailableModelDisplayPrice(adapter); price > 0 {
		lines = append(lines, fmt.Sprintf("**输入价格：** %s / 1M tokens", formatModelPrice(price, resolveModelDisplayCurrency(adapter))))
	}
	details := strings.Join(lines, "  \n") + "\n\n"
	if markdown := strings.TrimSpace(remark); markdown != "" {
		return markdown + "\n\n---\n\n" + details
	}
	return details
}