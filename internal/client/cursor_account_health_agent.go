package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"

	"cursor/gen/agentv1"
	"cursor/gen/aiserverv1"
	modeladapter "cursor/internal/backend/agent/model"
	"cursor/internal/backend/server/upstream"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	// The installed client learns this from GetServerConfig.agent_url_config and
	// falls back to the same host. api2 ChatService is retired and answers every
	// account with ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT.
	defaultCursorAgentHost = "https://agentn.global.api5.cursor.sh"
	cursorBidiAppendPath   = "/aiserver.v1.BidiService/BidiAppend"
	cursorServerConfigPath = "/aiserver.v1.ServerConfigService/GetServerConfig"
	cursorAgentRunPath     = "/agent.v1.AgentService/RunSSE"
)

// cursorBackendRoot is api2, where account, config, and BidiAppend live.
// Tests point it at an httptest server.
var cursorBackendRoot = "https://api2.cursor.sh"

// probeCursorModelCall runs the same agent turn the installed client runs:
// BidiAppend on api2, then AgentService/RunSSE on the server-config agent host.
// A retired ChatService call cannot tell a paid account from a free one.
func probeCursorModelCall(ctx context.Context, authorization, model string) (cursorModelProbeResult, error) {
	host := cursorAgentHost(ctx, authorization)
	requestID := uuid.NewString()
	run := mustProto(&agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{
			RunRequest: &agentv1.AgentRunRequest{
				ConversationState: &agentv1.ConversationStateStructure{
					Mode: agentv1.AgentMode_AGENT_MODE_ASK.Enum(),
				},
				Action: &agentv1.ConversationAction{
					Action: &agentv1.ConversationAction_UserMessageAction{
						UserMessageAction: &agentv1.UserMessageAction{
							UserMessage: &agentv1.UserMessage{
								Text:      "Reply with the single word pong.",
								MessageId: uuid.NewString(),
								Mode:      agentv1.AgentMode_AGENT_MODE_ASK,
							},
						},
					},
				},
				RequestedModel: &agentv1.RequestedModel{ModelId: model},
				ConversationId: proto.String(uuid.NewString()),
			},
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(host, "/")+cursorAgentRunPath,
		bytes.NewReader(connectFrame(mustProto(&aiserverv1.BidiRequestId{RequestId: requestID}))))
	if err != nil {
		return cursorModelProbeResult{}, err
	}
	applyCursorProbeHeaders(req, authorization, "application/connect+proto")
	resp, err := cursorHealthHTTPClient.Do(req)
	if err != nil {
		return cursorModelProbeResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, cursorHealthMaxBytes))
		return probeResultFromErrorText(string(raw)), nil
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = appendCursorProbe(ctx, authorization, requestID, 0, run)
	}()

	var pending []byte
	tmp := make([]byte, 4096)
	seq := int64(1)
	replied := false
	for {
		n, readErr := resp.Body.Read(tmp)
		if n > 0 {
			pending = append(pending, tmp[:n]...)
		}
		for len(pending) >= 5 {
			size := int(binary.BigEndian.Uint32(pending[1:5]))
			if size > cursorHealthMaxBytes {
				return cursorModelProbeResult{}, errString("Cursor sent an oversized stream frame")
			}
			if len(pending) < 5+size {
				break
			}
			flags := pending[0]
			payload := pending[5 : 5+size]
			pending = pending[5+size:]
			if flags&0x02 != 0 {
				if result, ok := errorResultIfPresent(payload); ok {
					return result, nil
				}
				return cursorModelProbeResult{}, errString("Cursor closed the model stream without a response")
			}
			if upstream.IsDeviceLimitError(string(payload)) {
				return cursorModelProbeResult{DeviceLimit: true, Code: "device_limit"}, nil
			}
			msg := &agentv1.AgentServerMessage{}
			if err := proto.Unmarshal(payload, msg); err != nil {
				if result, ok := errorResultIfPresent(payload); ok {
					return result, nil
				}
				continue
			}
			if !replied {
				if exec := msg.GetExecServerMessage(); exec != nil && exec.GetRequestContextArgs() != nil {
					replied = true
					_ = appendCursorProbe(ctx, authorization, requestID, seq, mustProto(requestContextReply(exec.GetId(), exec.GetExecId())))
					seq++
				}
			}
			if agentTurnProducedText(msg) {
				return cursorModelProbeResult{OK: true}, nil
			}
		}
		if readErr != nil {
			if result, ok := errorResultIfPresent(pending); ok {
				return result, nil
			}
			if readErr == io.EOF {
				return cursorModelProbeResult{}, errString("Cursor closed the model stream without a response")
			}
			return cursorModelProbeResult{}, readErr
		}
	}
}

