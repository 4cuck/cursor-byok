package forwarder

import (
	"testing"
	"time"

	"cursor/gen/agentv1"
)

// The client sends a heartbeat every few seconds for as long as a run is
// open. It says nothing about the conversation, so it must not become a
// history entry: each one used to be appended to the history (and trigger a
// rewrite of the history file) for nothing.
func TestClientHeartbeatLeavesNoHistoryEntry(t *testing.T) {
	broker := NewStreamBroker()
	stream, err := broker.OpenStream("req-heartbeat", "conv-heartbeat", 1, "m", "m", agentv1.AgentMode_AGENT_MODE_AGENT, "hello")
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	setupCheckpointConversation(stream)
	service := newTestService(broker, func() time.Duration { return time.Second }, nil)

	intent, err := service.decodeInboundIntent("req-heartbeat", &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &agentv1.ClientHeartbeat{}},
	}, "client_heartbeat")
	if err != nil {
		t.Fatalf("decodeInboundIntent() error = %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := service.handleMetadataIntent(intent); err != nil {
			t.Fatalf("handleMetadataIntent(heartbeat) error = %v", err)
		}
	}

	if entries := snapshotHistoryEntries(stream); len(entries) != 0 {
		t.Fatalf("3 heartbeats left %d history entries, want 0", len(entries))
	}
}

// Other client messages on the same path still leave their record.
func TestBackgroundShellActionStillLeavesItsHistoryEntry(t *testing.T) {
	broker := NewStreamBroker()
	stream, err := broker.OpenStream("req-bg-action", "conv-bg-action", 1, "m", "m", agentv1.AgentMode_AGENT_MODE_AGENT, "hello")
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	setupCheckpointConversation(stream)
	service := newTestService(broker, func() time.Duration { return time.Second }, nil)

	intent := InboundIntent{
		Kind:      "metadata",
		RequestID: "req-bg-action",
		ClientMessage: &agentv1.AgentClientMessage{
			Message: &agentv1.AgentClientMessage_ConversationAction{ConversationAction: &agentv1.ConversationAction{
				Action: &agentv1.ConversationAction_BackgroundShellAction{
					BackgroundShellAction: &agentv1.BackgroundShellAction{ToolCallId: "call-backgrounded"},
				},
			}},
		},
	}
	if err := service.handleMetadataIntent(intent); err != nil {
		t.Fatalf("handleMetadataIntent(background shell action) error = %v", err)
	}

	if got := countHistoryByKind(stream, "metadata"); got < 1 {
		t.Fatalf("metadata entries = %d, want the background shell action recorded", got)
	}
}
