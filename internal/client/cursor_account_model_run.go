package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"cursor/gen/agentv1"
	modeladapter "cursor/internal/backend/agent/model"
	serverconfig "cursor/internal/backend/server/config"
)

const cursorAccountTestPrompt = "Reply with the single word ok."

func (s *ProxyService) cursorAccountGateway() modeladapter.AccountModelGateway {
	if s == nil || s.cursorAccount == nil {
		return nil
	}
	return modeladapter.OpenCursorAccountGateway(s.cursorAccount)
}

func (s *ProxyService) runCursorAccountModelTest(adapter serverconfig.ModelAdapterConfig, requestHash string) (ModelAdapterTestResult, error) {
	gateway := s.cursorAccountGateway()
	if gateway == nil {
		return cursorAccountTestFailure(adapter.ID, requestHash, errors.New("sign in to a Cursor account first")), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelAdapterTestTimeout)
	defer cancel()
	startedAt := time.Now().UTC()
	metrics, err := collectCursorAccountTest(ctx, gateway, adapter)
	if err != nil {
		return cursorAccountTestFailure(adapter.ID, requestHash, err), nil
	}
	result, ok := buildSuccessfulModelAdapterTestResult(adapter.ID, requestHash, startedAt, metrics)
	if !ok {
		return cursorAccountTestFailure(adapter.ID, requestHash, errors.New("Cursor returned no text for this model")), nil
	}
	return result, nil
}

func (s *ProxyService) probeCursorAccountModel(adapter serverconfig.ModelAdapterConfig) ModelAdapterProbeResult {
	id := strings.TrimSpace(adapter.ID)
	if id == "" {
		id = strings.TrimSpace(adapter.ModelID)
	}
	result := ModelAdapterProbeResult{ID: id, ModelID: strings.TrimSpace(adapter.ModelID)}
	gateway := s.cursorAccountGateway()
	if gateway == nil {
		result.Message = "Sign in to a Cursor account first"
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelAdapterProbeTimeout)
	defer cancel()
	metrics, err := collectCursorAccountTest(ctx, gateway, adapter)
	if err != nil {
		result.Message = readableCursorAccountError(err)
		result.RawResponse = strings.TrimSpace(err.Error())
		return result
	}
	if metrics == nil || metrics.firstTextTokenAt.IsZero() {
		result.Message = "Cursor returned no text for this model"
		return result
	}
	result.OK = true
	result.Status = 200
	return result
}

func collectCursorAccountTest(ctx context.Context, gateway modeladapter.AccountModelGateway, adapter serverconfig.ModelAdapterConfig) (*modelAdapterTestMetrics, error) {
	metrics := &modelAdapterTestMetrics{}
	err := gateway.Stream(ctx, modeladapter.StreamRequest{
		RequestID:       "cursor-account-model-test",
		RunID:           "cursor-account-model-test",
		ModelCallID:     "cursor-account-model-test",
		ConversationID:  strings.TrimSpace(adapter.CursorAccountID),
		Mode:            agentv1.AgentMode_AGENT_MODE_ASK,
		ModelID:         strings.TrimSpace(adapter.ID),
		ProviderModelID: strings.TrimSpace(adapter.ModelID),
		GroupName:       strings.TrimSpace(adapter.GroupName),
		CursorAccountID: strings.TrimSpace(adapter.CursorAccountID),
		// 账号没有该模型权限时，上游可能一直不回包。测试不等满整个超时，
		// 没有正文或明确报错就按「没有权限」结束。
		SilentTimeout: 12 * time.Second,
		ModelSource:   serverconfigSource(adapter),
		Messages:      []modeladapter.Message{{Role: "user", Content: cursorAccountTestPrompt}},
		Stream:        true,
	}, func(event modeladapter.ModelEvent) error {
		now := time.Now().UTC()
		switch event.Kind {
		case modeladapter.ModelEventKindTextDelta:
			if metrics.firstResponseAt.IsZero() {
				metrics.firstResponseAt = now
			}
			if strings.TrimSpace(event.Text) != "" && metrics.firstTextTokenAt.IsZero() {
				metrics.firstTextTokenAt = now
			}
			metrics.text.WriteString(event.Text)
			metrics.rawResponse += event.Text
		case modeladapter.ModelEventKindThinkingDelta:
			if metrics.firstResponseAt.IsZero() {
				metrics.firstResponseAt = now
			}
		case modeladapter.ModelEventKindTurnFinished:
			metrics.finishedAt = now
			metrics.outputTokens = event.OutputTokens
			metrics.reasoningTokens = event.ReasoningTokens
			metrics.outputProvided = event.UsagePresent && event.OutputTokens > 0
		case modeladapter.ModelEventKindProviderError:
			if event.Err != nil {
				return event.Err
			}
			return errors.New("cursor account model returned an error")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if metrics.finishedAt.IsZero() {
		metrics.finishedAt = time.Now().UTC()
	}
	return metrics, nil
}

func cursorAccountTestFailure(adapterID, requestHash string, err error) ModelAdapterTestResult {
	message := readableCursorAccountError(err)
	return ModelAdapterTestResult{
		AdapterID:   strings.TrimSpace(adapterID),
		RequestHash: strings.TrimSpace(requestHash),
		Status:      string(ModelAdapterTestStatusError),
		SummaryText: message,
		Error:       message,
		RawResponse: strings.TrimSpace(err.Error()),
		TestedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func readableCursorAccountError(err error) string {
	if err == nil {
		return "Cursor account model test failed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Cursor account model test timed out"
	}
	message := strings.TrimSpace(err.Error())
	if strings.Contains(strings.ToLower(message), "does not have access to") {
		return message
	}
	if message == "" {
		return "Cursor account model test failed"
	}
	if len(message) > 240 {
		return message[:240]
	}
	return message
}

func serverconfigSource(adapter serverconfig.ModelAdapterConfig) string {
	return strings.TrimSpace(adapter.Source)
}