func cursorAgentHost(ctx context.Context, authorization string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cursorBackendRoot, "/")+cursorServerConfigPath,
		bytes.NewReader(mustProto(&aiserverv1.GetServerConfigRequest{})))
	if err != nil {
		return defaultCursorAgentHost
	}
	applyCursorProbeHeaders(req, authorization, "application/proto")
	resp, err := cursorHealthHTTPClient.Do(req)
	if err != nil {
		return defaultCursorAgentHost
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cursorHealthMaxBytes))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return defaultCursorAgentHost
	}
	cfg := &aiserverv1.GetServerConfigResponse{}
	if err := proto.Unmarshal(raw, cfg); err != nil {
		return defaultCursorAgentHost
	}
	host := strings.TrimSpace(cfg.GetAgentUrlConfig().GetAgentnUrl())
	if host == "" {
		host = strings.TrimSpace(cfg.GetAgentUrlConfig().GetAgentUrl())
	}
	if host == "" {
		return defaultCursorAgentHost
	}
	return strings.TrimRight(host, "/")
}

func appendCursorProbe(ctx context.Context, authorization, requestID string, seq int64, payload []byte) error {
	body := mustProto(&aiserverv1.BidiAppendRequest{
		Data:        hex.EncodeToString(payload),
		RequestId:   &aiserverv1.BidiRequestId{RequestId: requestID},
		AppendSeqno: seq,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cursorBackendRoot, "/")+cursorBidiAppendPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	applyCursorProbeHeaders(req, authorization, "application/proto")
	resp, err := cursorHealthHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, cursorHealthMaxBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errString("BidiAppend returned HTTP " + http.StatusText(resp.StatusCode))
	}
	return nil
}

func requestContextReply(id uint32, execID string) *agentv1.AgentClientMessage {
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{
			ExecClientMessage: &agentv1.ExecClientMessage{
				Id:     id,
				ExecId: execID,
				Message: &agentv1.ExecClientMessage_RequestContextResult{
					RequestContextResult: &agentv1.RequestContextResult{
						Result: &agentv1.RequestContextResult_Success{
							Success: &agentv1.RequestContextSuccess{
								RequestContext: modeladapter.CursorHostRequestContext(),
							},
						},
					},
				},
			},
		},
	}
}

func agentTurnProducedText(msg *agentv1.AgentServerMessage) bool {
	update := msg.GetInteractionUpdate()
	if update == nil {
		return false
	}
	if text := update.GetTextDelta(); text != nil && strings.TrimSpace(text.GetText()) != "" {
		return true
	}
	return update.GetTurnEnded() != nil
}

func applyCursorProbeHeaders(req *http.Request, authorization, contentType string) {
	upstream.ApplyCursorClientHeaders(req, authorization, contentType)
}

func probeResultFromErrorText(text string) cursorModelProbeResult {
	result := cursorModelProbeResult{}
	if upstream.IsDeviceLimitError(text) {
		result.DeviceLimit = true
		result.Code = "device_limit"
	}
	if code := extractJSONField(text, `"error":"`); code != "" {
		result.Code = code
	}
	result.Title = extractJSONField(text, `"title":"`)
	result.Action = extractJSONField(text, `"actionRequired":"`)
	result.Detail = extractJSONField(text, `"detail":"`)
	return result
}

func errorResultIfPresent(payload []byte) (cursorModelProbeResult, bool) {
	text := string(payload)
	if !strings.Contains(text, `"error"`) {
		return cursorModelProbeResult{}, false
	}
	result := probeResultFromErrorText(text)
	if result.Code == "" && result.Title == "" && !result.DeviceLimit {
		return cursorModelProbeResult{}, false
	}
	return result, true
}

func mustProto(msg proto.Message) []byte {
	raw, err := proto.Marshal(msg)
	if err != nil {
		panic(err)
	}
	return raw
}

type probeError string

func (e probeError) Error() string { return string(e) }

func errString(message string) error { return probeError(message) }
