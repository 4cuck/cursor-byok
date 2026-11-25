package modeladapter

import (
	"encoding/json"
	"strings"

	"cursor/internal/modelcontext"
)

// cursorRegistryCatalog is the parameter section of a Cursor AvailableModels entry.
// Each model family registers its own parameter ids. Grok 4.7 uses reasoning_effort,
// GPT uses reasoning, and several Claude models use effort. Sending the wrong id
// makes Cursor reject the call.
type cursorRegistryCatalog struct {
	definitions []cursorRegistryDefinition
	variants    []cursorRegistryVariant
}

type cursorRegistryDefinition struct {
	id     string
	values []string
}

type cursorRegistryVariant struct {
	values        map[string]string
	defaultNonMax bool
	defaultMax    bool
	maxMode       bool
}

func parseCursorRegistryCatalog(raw string) (cursorRegistryCatalog, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return cursorRegistryCatalog{}, false
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return cursorRegistryCatalog{}, false
	}
	catalog := cursorRegistryCatalog{
		definitions: parseRegistryDefinitions(spec["parameterDefinitions"]),
		variants:    parseRegistryVariants(spec["variants"]),
	}
	return catalog, true
}

func parseRegistryDefinitions(raw any) []cursorRegistryDefinition {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	definitions := make([]cursorRegistryDefinition, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := strings.TrimSpace(stringMapValue(object, "id"))
		if id == "" {
			continue
		}
		definitions = append(definitions, cursorRegistryDefinition{id: id, values: registryDefinitionValues(object["parameterType"])})
	}
	return definitions
}

func registryDefinitionValues(raw any) []string {
	object, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"enumParameter", "booleanParameter"} {
		parameter, ok := object[key].(map[string]any)
		if !ok {
			continue
		}
		return registryValueList(parameter["values"])
	}
	return nil
}

func registryValueList(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		value := strings.TrimSpace(stringMapValue(object, "value"))
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return values
}

func parseRegistryVariants(raw any) []cursorRegistryVariant {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	variants := make([]cursorRegistryVariant, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		values := map[string]string{}
		if pairs, ok := object["parameterValues"].([]any); ok {
			for _, pair := range pairs {
				entry, ok := pair.(map[string]any)
				if !ok {
					continue
				}
				id := strings.TrimSpace(stringMapValue(entry, "id"))
				value := strings.TrimSpace(stringMapValue(entry, "value"))
				if id == "" || value == "" {
					continue
				}
				values[id] = value
			}
		}
		variants = append(variants, cursorRegistryVariant{
			values:        values,
			defaultNonMax: boolMapValue(object, "isDefaultNonMaxConfig"),
			defaultMax:    boolMapValue(object, "isDefaultMaxConfig"),
			maxMode:       boolMapValue(object, "isMaxMode"),
		})
	}
	return variants
}

