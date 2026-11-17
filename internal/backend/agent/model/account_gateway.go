package modeladapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrCursorAccountModelGatewayUnavailable = errors.New("Cursor account model gateway is not configured")

// CursorAccountAuthorizer supplies the signed-in Cursor account bearer token.
// Account models must not reuse a third-party API key.
type CursorAccountAuthorizer interface {
	Authorization(context.Context) (string, error)
}

// AccountModelGateway carries Cursor account model calls. It stays separate from
// the OpenAI, Anthropic, and Gemini adapters so OAuth credentials are never sent
// as a third-party API key.
type AccountModelGateway interface {
	Stream(context.Context, StreamRequest, func(ModelEvent) error) error
}

type unavailableAccountModelGateway struct{}

func (unavailableAccountModelGateway) Stream(_ context.Context, req StreamRequest, _ func(ModelEvent) error) error {
	modelID := strings.TrimSpace(req.ProviderModelID)
	if modelID == "" {
		modelID = strings.TrimSpace(req.ModelID)
	}
	if modelID == "" {
		return ErrCursorAccountModelGatewayUnavailable
	}
	return fmt.Errorf("%w: model %s", ErrCursorAccountModelGatewayUnavailable, modelID)
}

// EnableCursorAccountGateway turns on the verified Cursor agent relay.
func (router *Router) EnableCursorAccountGateway(auth CursorAccountAuthorizer) {
	if router == nil || auth == nil {
		return
	}
	router.accountGateway = newCursorAccountGateway(auth)
}
