package forwarder

import (
	"testing"

	"cursor/gen/agentv1"
	legacyruntime "cursor/internal/runtime"
)

// Usage is kept with input counted apart from cache read and cache write. The
// turn-ended message is Cursor's own, and Cursor's server fills its input with
// the whole prompt. A Cursor account turn must reach the IDE with the figures
// Cursor sent, not with the cached part taken out.

func publishedTurnEnded(t *testing.T, service *Service, stream *ActiveStream, usage turnUsageSnapshot) *agentv1.TurnEndedUpdate {
	t.Helper()
	if err := service.finishSuccessfulTurnAfterCheckpoint(stream, pendingTurnCompletion{RequestID: stream.RequestID, Usage: usage}); err != nil {
		t.Fatalf("finishSuccessfulTurnAfterCheckpoint() error = %v", err)
	}
	events, err := service.broker.ReadFromCursor(stream.RequestID, 0)
	if err != nil {
		t.Fatalf("ReadFromCursor() error = %v", err)
	}
	for _, event := range events {
		if event.Message == nil {
			continue
		}
		if ended := event.Message.GetInteractionUpdate().GetTurnEnded(); ended != nil {
			return ended
		}
	}
	t.Fatal("no turn-ended message was published")
	return nil
}

// One recorded 64-step turn on a 1m window, as usage keeps it.
var recordedLongTurnUsage = turnUsageSnapshot{
	InputTokens:      1_000,
	OutputTokens:     108_300,
	CacheReadTokens:  58_365_000,
	CacheWriteTokens: 905_000,
	UsagePresent:     true,
}

func TestTurnEndedForCursorAccountCarriesTheWholePromptAsCursorSentIt(t *testing.T) {
	service, stream := testCheckpointBlobService(t)
	service.resolver = cursorSpecChannelResolver{channel: &legacyruntime.ResolvedChannel{Source: legacyruntime.ModelSourceCursorAccount}}

	ended := publishedTurnEnded(t, service, stream, recordedLongTurnUsage)

	if got := ended.GetInputTokens(); got != 59_271_000 {
		t.Fatalf("input tokens sent to the IDE = %d, want 59271000, the figure Cursor's server reported", got)
	}
	if ended.GetCacheReadTokens() != 58_365_000 || ended.GetCacheWriteTokens() != 905_000 || ended.GetOutputTokens() != 108_300 {
		t.Fatalf("cache read / cache write / output = %d / %d / %d, want them unchanged", ended.GetCacheReadTokens(), ended.GetCacheWriteTokens(), ended.GetOutputTokens())
	}
}

func TestTurnEndedForOwnKeyModelKeepsItsInputFigure(t *testing.T) {
	service, stream := testCheckpointBlobService(t)
	service.resolver = cursorSpecChannelResolver{channel: &legacyruntime.ResolvedChannel{}}

	ended := publishedTurnEnded(t, service, stream, recordedLongTurnUsage)

	if got := ended.GetInputTokens(); got != 1_000 {
		t.Fatalf("input tokens sent to the IDE = %d, want 1000", got)
	}
}
