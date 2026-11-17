package promptengine

import (
	"strings"
	"testing"

	"cursor/gen/agentv1"
)

func TestBuildUserQueryReplayMessageWrapsUserQuery(t *testing.T) {
	message, ok := BuildUserQueryReplayMessage("fix the failing test")
	if !ok {
		t.Fatal("BuildUserQueryReplayMessage() ok = false")
	}
	if message.Role != "user" {
		t.Fatalf("role = %q, want user", message.Role)
	}
	if !strings.Contains(message.Content, "<user_query>") || !strings.Contains(message.Content, "fix the failing test") {
		t.Fatalf("content = %q, want wrapped user query", message.Content)
	}
}

func TestRequestContextSkillsStayInsideTheProject(t *testing.T) {
	section := buildRequestContextAgentSkillsSection(&agentv1.RequestContext{
		Env: &agentv1.RequestContextEnv{WorkspacePaths: []string{"/root/teapot"}},
		SkillOptions: &agentv1.SkillOptions{
			SkillDescriptors: []*agentv1.SkillDescriptor{
				{
					Name:           "superpowers-brainstorming",
					Description:    "You MUST use this before any creative work.",
					ReadmeFilePath: "/root/.cursor/skills/superpowers-brainstorming/SKILL.md",
				},
				{
					Name:           "find-skills",
					Description:    "Search for a skill that matches the task.",
					ReadmeFilePath: "/root/.cursor/skills/find-skills/SKILL.md",
				},
				{
					Name:           "teapot-review",
					Description:    "Review teapot changes in this repository.",
					ReadmeFilePath: "/root/teapot/.cursor/skills/teapot-review/SKILL.md",
				},
			},
		},
	})
	if strings.Contains(section, "superpowers") || strings.Contains(section, "find-skills") {
		t.Fatalf("user-level skill was injected: %s", section)
	}
	if !strings.Contains(section, "teapot-review") {
		t.Fatalf("project skill missing: %s", section)
	}
}

func TestBuildUserQueryReplayMessageEmpty(t *testing.T) {
	if _, ok := BuildUserQueryReplayMessage("   "); ok {
		t.Fatal("BuildUserQueryReplayMessage() ok = true for blank input")
	}
}

func TestEncodeDecodeReplayMessagesRoundTrip(t *testing.T) {
	original := []Message{
		{
			Role:    "user",
			Content: "<user_query>\nhello\n</user_query>",
		},
		{
			Role: "assistant",
			ToolCalls: []ToolCallDescriptor{{
				ID:   "call-1",
				Type: "function",
				Function: ToolCallFunctionShape{
					Name:      "Read",
					Arguments: `{"path":"main.go"}`,
				},
			}},
			ReasoningContent: "thinking",
		},
		{
			Role:       "tool",
			ToolCallID: "call-1",
			Name:       "Read",
			Content:    "package main",
		},
	}

	encoded, err := EncodeReplayMessages(original)
	if err != nil {
		t.Fatalf("EncodeReplayMessages() error = %v", err)
	}
	decoded, err := DecodeReplayMessages(encoded)
	if err != nil {
		t.Fatalf("DecodeReplayMessages() error = %v", err)
	}
	if len(decoded) != len(original) {
		t.Fatalf("decoded len = %d, want %d", len(decoded), len(original))
	}
	if decoded[1].ToolCalls[0].Function.Name != "Read" {
		t.Fatalf("tool call name = %q, want Read", decoded[1].ToolCalls[0].Function.Name)
	}
	if decoded[2].Content != "package main" {
		t.Fatalf("tool result content = %q", decoded[2].Content)
	}
}

func TestBuildToolCallReplayMessagesAssistantAndTool(t *testing.T) {
	toolCall := &agentv1.ToolCall{
		Tool: &agentv1.ToolCall_ReadToolCall{
			ReadToolCall: &agentv1.ReadToolCall{
				Args: &agentv1.ReadToolArgs{Path: "main.go"},
				Result: &agentv1.ReadToolResult{
					Result: &agentv1.ReadToolResult_Success{
						Success: &agentv1.ReadToolSuccess{
							Output: &agentv1.ReadToolSuccess_Content{Content: "package main"},
						},
					},
				},
			},
		},
	}
	messages, ok := BuildToolCallReplayMessages("call-shell", toolCall)
	if !ok {
		t.Fatal("BuildToolCallReplayMessages() ok = false")
	}
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	if messages[0].Role != "assistant" || len(messages[0].ToolCalls) != 1 {
		t.Fatalf("assistant message = %+v", messages[0])
	}
	if messages[1].Role != "tool" || messages[1].ToolCallID != "call-shell" {
		t.Fatalf("tool message = %+v", messages[1])
	}
}