func stringMapValue(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func boolMapValue(object map[string]any, key string) bool {
	value, _ := object[key].(bool)
	return value
}

// completeCursorRegistryParameters returns one value for every parameter the
// catalog defines, plus whether that combination is a max-mode variant.
// The selected variant's defaults fill anything the client did not send, and
// an effort alias such as effort is moved onto the id this model registers.
// With a pinned context value, a combination Cursor does not list falls back
// to the closest variant of that context.
func completeCursorRegistryParameters(catalog cursorRegistryCatalog, client []ModelParameter, effort string, fast bool, maxMode bool, pinnedContext string) ([]ModelParameter, bool) {
	if len(catalog.definitions) == 0 {
		return nil, maxMode
	}
	chosen, _ := catalog.defaultVariant(maxMode)
	effortDef, hasEffort := catalog.effortDefinition()
	clientValues := registryClientValues(client)
	completed := make([]ModelParameter, 0, len(catalog.definitions))
	for _, definition := range catalog.definitions {
		value := chosen.values[definition.id]
		if clientValue, ok := clientValues[definition.id]; ok {
			if canonical, allowed := definition.canonical(clientValue); allowed {
				value = canonical
			}
		} else if hasEffort && definition.id == effortDef.id {
			if alias, ok := registryEffortAlias(client, effortDef.id); ok {
				if canonical, allowed := definition.canonical(alias); allowed {
					value = canonical
				}
			} else if canonical, allowed := definition.canonical(effort); allowed && !registryClientSet(client, definition.id) && !registryAliasSet(client, effortDef.id) {
				value = canonical
			}
		}
		if definition.id == "fast" && !registryClientSet(client, "fast") {
			if tierFast, hasClientTier := registryClientServingTierFast(client); hasClientTier {
				want := "false"
				if tierFast {
					want = "true"
				}
				if canonical, allowed := definition.canonical(want); allowed {
					value = canonical
				}
			} else if fast {
				if canonical, allowed := definition.canonical("true"); allowed {
					value = canonical
				}
			}
		}
		if definition.id == "speed" && !registryClientSet(client, "speed") {
			if tierFast, hasClientTier := registryClientServingTierFast(client); hasClientTier {
				want := "default"
				if tierFast {
					want = "fast"
				}
				if canonical, allowed := definition.canonical(want); allowed {
					value = canonical
				}
			} else if fast {
				if canonical, allowed := definition.canonical("fast"); allowed {
					value = canonical
				}
			}
		}
		if value == "" {
			if fallback, ok := definition.firstValue(); ok {
				value = fallback
			}
		}
		if value == "" {
			continue
		}
		completed = append(completed, ModelParameter{ID: definition.id, Value: value})
	}
	if variant, ok := catalog.matchingVariant(completed); ok {
		return completed, variant.maxMode
	}
	if pinnedContext != "" {
		if variant, ok := catalog.closestVariant(completed, pinnedContext, maxMode); ok {
			return catalog.variantParameters(variant, completed), variant.maxMode
		}
	}
	return completed, maxMode
}

// closestVariant returns the variant of the context value that keeps the most
// requested values, preferring the requested mode and then its default.
func (catalog cursorRegistryCatalog) closestVariant(parameters []ModelParameter, contextValue string, maxMode bool) (cursorRegistryVariant, bool) {
	requested := make(map[string]string, len(parameters))
	for _, parameter := range parameters {
		requested[parameter.ID] = parameter.Value
	}
	var chosen cursorRegistryVariant
	best := -1
	for _, variant := range catalog.variants {
		if !strings.EqualFold(variant.values[modelcontext.CursorContextParameterID], contextValue) {
			continue
		}
		score := 0
		for id, value := range variant.values {
			if strings.EqualFold(requested[id], value) {
				score += 4
			}
		}
		if variant.maxMode == maxMode {
			score += 2
		}
		if (maxMode && variant.defaultMax) || (!maxMode && variant.defaultNonMax) {
			score++
		}
		if score > best {
			chosen, best = variant, score
		}
	}
	return chosen, best >= 0
}

func (catalog cursorRegistryCatalog) variantParameters(variant cursorRegistryVariant, fallback []ModelParameter) []ModelParameter {
	values := make(map[string]string, len(fallback))
	for _, parameter := range fallback {
		values[parameter.ID] = parameter.Value
	}
	parameters := make([]ModelParameter, 0, len(catalog.definitions))
	for _, definition := range catalog.definitions {
		value, ok := variant.values[definition.id]
		if !ok {
			value = values[definition.id]
		}
		if value == "" {
			continue
		}
		parameters = append(parameters, ModelParameter{ID: definition.id, Value: value})
	}
	return parameters
}

// withoutCursorContextValue removes any context parameter from parameters.
func withoutCursorContextValue(parameters []ModelParameter) []ModelParameter {
	if len(parameters) == 0 {
		return nil
	}
	filtered := make([]ModelParameter, 0, len(parameters))
	for _, parameter := range parameters {
		if !strings.EqualFold(strings.TrimSpace(parameter.ID), modelcontext.CursorContextParameterID) {
			filtered = append(filtered, parameter)
		}
	}
	return filtered
}

func clientCursorContextValue(parameters []ModelParameter) string {
	for _, parameter := range parameters {
		if strings.EqualFold(strings.TrimSpace(parameter.ID), modelcontext.CursorContextParameterID) {
			return strings.TrimSpace(parameter.Value)
		}
	}
	return ""
}

func matchOfferedCursorContextWindow(specJSON string, rawValue string) (modelcontext.CursorContextWindow, bool) {
	trimmed := strings.TrimSpace(rawValue)
	if trimmed == "" {
		return modelcontext.CursorContextWindow{}, false
	}
	windows := modelcontext.CursorContextWindows(specJSON)
	for _, window := range windows {
		if window.Value != "" && strings.EqualFold(window.Value, trimmed) {
			return window, true
		}
	}
	if tokens := modelcontext.ParseCursorContextValue(trimmed); tokens > 0 {
		for _, window := range windows {
			if window.Tokens == tokens {
				return window, true
			}
		}
	}
	return modelcontext.CursorContextWindow{}, false
}

// withCursorContextValue replaces the client's context value with the target window's value.
func withCursorContextValue(parameters []ModelParameter, value string) []ModelParameter {
	if value == "" {
		return parameters
	}
	pinned := make([]ModelParameter, 0, len(parameters)+1)
	pinned = append(pinned, ModelParameter{ID: modelcontext.CursorContextParameterID, Value: value})
	for _, parameter := range parameters {
		if !strings.EqualFold(strings.TrimSpace(parameter.ID), modelcontext.CursorContextParameterID) {
			pinned = append(pinned, parameter)
		}
	}
	return pinned
}

// defaultVariant returns the variant a request starts from in the given mode.
// Cursor's default for max mode need not be a max-only variant: for several
// models it is the normal default, which keeps fast serving and the larger
// window something the user picks. It is therefore looked up among all
// variants. Without a flagged default the first variant of the mode stands in.
func (catalog cursorRegistryCatalog) defaultVariant(maxMode bool) (cursorRegistryVariant, bool) {
	if maxMode {
		for _, variant := range catalog.variants {
			if variant.defaultMax {
				return variant, true
			}
		}
	}
	pool := catalog.variantsForMode(maxMode)
	for _, variant := range pool {
		if !maxMode && variant.defaultNonMax {
			return variant, true
		}
	}
	if len(pool) == 0 {
		return cursorRegistryVariant{}, false
	}
	return pool[0], true
}

func (catalog cursorRegistryCatalog) variantsForMode(maxMode bool) []cursorRegistryVariant {
	matched := make([]cursorRegistryVariant, 0, len(catalog.variants))
	for _, variant := range catalog.variants {
		if variant.maxMode == maxMode {
			matched = append(matched, variant)
		}
	}
	if len(matched) > 0 {
		return matched
	}
	return catalog.variants
}

func (catalog cursorRegistryCatalog) effortDefinition() (cursorRegistryDefinition, bool) {
	var found cursorRegistryDefinition
	best := 100
	have := false
	for _, definition := range catalog.definitions {
		if isRegistryStructuralParameter(definition.id) || !definition.hasEffortValue() {
			continue
		}
		rank := 50
		switch definition.id {
		case "reasoning_effort":
			rank = 0
		case "reasoning":
			rank = 1
		case "effort":
			rank = 2
		}
		if !have || rank < best {
			found = definition
			have = true
			best = rank
		}
	}
	return found, have
}

func isRegistryStructuralParameter(id string) bool {
	switch id {
	case "context", "fast", "speed", "thinking":
		return true
	default:
		return false
	}
}

// registryClientServingTierFast mirrors Cursor IDE's serving-tier.ts:
// "fast" ("true"/"false") and "speed" ("fast"/"default") are interchangeable
// serving-tier parameters across model families.
func registryClientServingTierFast(parameters []ModelParameter) (bool, bool) {
	for _, parameter := range parameters {
		id := strings.ToLower(strings.TrimSpace(parameter.ID))
		val := strings.ToLower(strings.TrimSpace(parameter.Value))
		switch id {
		case "fast":
			if val == "true" || val == "fast" {
				return true, true
			}
			if val == "false" || val == "default" {
				return false, true
			}
		case "speed":
			if val == "fast" || val == "true" {
				return true, true
			}
			if val == "default" || val == "false" {
				return false, true
			}
		}
	}
	return false, false
}

func (definition cursorRegistryDefinition) hasEffortValue() bool {
	for _, value := range definition.values {
		if isRegistryEffortValue(value) {
			return true
		}
	}
	return false
}

func (definition cursorRegistryDefinition) canonical(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if len(definition.values) == 0 {
		return value, true
	}
	for _, candidate := range registryEffortValueCandidates(value) {
		for _, allowed := range definition.values {
			if strings.EqualFold(allowed, candidate) {
				return allowed, true
			}
		}
	}
	return "", false
}

func registryEffortValueCandidates(value string) []string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "xhigh":
		return []string{value, "extra-high"}
	case "extra-high", "extra_high", "extrahigh":
		return []string{value, "extra-high", "xhigh"}
	case "disabled":
		return []string{value, "none"}
	default:
		return []string{value}
	}
}

