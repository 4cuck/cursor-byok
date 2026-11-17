package client

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"cursor/gen/aiserverv1"

	"google.golang.org/protobuf/proto"
)

// Plan gates are the codes the installed client maps to upgrade or payment.
// The retired ChatService name ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT is not one
// of them: Cursor attaches actionRequired=payment to that code even when the
// detail says the client version is unsupported.
func TestIsPlanGateCode(t *testing.T) {
	gated := []string{
		"ERROR_PRO_REQUIRED",
		"ERROR_UPGRADE_REQUIRED",
		"ERROR_PRO_USER_ONLY",
		"ERROR_USAGE_PRICING_REQUIRED",
		"ERROR_FREE_USER_USAGE_LIMIT",
	}
	for _, code := range gated {
		if !isPlanGateCode(code) {
			t.Fatalf("expected %q to be a plan gate", code)
		}
	}

	notGated := []string{
		"",
		"ERROR_NOT_LOGGED_IN",
		"ERROR_CUSTOM_MESSAGE",
		"ERROR_BAD_MODEL_NAME",
		"ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT",
		"ERROR_OUTDATED_CLIENT",
		"STREAM_OK",
	}
	for _, code := range notGated {
		if isPlanGateCode(code) {
			t.Fatalf("%q must not be treated as a plan gate", code)
		}
	}
}

// A device-limit refusal is distinct from a plan gate: one is temporary, the
// other needs a plan change. Mixing them up would put a paying user on a
// pointless 24h cooldown.
func TestRetiredChatErrorIsAVersionGate(t *testing.T) {
	payload := `{"error":{"code":"resource_exhausted","message":"Error","details":[{"debug":{"error":"ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT",` +
		`"details":{"title":"Update Required","detail":"Your version of Cursor is no longer supported. Please update to the latest version at cursor.com/downloads to continue.",` +
		`"analyticsMetadata":{"actionRequired":"payment"}}}}]}}`
	result := probeResultFromErrorText(payload)
	if result.Code != "ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT" {
		t.Fatalf("code = %q", result.Code)
	}
	if result.Action != "payment" {
		t.Fatalf("action = %q", result.Action)
	}
	if !isClientVersionGate(result) {
		t.Fatal("retired chat refusal should be a client-version gate")
	}
	if isPlanRestriction(result) {
		t.Fatal("retired chat refusal must not be reported as a missing plan")
	}
}

func TestAPIKeySessionRejectedDoesNotTreatDeviceLimitAsLogout(t *testing.T) {
	if apiKeySessionRejected(cursorModelProbeResult{DeviceLimit: true, Code: "device_limit", Detail: "Too many computers"}, nil) {
		t.Fatal("device limit must not refresh the api key")
	}
	if !apiKeySessionRejected(cursorModelProbeResult{Code: "ERROR_NOT_LOGGED_IN"}, nil) {
		t.Fatal("not-logged-in should refresh an api key session")
	}
}

func TestUsagePricingIsAPlanRestriction(t *testing.T) {
	result := cursorModelProbeResult{
		Code:   "ERROR_USAGE_PRICING_REQUIRED",
		Action: "payment",
		Title:  "Usage pricing required",
		Detail: "Enable usage-based pricing to continue.",
	}
	if !isPlanRestriction(result) {
		t.Fatal("usage pricing should be a plan restriction")
	}
	if isClientVersionGate(result) {
		t.Fatal("usage pricing must not be a version gate")
	}
}

// extractJSONField must survive the length-prefixed frame layout, where the body
// is not valid standalone JSON.
func TestExtractJSONFieldFromFrames(t *testing.T) {
	frame := "\x02\x00\x00\x01\x40{\"error\":{\"code\":\"resource_exhausted\"," +
		"\"details\":[{\"debug\":{\"error\":\"ERROR_X\",\"details\":{\"title\":\"Update Required\"," +
		"\"analyticsMetadata\":{\"actionRequired\":\"payment\"}}}}]}}"

	if got := extractJSONField(frame, `"error":"`); got != "ERROR_X" {
		t.Fatalf("error = %q", got)
	}
	if got := extractJSONField(frame, `"title":"`); got != "Update Required" {
		t.Fatalf("title = %q", got)
	}
	if got := extractJSONField(frame, `"actionRequired":"`); got != "payment" {
		t.Fatalf("action = %q", got)
	}
	if got := extractJSONField(frame, `"missing":"`); got != "" {
		t.Fatalf("missing field = %q, want empty", got)
	}
}

// A paid account is healthy when the current agent host accepts a turn. The
// retired api2 ChatService call cannot make that distinction.
func TestProbeCursorModelCallAcceptsAgentTurn(t *testing.T) {
	appended := make(chan int64, 4)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cursorAgentRunPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		if !waitAppend(r.Context(), appended, 0) {
			return
		}
		writeAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_ExecServerMessage{
				ExecServerMessage: &agentv1.ExecServerMessage{
					Id:     7,
					ExecId: "exec-1",
					Message: &agentv1.ExecServerMessage_RequestContextArgs{
						RequestContextArgs: &agentv1.RequestContextArgs{},
					},
				},
			},
		})
		if flusher != nil {
			flusher.Flush()
		}
		if !waitAppend(r.Context(), appended, 1) {
			return
		}
		writeAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TextDelta{
						TextDelta: &agentv1.TextDeltaUpdate{Text: "pong"},
					},
				},
			},
		})
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer agent.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cursorServerConfigPath:
			raw := mustProto(&aiserverv1.GetServerConfigResponse{
				AgentUrlConfig: &aiserverv1.AgentUrlConfig{AgentnUrl: agent.URL},
			})
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(raw)
		case cursorBidiAppendPath:
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			msg := &aiserverv1.BidiAppendRequest{}
			if err := proto.Unmarshal(body, msg); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			appended <- msg.GetAppendSeqno()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	previous := cursorBackendRoot
	cursorBackendRoot = api.URL
	t.Cleanup(func() { cursorBackendRoot = previous })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := probeCursorModelCall(ctx, "Bearer test", "default")
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("probe result = %+v, want model text accepted", result)
	}
}

func waitAppend(ctx context.Context, appended <-chan int64, want int64) bool {
	select {
	case seq := <-appended:
		return seq == want
	case <-ctx.Done():
		return false
	case <-time.After(4 * time.Second):
		return false
	}
}

func writeAgentFrame(w http.ResponseWriter, msg proto.Message) {
	payload := mustProto(msg)
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	_, _ = w.Write(append(header, payload...))
}
