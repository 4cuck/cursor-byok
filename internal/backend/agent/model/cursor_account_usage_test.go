package modeladapter

import (
	"bytes"
	"context"
	"testing"

	"cursor/gen/agentv1"

	"google.golang.org/protobuf/proto"
)

// Cursor reports a turn's input_tokens as the whole prompt, cache read and
// cache write included. Every other adapter reports input without them, and
// totals, cache hit rate and cost are all computed on that basis. Passed on
// unchanged, a Cursor account turn counts its cached tokens twice, shows about
// half its real cache hit rate, and is priced as if nothing had been cached.

func cursorAccountTurnFinished(t *testing.T, ended *agentv1.TurnEndedUpdate) ModelEvent {
	t.Helper()
	var input bytes.Buffer
	writeCursorAgentFrame(&input, &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{InteractionUpdate: &agentv1.InteractionUpdate{
			Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: ended},
		}},
	})
	var finished []ModelEvent
	recorder := &appendRecorder{}
	err := readCursorAccountStream(context.Background(), recorder.append, "req", "conv", "model", "cursor", 0, &input, nil, nil, nil, nil, func(event ModelEvent) error {
		if event.Kind == ModelEventKindTurnFinished {
			finished = append(finished, event)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("readCursorAccountStream() error = %v", err)
	}
	if len(finished) != 1 {
		t.Fatalf("turn finished events = %d, want 1", len(finished))
	}
	return finished[0]
}

// The figures are one recorded 64-step turn on a 1m window.
func TestCursorAccountTurnUsageReportsInputWithoutCachedTokens(t *testing.T) {
	finished := cursorAccountTurnFinished(t, &agentv1.TurnEndedUpdate{
		InputTokens:      proto.Int64(59_271_000),
		OutputTokens:     proto.Int64(108_300),
		CacheReadTokens:  proto.Int64(58_365_000),
		CacheWriteTokens: proto.Int64(905_000),
	})

	if finished.InputTokens != 1_000 {
		t.Fatalf("input tokens = %d, want 1000: the 59.27M prompt minus 58.365M read from cache and 0.905M written to it", finished.InputTokens)
	}
	if finished.CacheReadTokens != 58_365_000 || finished.CacheWriteTokens != 905_000 || finished.OutputTokens != 108_300 {
		t.Fatalf("cache read / cache write / output = %d / %d / %d, want them unchanged", finished.CacheReadTokens, finished.CacheWriteTokens, finished.OutputTokens)
	}
	if !finished.UsagePresent {
		t.Fatal("usage must be marked present")
	}
	used := finished.InputTokens + finished.CacheReadTokens + finished.CacheWriteTokens + finished.OutputTokens
	if want := int64(59_271_000 + 108_300); used != want {
		t.Fatalf("the four parts add up to %d, want %d: what the turn actually used", used, want)
	}
}

func TestCursorAccountTurnUsageWithoutCacheKeepsInput(t *testing.T) {
	finished := cursorAccountTurnFinished(t, &agentv1.TurnEndedUpdate{
		InputTokens:  proto.Int64(1200),
		OutputTokens: proto.Int64(25),
	})

	if finished.InputTokens != 1200 || finished.OutputTokens != 25 {
		t.Fatalf("input / output = %d / %d, want 1200 / 25", finished.InputTokens, finished.OutputTokens)
	}
}

// An input smaller than the cache tokens cannot be a total that contains them.
func TestCursorAccountTurnUsageKeepsInputThatCannotContainCacheTokens(t *testing.T) {
	finished := cursorAccountTurnFinished(t, &agentv1.TurnEndedUpdate{
		InputTokens:     proto.Int64(4_000),
		CacheReadTokens: proto.Int64(250_000),
	})

	if finished.InputTokens != 4_000 || finished.CacheReadTokens != 250_000 {
		t.Fatalf("input / cache read = %d / %d, want 4000 / 250000", finished.InputTokens, finished.CacheReadTokens)
	}
}