func (catalog cursorRegistryCatalog) matchingVariant(parameters []ModelParameter) (cursorRegistryVariant, bool) {
	values := make(map[string]string, len(parameters))
	for _, parameter := range parameters {
		values[parameter.ID] = parameter.Value
	}
	for _, variant := range catalog.variants {
		if len(variant.values) != len(values) {
			continue
		}
		matched := true
		for id, value := range variant.values {
			if !strings.EqualFold(values[id], value) {
				matched = false
				break
			}
		}
		if matched {
			return variant, true
		}
	}
	return cursorRegistryVariant{}, false
}

func (definition cursorRegistryDefinition) firstValue() (string, bool) {
	if len(definition.values) == 0 {
		return "", false
	}
	return definition.values[0], true
}

func isRegistryEffortValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "minimal", "low", "medium", "high", "xhigh", "extra-high", "extra_high", "extrahigh", "max":
		return true
	default:
		return false
	}
}

func isRegistryEffortAlias(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "effort", "reasoning", "reasoning_effort", "thinking_intensity", "anthropic_thinking_effort", "openai_reasoning_effort":
		return true
	default:
		return false
	}
}

func registryClientValues(parameters []ModelParameter) map[string]string {
	values := make(map[string]string, len(parameters))
	for _, parameter := range parameters {
		id := strings.TrimSpace(parameter.ID)
		value := strings.TrimSpace(parameter.Value)
		if id == "" || value == "" {
			continue
		}
		if _, exists := values[id]; exists {
			continue
		}
		values[id] = value
	}
	return values
}

func registryClientSet(parameters []ModelParameter, id string) bool {
	_, ok := registryClientValues(parameters)[id]
	return ok
}

func registryAliasSet(parameters []ModelParameter, specEffortID string) bool {
	_, ok := registryEffortAlias(parameters, specEffortID)
	return ok
}

func registryEffortAlias(parameters []ModelParameter, specEffortID string) (string, bool) {
	for _, parameter := range parameters {
		id := strings.TrimSpace(parameter.ID)
		value := strings.TrimSpace(parameter.Value)
		if id == "" || value == "" || strings.EqualFold(id, specEffortID) || !isRegistryEffortAlias(id) {
			continue
		}
		if isRegistryEffortValue(value) {
			return strings.ToLower(value), true
		}
	}
	return "", false
}