package modelcontext

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CursorContextParameterID is the model parameter Cursor uses to pick a context size.
const CursorContextParameterID = "context"

// CursorContextWindow is one context size that a Cursor account model offers.
type CursorContextWindow struct {
	Tokens int `json:"tokens"`
	// Value is the context parameter value Cursor expects for this window, such as "1m".
	// It is empty for models that have no context parameter.
	Value string `json:"value,omitempty"`
	// MaxMode reports that Cursor serves this window only in max mode.
	MaxMode bool `json:"maxMode,omitempty"`
	// Default marks the window the IDE uses when nothing else is chosen.
	Default bool `json:"default,omitempty"`
}

type cursorModelSpec struct {
	SupportsMaxMode             *bool                  `json:"supportsMaxMode"`
	SupportsNonMaxMode          *bool                  `json:"supportsNonMaxMode"`
	ContextTokenLimit           int                    `json:"contextTokenLimit"`
	ContextTokenLimitForMaxMode int                    `json:"contextTokenLimitForMaxMode"`
	ParameterDefinitions        []cursorSpecDefinition `json:"parameterDefinitions"`
	Variants                    []cursorSpecVariant    `json:"variants"`
	// SyncedWindows is present once a sync has listed the windows, so a stored
	// window is a choice rather than an older local guess.
	SyncedWindows json.RawMessage `json:"contextWindows"`
}

type cursorSpecDefinition struct {
	ID            string `json:"id"`
	ParameterType struct {
		EnumParameter *struct {
			Values []struct {
				Value string `json:"value"`
			} `json:"values"`
		} `json:"enumParameter"`
	} `json:"parameterType"`
}

type cursorSpecVariant struct {
	ParameterValues []struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	} `json:"parameterValues"`
	IsMaxMode             bool `json:"isMaxMode"`
	IsDefaultMaxConfig    bool `json:"isDefaultMaxConfig"`
	IsDefaultNonMaxConfig bool `json:"isDefaultNonMaxConfig"`
}

var cursorContextValuePattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)([kmb])?$`)

// ParseCursorContextValue converts a context parameter value such as "272k" or "1m"
// to tokens with the same rules as the IDE. Unparseable values return 0.
func ParseCursorContextValue(value string) int {
	match := cursorContextValuePattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(match[2]) {
	case "k":
		number *= 1e3
	case "m":
		number *= 1e6
	case "b":
		number *= 1e9
	}
	if number <= 0 || number > math.MaxInt32 {
		return 0
	}
	return int(math.Round(number))
}

// FormatCursorContextValue converts a token count to Cursor's compact context
// parameter notation ("300k", "1m") when exact, or its decimal string otherwise.
func FormatCursorContextValue(tokens int) string {
	if tokens <= 0 {
		return ""
	}
	if tokens%1_000_000 == 0 {
		return strconv.Itoa(tokens/1_000_000) + "m"
	}
	if tokens%1_000 == 0 {
		return strconv.Itoa(tokens/1_000) + "k"
	}
	return strconv.Itoa(tokens)
}

// CursorContextWindows lists the context windows a stored Cursor model spec offers,
// smallest first, with Cursor's default window marked.
func CursorContextWindows(specJSON string) []CursorContextWindow {
	spec, ok := parseCursorModelSpec(specJSON)
	if !ok {
		return nil
	}
	return spec.windows()
}

// ResolveRuntimeCursorContextWindow returns the active Cursor window for a live
// turn. An explicit IDE context parameter wins first (even when another feature
// like Fast mode sets maxMode), followed by maxMode when no context parameter
// was sent, and finally the configured/default window.
func ResolveRuntimeCursorContextWindow(specJSON string, configured int, clientContextValue string, maxMode bool) (CursorContextWindow, bool) {
	spec, ok := parseCursorModelSpec(specJSON)
	if !ok {
		return CursorContextWindow{}, false
	}
	windows := spec.windows()
	if len(windows) == 0 {
		return CursorContextWindow{}, false
	}
	if trimmed := strings.TrimSpace(clientContextValue); trimmed != "" {
		for _, window := range windows {
			if window.Value != "" && strings.EqualFold(window.Value, trimmed) {
				return window, true
			}
		}
		if tokens := ParseCursorContextValue(trimmed); tokens > 0 {
			for _, window := range windows {
				if window.Tokens == tokens {
					return window, true
				}
			}
		}
	}
	if maxMode {
		if variant, ok := spec.defaultVariant(true); ok {
			if tokens := ParseCursorContextValue(variant.parameter(CursorContextParameterID)); tokens > 0 {
				for _, window := range windows {
					if window.Tokens == tokens {
						return window, true
					}
				}
			}
		}
		for index := len(windows) - 1; index >= 0; index-- {
			if windows[index].MaxMode {
				return windows[index], true
			}
		}
		return windows[len(windows)-1], true
	}
	return ResolveCursorContextWindow(specJSON, configured)
}

// ResolveCursorContextWindow returns the window a Cursor account model should use:
// the configured window when Cursor offers it, otherwise Cursor's default window.
// A spec synced before windows were listed holds a local guess, not a choice, so
// its configured value is ignored.
func ResolveCursorContextWindow(specJSON string, configured int) (CursorContextWindow, bool) {
	spec, ok := parseCursorModelSpec(specJSON)
	if !ok {
		return CursorContextWindow{}, false
	}
	windows := spec.windows()
	if len(windows) == 0 {
		return CursorContextWindow{}, false
	}
	if len(spec.SyncedWindows) > 0 && configured > 0 {
		for _, window := range windows {
			if window.Tokens == configured {
				return window, true
			}
		}
	}
	for _, window := range windows {
		if window.Default {
			return window, true
		}
	}
	return windows[0], true
}

// ResolveCursorAccountTokens returns the context window for a Cursor account model.
// Models without Cursor window data fall back to the configured value or the catalog.
func ResolveCursorAccountTokens(modelID, specJSON string, configured int) int {
	if window, ok := ResolveCursorContextWindow(specJSON, configured); ok {
		return window.Tokens
	}
	return Resolve(modelID, configured)
}

// CompactCursorModelSpecJSON strips verbose UI-only tooltips and badges from a
// stored CursorModelSpec while preserving every parameter, variant, label,
// hotkey flag, cost hint, and context window needed by the IDE model picker and
// runtime parameter completion. Already-compact specs are returned unchanged so
// persisted configs and test fixtures stay byte-stable.
func CompactCursorModelSpecJSON(specJSON string) string {
	trimmed := strings.TrimSpace(specJSON)
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, `"markdownTooltip"`) &&
		!strings.Contains(trimmed, `"tooltipData"`) &&
		!strings.Contains(trimmed, `"modelPickerBadges"`) &&
		!strings.Contains(trimmed, `"confirmationDialogue"`) &&
		!strings.Contains(trimmed, `"variantStringRepresentation"`) &&
		!strings.Contains(trimmed, `"legacySlug"`) {
		return trimmed
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(trimmed), &root); err != nil || len(root) == 0 {
		return trimmed
	}
	changed := false
	if defs, ok := root["parameterDefinitions"].([]any); ok {
		for _, rawDef := range defs {
			def, ok := rawDef.(map[string]any)
			if !ok {
				continue
			}
			if _, exists := def["markdownTooltip"]; exists {
				delete(def, "markdownTooltip")
				changed = true
			}
			paramType, _ := def["parameterType"].(map[string]any)
			for _, kind := range []string{"enumParameter", "booleanParameter"} {
				typed, ok := paramType[kind].(map[string]any)
				if !ok {
					continue
				}
				values, ok := typed["values"].([]any)
				if !ok {
					continue
				}
				for _, rawVal := range values {
					val, ok := rawVal.(map[string]any)
					if !ok {
						continue
					}
					for _, dropKey := range []string{
						"markdownTooltip",
						"modelPickerBadges",
						"blockedByAdminAllowlist",
						"defaultBlockedInAdminAllowlist",
						"hideFromUserPickerWhenAdminBlocked",
					} {
						if _, exists := val[dropKey]; exists {
							delete(val, dropKey)
							changed = true
						}
					}
				}
			}
		}
	}
	if variants, ok := root["variants"].([]any); ok {
		for _, rawVariant := range variants {
			variant, ok := rawVariant.(map[string]any)
			if !ok {
				continue
			}
			for _, dropKey := range []string{
				"displayName",
				"displayNameOutsidePicker",
				"tooltipData",
				"tagline",
				"variantStringRepresentation",
				"confirmationDialogue",
				"legacySlug",
			} {
				if _, exists := variant[dropKey]; exists {
					delete(variant, dropKey)
					changed = true
				}
			}
		}
	}
	if !changed {
		return trimmed
	}
	compacted, err := json.Marshal(root)
	if err != nil {
		return trimmed
	}
	return string(compacted)
}

func parseCursorModelSpec(specJSON string) (cursorModelSpec, bool) {
	raw := strings.TrimSpace(specJSON)
	if raw == "" {
		return cursorModelSpec{}, false
	}
	var spec cursorModelSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return cursorModelSpec{}, false
	}
	return spec, true
}

func (spec cursorModelSpec) windows() []CursorContextWindow {
	nonMaxAllowed := spec.SupportsNonMaxMode == nil || *spec.SupportsNonMaxMode
	var windows []CursorContextWindow
	if values := spec.contextValues(); len(values) > 0 {
		for _, value := range values {
			tokens := ParseCursorContextValue(value)
			if tokens <= 0 {
				continue
			}
			windows = append(windows, CursorContextWindow{
				Tokens:  tokens,
				Value:   value,
				MaxMode: !nonMaxAllowed || !spec.offersWithoutMaxMode(value, tokens),
			})
		}
	} else {
		limit := spec.ContextTokenLimit
		maxLimit := spec.ContextTokenLimitForMaxMode
		if maxLimit <= 0 {
			maxLimit = limit
		}
		if nonMaxAllowed && limit > 0 {
			windows = append(windows, CursorContextWindow{Tokens: limit})
		}
		maxAllowed := spec.SupportsMaxMode == nil || *spec.SupportsMaxMode || !nonMaxAllowed
		if maxAllowed && maxLimit > 0 && (len(windows) == 0 || maxLimit > windows[0].Tokens) {
			windows = append(windows, CursorContextWindow{Tokens: maxLimit, MaxMode: true})
		}
	}
	windows = dedupeCursorWindows(windows)
	if len(windows) == 0 {
		return nil
	}
	windows[spec.defaultWindowIndex(windows, nonMaxAllowed)].Default = true
	return windows
}

func (spec cursorModelSpec) contextValues() []string {
	for _, definition := range spec.ParameterDefinitions {
		if !strings.EqualFold(strings.TrimSpace(definition.ID), CursorContextParameterID) || definition.ParameterType.EnumParameter == nil {
			continue
		}
		values := make([]string, 0, len(definition.ParameterType.EnumParameter.Values))
		for _, value := range definition.ParameterType.EnumParameter.Values {
			if trimmed := strings.TrimSpace(value.Value); trimmed != "" {
				values = append(values, trimmed)
			}
		}
		return values
	}
	return nil
}

// offersWithoutMaxMode reports whether any non-max variant uses the context value.
// Without variants for the value, windows up to the non-max limit count as normal.
func (spec cursorModelSpec) offersWithoutMaxMode(value string, tokens int) bool {
	mentioned := false
	for _, variant := range spec.Variants {
		if !strings.EqualFold(variant.parameter(CursorContextParameterID), value) {
			continue
		}
		mentioned = true
		if !variant.IsMaxMode {
			return true
		}
	}
	if mentioned {
		return false
	}
	return spec.ContextTokenLimit <= 0 || tokens <= spec.ContextTokenLimit
}

// defaultWindowIndex mirrors the IDE: the window comes from the default variant's
// context value, else from contextTokenLimit.
func (spec cursorModelSpec) defaultWindowIndex(windows []CursorContextWindow, nonMaxAllowed bool) int {
	if variant, ok := spec.defaultVariant(!nonMaxAllowed); ok {
		if tokens := ParseCursorContextValue(variant.parameter(CursorContextParameterID)); tokens > 0 {
			for index, window := range windows {
				if window.Tokens == tokens {
					return index
				}
			}
		}
	}
	for index, window := range windows {
		if window.MaxMode == !nonMaxAllowed {
			return index
		}
	}
	return 0
}

func (spec cursorModelSpec) defaultVariant(maxMode bool) (cursorSpecVariant, bool) {
	for _, variant := range spec.Variants {
		if (maxMode && variant.IsDefaultMaxConfig) || (!maxMode && variant.IsDefaultNonMaxConfig) {
			return variant, true
		}
	}
	for _, variant := range spec.Variants {
		if variant.IsMaxMode == maxMode {
			return variant, true
		}
	}
	if len(spec.Variants) == 0 {
		return cursorSpecVariant{}, false
	}
	return spec.Variants[0], true
}

func (variant cursorSpecVariant) parameter(id string) string {
	for _, value := range variant.ParameterValues {
		if strings.EqualFold(strings.TrimSpace(value.ID), id) {
			return strings.TrimSpace(value.Value)
		}
	}
	return ""
}

// dedupeCursorWindows keeps one window per size, preferring the one that does not need max mode.
func dedupeCursorWindows(windows []CursorContextWindow) []CursorContextWindow {
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].Tokens < windows[j].Tokens })
	out := windows[:0]
	for _, window := range windows {
		if last := len(out) - 1; last >= 0 && out[last].Tokens == window.Tokens {
			if out[last].MaxMode && !window.MaxMode {
				out[last] = window
			}
			continue
		}
		out = append(out, window)
	}
	return out
}