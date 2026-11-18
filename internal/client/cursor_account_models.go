package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"cursor/gen/aiserverv1"
	"cursor/internal/backend/server/upstream"
	"cursor/internal/modelcontext"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	cursorModelsEndpoint     = "https://api2.cursor.sh/aiserver.v1.AiService/AvailableModels"
	cursorModelsMaxRespBytes = 8 << 20
	cursorModelsTimeout      = 20 * time.Second
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
	Models []CursorAccountModel `json:"models"`
	Email  string               `json:"email,omitempty"`
}

// FetchCursorAccountModels lists the models the signed-in Cursor account may use.
//
// This is what lets an imported API-key account act as a BYOK upstream: once the
// model names are known, the frontend can materialize cursor_account adapters so
// the user can pick between the official upstream and third-party suppliers.
func (s *ProxyService) FetchCursorAccountModels() (CursorAccountModelCatalog, error) {
	if s == nil || s.cursorAccount == nil {
		return CursorAccountModelCatalog{}, errors.New("Cursor account service is not initialized")
	}
	if !s.cursorAccount.SignedIn() {
		return CursorAccountModelCatalog{}, errors.New("sign in to a Cursor account first")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cursorModelsTimeout)
	defer cancel()

	authorization, err := s.cursorAccount.Authorization(ctx)
	if err != nil {
		return CursorAccountModelCatalog{}, fmt.Errorf("resolve Cursor account authorization: %w", err)
	}

	parsed, err := requestCursorModelCatalog(ctx, authorization)
	if err != nil {
		return CursorAccountModelCatalog{}, err
	}

	catalog := CursorAccountModelCatalog{Models: collapseCursorCatalogModels(parsed.GetModels())}
	sort.Slice(catalog.Models, func(i, j int) bool {
		return catalog.Models[i].Name < catalog.Models[j].Name
	})
	return catalog, nil
}

var cursorModelsHTTPClient = &http.Client{Timeout: cursorModelsTimeout}

// requestCursorModelCatalog sends the same AvailableModels request as the installed IDE:
// one model per family, with effort, fast, and context as parameters. An empty
// request returns every suffix as its own model.
func requestCursorModelCatalog(ctx context.Context, authorization string) (*aiserverv1.AvailableModelsResponse, error) {
	body, err := proto.Marshal(&aiserverv1.AvailableModelsRequest{
		ExcludeMaxNamedModels: true,
		UseModelParameters:    proto.Bool(true),
		UseReactModelPicker:   proto.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorModelsEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", authorization)
	req.Header.Set("x-cursor-checksum", upstream.BuildCursorChecksum(authorization))
	req.Header.Set("x-cursor-client-type", "ide")
	req.Header.Set("x-cursor-client-version", upstream.CursorClientVersion())
	req.Header.Set("content-type", "application/proto")
	req.Header.Set("accept", "application/proto")
	req.Header.Set("connect-protocol-version", "1")

	resp, err := cursorModelsHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Cursor model catalog: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, cursorModelsMaxRespBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Cursor model catalog returned HTTP %d", resp.StatusCode)
	}
	parsed := &aiserverv1.AvailableModelsResponse{}
	if err := proto.Unmarshal(payload, parsed); err != nil {
		return nil, fmt.Errorf("decode Cursor model catalog: %w", err)
	}
	return parsed, nil
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
	if defs := marshalCursorProtoList(model.GetParameterDefinitions()); len(defs) > 0 {
		spec["parameterDefinitions"] = defs
	}
	if variants := marshalCursorProtoList(model.GetVariants()); len(variants) > 0 {
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