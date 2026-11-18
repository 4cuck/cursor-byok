package forwarder

import (
	"strings"
	"testing"

	modeladapter "cursor/internal/backend/agent/model"
)

func TestCollapsePrefixChainedReplayReasoningKeepsLongestOnce(t *testing.T) {
	base := strings.Repeat("thinking about the shared tree. ", 20)
	longer := base + "then I checked the next file."
	longest := longer + " then I launched the subagents."
	messages := []modeladapter.Message{
		{Role: "assistant", ReasoningContent: base, ReasoningSignature: "sig-a", ToolCalls: []modeladapter.ToolCallDescriptor{{ID: "1"}}},
		{Role: "tool", ToolCallID: "1", Content: "ok"},
		{Role: "assistant", ReasoningContent: longer, ReasoningSignature: "sig-b", ToolCalls: []modeladapter.ToolCallDescriptor{{ID: "2"}}},
		{Role: "tool", ToolCallID: "2", Content: "ok"},
		{Role: "assistant", ReasoningContent: longest, ReasoningSignature: "sig-c", ToolCalls: []modeladapter.ToolCallDescriptor{{ID: "3"}}},
		{Role: "tool", ToolCallID: "3", Content: "ok"},
		{Role: "assistant", ReasoningContent: longest, ReasoningSignature: "sig-dup", ToolCalls: []modeladapter.ToolCallDescriptor{{ID: "4"}}},
	}

	collapsed := collapsePrefixChainedReplayReasoning(messages)
	keptContent := make([]string, 0)
	keptSignatures := make([]string, 0)
	for _, message := range collapsed {
		if strings.TrimSpace(message.ReasoningContent) == "" {
			continue
		}
		keptContent = append(keptContent, message.ReasoningContent)
		keptSignatures = append(keptSignatures, message.ReasoningSignature)
	}
	if len(keptContent) != 1 || keptContent[0] != longest || len(keptSignatures) != 1 || keptSignatures[0] != "sig-c" {
		t.Fatalf("kept reasoning = %#v signatures=%#v", keptContent, keptSignatures)
	}
	if collapsed[0].ReasoningContent != "" || collapsed[0].ReasoningSignature != "" {
		t.Fatalf("prefix copy was kept: %#v", collapsed[0])
	}
	if collapsed[6].ReasoningContent != "" || collapsed[6].ReasoningSignature != "" {
		t.Fatalf("duplicate copy was kept: %#v", collapsed[6])
	}
}

func TestCollapsePrefixChainedReplayReasoningKeepsDistinctThoughts(t *testing.T) {
	messages := []modeladapter.Message{
		{Role: "assistant", ReasoningContent: strings.Repeat("alpha ", 80), ReasoningSignature: "a"},
		{Role: "assistant", ReasoningContent: strings.Repeat("beta ", 80), ReasoningSignature: "b"},
	}

	collapsed := collapsePrefixChainedReplayReasoning(messages)
	if collapsed[0].ReasoningContent == "" || collapsed[0].ReasoningSignature != "a" {
		t.Fatalf("first thought = %#v", collapsed[0])
	}
	if collapsed[1].ReasoningContent == "" || collapsed[1].ReasoningSignature != "b" {
		t.Fatalf("second thought = %#v", collapsed[1])
	}
}

func TestCollapsePrefixChainedReplayReasoningIgnoresShortSharedOpening(t *testing.T) {
	opening := "I will look. "
	messages := []modeladapter.Message{
		{Role: "assistant", ReasoningContent: opening, ReasoningSignature: "short"},
		{Role: "assistant", ReasoningContent: opening + strings.Repeat("detail ", 80), ReasoningSignature: "long"},
	}

	collapsed := collapsePrefixChainedReplayReasoning(messages)
	if collapsed[0].ReasoningSignature != "short" || collapsed[1].ReasoningSignature != "long" {
		t.Fatalf("short opening was treated as a duplicated buffer: %#v %#v", collapsed[0], collapsed[1])
	}
}

func TestMergeReplayReasoningKeepsExtendedPrefixAndSignature(t *testing.T) {
	base := strings.Repeat("same thoughts. ", 30)
	extended := base + "new step."
	target := modeladapter.Message{ReasoningContent: base, ReasoningSignature: "old", ReasoningSignatureSource: "anthropic"}
	incoming := modeladapter.Message{ReasoningContent: extended, ReasoningSignature: "new", ReasoningSignatureSource: "anthropic"}

	mergeReplayReasoningOnto(&target, incoming)
	if target.ReasoningContent != extended {
		t.Fatalf("content = %q", target.ReasoningContent)
	}
	if target.ReasoningSignature != "new" || target.ReasoningSignatureSource != "anthropic" {
		t.Fatalf("signature = %q source=%q", target.ReasoningSignature, target.ReasoningSignatureSource)
	}
}

func TestMergeReplayReasoningConcatenatesDistinctThoughts(t *testing.T) {
	left := "first thought"
	right := "second thought"
	if got, want := mergeReplayReasoning(left, right), left+"\n\n"+right; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestNormalizeReplayDropsPrefixChainedReasoningBetweenToolResults(t *testing.T) {
	base := strings.Repeat("thinking about the shared tree. ", 20)
	longer := base + "checked the caller."
	messages := normalizeReplayMessageSequence([]modeladapter.Message{
		assistantToolReplay("tool-1", "Read", base, "sig-a"),
		{Role: "tool", Name: "Read", ToolCallID: "tool-1", Content: "file one"},
		assistantToolReplay("tool-2", "Read", longer, "sig-b"),
		{Role: "tool", Name: "Read", ToolCallID: "tool-2", Content: "file two"},
	})

	kept := make([]string, 0)
	for _, message := range messages {
		if strings.TrimSpace(message.Role) != "assistant" {
			continue
		}
		if text := strings.TrimSpace(message.ReasoningContent); text != "" {
			kept = append(kept, text)
			if message.ReasoningSignature != "sig-b" {
				t.Fatalf("surviving signature = %q", message.ReasoningSignature)
			}
		}
	}
	if len(kept) != 1 || kept[0] != longer {
		t.Fatalf("replayed reasoning = %#v", kept)
	}
}

func assistantToolReplay(id string, name string, reasoning string, signature string) modeladapter.Message {
	return modeladapter.Message{
		Role:               "assistant",
		ReasoningContent:   reasoning,
		ReasoningSignature: signature,
		ToolCalls: []modeladapter.ToolCallDescriptor{{
			ID:   id,
			Type: "function",
			Function: modeladapter.ToolCallFunctionShape{
				Name:      name,
				Arguments: "{}",
			},
		}},
	}
}
