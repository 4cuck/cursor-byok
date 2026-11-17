package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"cursor/gen/aiserverv1"
	"cursor/internal/backend/server/upstream"

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

	body, err := proto.Marshal(&aiserverv1.AvailableModelsRequest{})
	if err != nil {
		return CursorAccountModelCatalog{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorModelsEndpoint, bytes.NewReader(body))
	if err != nil {
		return CursorAccountModelCatalog{}, err
	}
	req.Header.Set("authorization", authorization)
	req.Header.Set("x-cursor-checksum", upstream.BuildCursorChecksum(authorization))
	req.Header.Set("content-type", "application/proto")
	req.Header.Set("accept", "application/proto")
	req.Header.Set("connect-protocol-version", "1")

	resp, err := cursorModelsHTTPClient.Do(req)
	if err != nil {
		return CursorAccountModelCatalog{}, fmt.Errorf("request Cursor model catalog: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, cursorModelsMaxRespBytes))
	if err != nil {
		return CursorAccountModelCatalog{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CursorAccountModelCatalog{}, fmt.Errorf("Cursor model catalog returned HTTP %d", resp.StatusCode)
	}

	parsed := &aiserverv1.AvailableModelsResponse{}
	if err := proto.Unmarshal(payload, parsed); err != nil {
		return CursorAccountModelCatalog{}, fmt.Errorf("decode Cursor model catalog: %w", err)
	}

	catalog := CursorAccountModelCatalog{Models: []CursorAccountModel{}}
	seen := map[string]struct{}{}
	for _, model := range parsed.GetModels() {
		name := strings.TrimSpace(model.GetName())
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		displayName := name
		if tooltip := model.GetTooltipData(); tooltip != nil {
			if title := strings.TrimSpace(tooltip.GetMarkdownContent()); title != "" {
				displayName = name
			}
		}
		catalog.Models = append(catalog.Models, CursorAccountModel{
			Name:        name,
			DisplayName: displayName,
			DefaultOn:   model.GetDefaultOn(),
		})
	}
	sort.Slice(catalog.Models, func(i, j int) bool {
		return catalog.Models[i].Name < catalog.Models[j].Name
	})
	return catalog, nil
}

var cursorModelsHTTPClient = &http.Client{Timeout: cursorModelsTimeout}