package forwarder

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"cursor/gen/agentv1"
	"cursor/internal/backend/agent/bridge/interaction"
	runtimecore "cursor/internal/backend/agent/core"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type webFixtureTransport struct{ calls int }

func (transport *webFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport.calls++
	body, mediaType := "offline fixture", "text/plain"
	if req.URL.Host != "192.0.2.1" {
		body, mediaType = `<div id="content_left"><div class="c-container"><h3><a href="https://result.invalid/">offline fixture</a></h3></div></div>`, "text/html"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {mediaType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func webCompletionCount(stream *ActiveStream) int {
	n := 0
	for _, event := range stream.Backlog {
		if event.Message.GetInteractionUpdate().GetToolCallCompleted() != nil {
			n++
		}
	}
	return n
}

func assertRestoredWebCard(t *testing.T, stream *ActiveStream, id string, want *agentv1.ToolCall) {
	t.Helper()
	projection, err := NewHistoryProjector().ProjectCheckpointProjection(stream.CheckpointConversation)
	if err != nil {
		t.Fatal(err)
	}
	blobs := map[string][]byte{}
	for _, b := range projection.Blobs {
		blobs[string(b.ID)] = b.Data
	}
	n := 0
	for _, turnID := range projection.State.GetTurns() {
		turn := &agentv1.ConversationTurnStructure{}
		if err := proto.Unmarshal(blobs[string(turnID)], turn); err != nil {
			t.Fatal(err)
		}
		for _, stepID := range turn.GetAgentConversationTurn().GetSteps() {
			step := &agentv1.ConversationStep{}
			if err := proto.Unmarshal(blobs[string(stepID)], step); err != nil {
				t.Fatal(err)
			}
			if card := step.GetToolCall(); card != nil {
				n++
				withID := proto.CloneOf(want)
				withID.ToolCallId = stringPtr(id)
				if !proto.Equal(card, withID) {
					t.Fatalf("restored web card = %v, want %v", card, withID)
				}
			}
		}
	}
	if n != 1 {
		t.Fatalf("restored %d web cards, want 1", n)
	}
}

func TestNativeWebResultWithoutApproval(t *testing.T) {
	for _, raw := range []string{
		`{"callId":"call-alone","toolCall":{"webSearchToolCall":{"args":{"searchTerm":"docs"},"result":{"success":{"references":[{"url":"https://example.invalid/","chunk":"upstream only"}]}}}}}`,
		`{"toolCall":{"webFetchToolCall":{"args":{"toolCallId":"call-alone","url":"https://example.invalid/"},"result":{"error":{"error":"upstream only"}}}}}`,
	} {
		transport := &webFixtureTransport{}
		s := &Service{broker: NewStreamBroker(), projector: NewHistoryProjector(), interactionBridge: interaction.NewBridgeWithHTTPClient(&http.Client{Transport: transport})}
		stream, err := s.broker.OpenStream("req", "conv", 1, "model", "model", agentv1.AgentMode_AGENT_MODE_AGENT, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		setupCheckpointConversation(stream)
		completion := &agentv1.ToolCallCompletedUpdate{}
		if err := protojson.Unmarshal([]byte(raw), completion); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := s.applyProviderModelEvent(stream, modeladapter.ModelEvent{Kind: modeladapter.ModelEventKindCursorWebResult, CursorWebResult: completion}); err != nil {
				t.Fatal(err)
			}
		}
		if webCompletionCount(stream) != 1 || countHistoryByKind(stream, "tool_result") != 1 || pendingBridgeCount(stream) != 0 || stream.ToolInvocationCount != 0 || transport.calls != 0 {
			t.Fatal("standalone completion dispatched, duplicated, or left a pending call")
		}
		for _, e := range stream.Backlog {
			if e.Message.GetInteractionQuery() != nil || e.Message.GetExecServerMessage() != nil {
				t.Fatal("unexpected execution request")
			}
			if c := e.Message.GetInteractionUpdate().GetToolCallCompleted(); c != nil && (c.GetCallId() != "call-alone" || !proto.Equal(c.GetToolCall(), completion.GetToolCall())) {
				t.Fatalf("unexpected card: %v", c)
			}
		}
		assertRestoredWebCard(t, stream, "call-alone", completion.GetToolCall())
		messages, err := s.projector.ProjectPromptReplay(stream.CheckpointConversation)
		if err != nil || len(messages) != 2 || len(messages[0].ToolCalls) != 1 || messages[0].ToolCalls[0].ID != "call-alone" || messages[1].ToolCallID != "call-alone" || !strings.Contains(messages[1].Content, "upstream only") {
			t.Fatalf("replay=%+v err=%v", messages, err)
		}
	}
}

func TestNativeWebMissingResultClosesTurn(t *testing.T) {
	for _, mode := range []string{"stream ended", "approval queue unavailable", "result deadline", "canceled after approval", "canceled before approval"} {
		t.Run(mode, func(t *testing.T) {
			transport := &webFixtureTransport{}
			s := NewService(t.TempDir(), nilResolver{})
			s.interactionBridge = interaction.NewBridgeWithHTTPClient(&http.Client{Transport: transport})
			stream, err := s.broker.OpenStream("req", "conv", 1, "model", "model", agentv1.AgentMode_AGENT_MODE_AGENT, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			setupCheckpointConversation(stream)
			stream.CurrentModelCallID, stream.ProviderPassCount, stream.ProviderActive = "model-call", 1, true
			replies := make(chan *agentv1.InteractionResponse, 2)
			stream.CursorInteractionReplies = replies
			if mode == "approval queue unavailable" {
				stream.CursorInteractionReplies = nil
			}
			query := &agentv1.InteractionQuery{}
			if err := protojson.Unmarshal([]byte(`{"id":88,"webFetchRequestQuery":{"args":{"url":"https://example.invalid/","toolCallId":"call-missing"}}}`), query); err != nil {
				t.Fatal(err)
			}
			raw, err := proto.Marshal(query)
			if err != nil {
				t.Fatal(err)
			}
			args, err := protojson.Marshal(query.GetWebFetchRequestQuery().GetArgs())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.applyProviderModelEvent(stream, modeladapter.ModelEvent{Kind: modeladapter.ModelEventKindToolLikeCompleted, ToolInvocation: &runtimecore.ToolInvocation{ToolName: "WebFetch", CallID: "call-missing", NativeInteraction: raw, ArgsJSON: args}}); err != nil {
				t.Fatal(err)
			}
			if mode != "canceled before approval" {
				reply := &agentv1.InteractionResponse{}
				if err := protojson.Unmarshal([]byte(`{"id":88,"webFetchRequestResponse":{"approved":{}}}`), reply); err != nil {
					t.Fatal(err)
				}
				if err := s.handleInteractionResult(InboundIntent{RequestID: stream.RequestID, InteractionResponse: reply}); err != nil {
					t.Fatal(err)
				}
				if mode != "approval queue unavailable" && (len(replies) != 1 || webCompletionCount(stream) != 0) {
					t.Fatal("approval did not remain nonterminal")
				}
			}
			wantStatus := StreamStatusCanceled
			if mode == "stream ended" {
				wantStatus = StreamStatusCompleted
				if err := s.applyProviderModelEvent(stream, modeladapter.ModelEvent{Kind: modeladapter.ModelEventKindTurnFinished, FinishReason: "stop"}); err != nil {
					t.Fatal(err)
				}
				err = s.handleProviderDoneEvent(stream, &streamProviderEvent{Done: true})
			} else if mode == "approval queue unavailable" {
				// Failure to queue the decision must stop the still-waiting upstream.
				if stream.ProviderActive {
					t.Fatal("upstream still waiting for undeliverable approval")
				}
			} else if mode == "result deadline" {
				id := "call-missing"
				key := providerTimerKey(streamTimerCursorWebResult, id)
				if stream.StreamTimers[key] == nil {
					t.Fatal("approved call has no result deadline")
				}
				err = s.handleTimerEvent(stream, &streamTimerEvent{Kind: streamTimerCursorWebResult, Key: key, ExecID: id, Token: stream.TimerTokens[key]})
			} else {
				err = s.handleCancelIntent(InboundIntent{RequestID: stream.RequestID, CancelReason: "user stopped"})
			}
			if err != nil {
				t.Fatal(err)
			}
			// Reconnectable cards use the standard checkpoint blob acknowledgement;
			// no additional permission or tool result is needed to finish the turn.
			if err := acknowledgePendingCheckpointBlobs(s, stream); err != nil {
				t.Fatal(err)
			}
			if stream.Status != wantStatus || stream.ProviderActive || stream.PendingProviderCompletion != nil || pendingBridgeCount(stream) != 0 || len(stream.approvedCursorWeb) != 0 || webCompletionCount(stream) != 1 || transport.calls != 0 {
				t.Fatalf("not terminal: status=%s phase=%s", stream.Status, stream.Phase)
			}
			if n := countToolResultEntries(t, snapshotHistoryEntries(stream), "call-missing"); n != 1 {
				t.Fatalf("history results=%d", n)
			}
			history := lastToolResultPayload(t, stream)
			if strings.Contains(string(history.ToolCall), `"success"`) || history.ResultText == "" {
				t.Fatalf("fabricated or missing outcome: %+v", history)
			}
			wantLogical := string(wantStatus)
			if mode == "result deadline" || mode == "approval queue unavailable" {
				wantLogical = conversationStatusInterrupted
			}
			if status := deriveRequestLoopStatus(snapshotHistoryEntries(stream), stream.RequestID, stream.TurnSeq, ""); status != wantLogical {
				t.Fatalf("logical status=%q", status)
			}
		})
	}
}

func TestWebInteractionExecutionOwner(t *testing.T) {
	const callID, modelID = "call-web", "model-call-web"
	for _, tc := range []struct {
		tool, query, approval, denial, args string
	}{
		{
			tool:     "WebSearch",
			query:    `{"id":701,"webSearchRequestQuery":{"args":{"searchTerm":"fixture query","toolCallId":"call-web"}}}`,
			approval: `{"id":701,"webSearchRequestResponse":{"approved":{}}}`,
			denial:   `{"id":701,"webSearchRequestResponse":{"rejected":{"reason":"not now"}}}`,
			args:     `{"searchTerm":"fixture query","toolCallId":"call-web"}`,
		},
		{
			tool:     "WebFetch",
			query:    `{"id":702,"webFetchRequestQuery":{"args":{"url":"https://192.0.2.1/","toolCallId":"call-web"}}}`,
			approval: `{"id":702,"webFetchRequestResponse":{"approved":{}}}`,
			denial:   `{"id":702,"webFetchRequestResponse":{"rejected":{"reason":"not now"}}}`,
			args:     `{"url":"https://192.0.2.1/","toolCallId":"call-web"}`,
		},
	} {
		t.Run(tc.tool+"/compatible", func(t *testing.T) {
			transport := &webFixtureTransport{}
			bridge := interaction.NewBridgeWithHTTPClient(&http.Client{Transport: transport})
			query, pending, err := bridge.OpenQuery(runtimecore.ToolInvocation{CallID: callID, ToolName: tc.tool, ArgsJSON: []byte(tc.args)})
			if err != nil {
				t.Fatal(err)
			}
			reply := &agentv1.InteractionResponse{}
			if err := protojson.Unmarshal([]byte(tc.approval), reply); err != nil {
				t.Fatal(err)
			}
			reply.Id = query.GetInteractionQuery().GetId()
			result, err := bridge.ApplyInteractionResponse(reply, pending)
			if err != nil || transport.calls != 1 || !strings.Contains(result.ToolResultPayload, "offline fixture") {
				t.Fatalf("local result = %q, calls = %d, err = %v", result.ToolResultPayload, transport.calls, err)
			}
		})
		for _, decision := range []struct{ name, response string }{{"approved", tc.approval}, {"denied", tc.denial}} {
			t.Run(tc.tool+"/native/"+decision.name, func(t *testing.T) {
				transport := &webFixtureTransport{}
				service := &Service{
					broker:            NewStreamBroker(),
					projector:         NewHistoryProjector(),
					interactionBridge: interaction.NewBridgeWithHTTPClient(&http.Client{Transport: transport}),
				}
				stream, err := service.broker.OpenStream("req-web", "conv-web", 1, "model", "model", agentv1.AgentMode_AGENT_MODE_AGENT, "fixture")
				if err != nil {
					t.Fatal(err)
				}
				setupCheckpointConversation(stream)
				t.Cleanup(func() {
					stream.mu.Lock()
					defer stream.mu.Unlock()
					stopAllStreamTimersLocked(stream)
				})
				stream.CursorInteractionReplies = make(chan *agentv1.InteractionResponse, 2)
				query := &agentv1.InteractionQuery{}
				if err := protojson.Unmarshal([]byte(tc.query), query); err != nil {
					t.Fatal(err)
				}
				raw, err := proto.Marshal(query)
				if err != nil {
					t.Fatal(err)
				}
				invocation := runtimecore.ToolInvocation{ToolName: tc.tool, CallID: callID, ModelCallID: modelID, ArgsJSON: []byte(tc.args), NativeInteraction: raw}
				stream.CurrentModelCallID = modelID
				stream.ProviderActive = true
				if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{Kind: modeladapter.ModelEventKindToolLikeCompleted, ToolInvocation: &invocation}); err != nil {
					t.Fatal(err)
				}
				reply := &agentv1.InteractionResponse{}
				if err := protojson.Unmarshal([]byte(decision.response), reply); err != nil {
					t.Fatal(err)
				}
				// Unknown wire fields must survive forwarding to newer Cursor servers.
				reply.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
				wantReply := proto.Clone(reply)
				for range 2 { // The client may retry the same approval.
					if err := service.handleInteractionResult(InboundIntent{RequestID: stream.RequestID, InteractionResponse: reply}); err != nil {
						t.Fatal(err)
					}
				}
				if transport.calls != 0 || len(stream.CursorInteractionReplies) != 1 {
					t.Fatalf("local HTTP calls = %d, upstream replies = %d; want 0 and 1", transport.calls, len(stream.CursorInteractionReplies))
				}
				if got := <-stream.CursorInteractionReplies; !proto.Equal(got, wantReply) {
					t.Fatalf("upstream reply = %v, want %v", got, wantReply)
				}
				id := strconv.FormatUint(uint64(query.GetId()), 10)
				if !recentlyCompletedInteractionExists(stream, id) || stream.StreamTimers[providerTimerKey(streamTimerInteractionTimeout, id)] != nil || !stream.CursorInlineToolTurn {
					t.Fatal("interaction did not close on the original stream with its timer cleared")
				}
				wantCard := buildStartedToolCall(invocation)
				wantText := "upstream evidence"
				if decision.name == "approved" {
					if countHistoryByKind(stream, "tool_result") != 0 || webCompletionCount(stream) != 0 || pendingBridgeCount(stream) != 0 {
						t.Fatal("approval must release the interaction, not publish a result")
					}
					if tc.tool == "WebSearch" {
						wantCard.GetWebSearchToolCall().Result = &agentv1.WebSearchResult{Result: &agentv1.WebSearchResult_Success{Success: &agentv1.WebSearchSuccess{References: []*agentv1.WebSearchReference{{Url: "https://reference.invalid/", Title: "Page", Chunk: wantText}}}}}
					} else {
						wantCard.GetWebFetchToolCall().Result = &agentv1.WebFetchResult{Result: &agentv1.WebFetchResult_Success{Success: &agentv1.WebFetchSuccess{Url: query.GetWebFetchRequestQuery().GetArgs().GetUrl(), Markdown: wantText}}}
					}
				} else {
					wantText = "not now"
					if tc.tool == "WebSearch" {
						wantCard.GetWebSearchToolCall().Result = &agentv1.WebSearchResult{Result: &agentv1.WebSearchResult_Rejected{Rejected: &agentv1.WebSearchRejected{Reason: wantText}}}
					} else {
						wantCard.GetWebFetchToolCall().Result = &agentv1.WebFetchResult{Result: &agentv1.WebFetchResult_Rejected{Rejected: &agentv1.WebFetchRejected{Reason: wantText}}}
					}
				}
				for range 2 { // Retransmitted server terminal frame, including an echo of a denial.
					if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{Kind: modeladapter.ModelEventKindCursorWebResult,
						CursorWebResult: &agentv1.ToolCallCompletedUpdate{CallId: callID, ToolCall: wantCard}}); err != nil {
						t.Fatal(err)
					}
				}
				if countHistoryByKind(stream, "tool_call") != 1 || countHistoryByKind(stream, "tool_result") != 1 || transport.calls != 0 || stream.ToolInvocationCount != 1 || len(stream.CursorInteractionReplies) != 0 {
					t.Fatal("expected one permission query, one result, no additional execution or replies")
				}
				history := lastToolResultPayload(t, stream)
				gotCard := &agentv1.ToolCall{}
				if err := protojson.Unmarshal(history.ToolCall, gotCard); err != nil || !proto.Equal(gotCard, wantCard) || !strings.Contains(history.ResultText, wantText) {
					t.Fatalf("history card = %v, text = %q, err = %v", gotCard, history.ResultText, err)
				}
				completed := 0
				for _, event := range stream.Backlog {
					if update := event.Message.GetInteractionUpdate().GetToolCallCompleted(); update != nil {
						completed++
						if update.GetCallId() != callID || update.GetModelCallId() != modelID || !proto.Equal(update.GetToolCall(), wantCard) {
							t.Fatalf("client card = %v, want %v for original call", update, wantCard)
						}
					}
				}
				if completed != 1 {
					t.Fatalf("client completion count = %d, want 1", completed)
				}
				assertRestoredWebCard(t, stream, callID, wantCard)
				messages, err := service.projector.ProjectPromptReplay(stream.CheckpointConversation)
				if err != nil || len(messages) == 0 {
					t.Fatalf("missing history replay: %v", err)
				}
				if last := messages[len(messages)-1]; last.Role != "tool" || last.ToolCallID != callID || !strings.Contains(last.Content, wantText) {
					t.Fatalf("replayed decision = %+v, want %q", last, wantText)
				}
			})
		}
	}
}
