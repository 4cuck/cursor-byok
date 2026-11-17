package modeladapter

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"cursor/gen/agentv1"
	"cursor/internal/appdata"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The IDE agent does not replay memory by pasting earlier turns into the next
// user message. The server streams conversationCheckpointUpdate, the client
// stores that state and every KV blob, and the next run sends the checkpoint
// back as conversation_state with only the new user message.
type cursorAccountMemory struct {
	mu       sync.Mutex
	states   map[string]*agentv1.ConversationStateStructure
	blobs    map[string][]byte
	messages map[string][][]byte
}

var cursorAgentMemory = &cursorAccountMemory{
	states:   map[string]*agentv1.ConversationStateStructure{},
	blobs:    map[string][]byte{},
	messages: map[string][][]byte{},
}

var cursorAccountFrameNames []string

func cursorAccountResetFrameLog() {
	cursorAgentMemory.mu.Lock()
	cursorAccountFrameNames = nil
	cursorAgentMemory.mu.Unlock()
}

func cursorAccountFrameLog() []string {
	cursorAgentMemory.mu.Lock()
	defer cursorAgentMemory.mu.Unlock()
	return append([]string(nil), cursorAccountFrameNames...)
}

func cursorAccountNoteFrame(name string) {
	cursorAgentMemory.mu.Lock()
	if len(cursorAccountFrameNames) < 80 {
		cursorAccountFrameNames = append(cursorAccountFrameNames, name)
	}
	cursorAgentMemory.mu.Unlock()
}

func cursorAccountServerMessageKind(msg *agentv1.AgentServerMessage) string {
	if msg == nil {
		return "nil"
	}
	switch {
	case msg.GetConversationCheckpointUpdate() != nil:
		return "checkpoint"
	case msg.GetKvServerMessage() != nil:
		if set := msg.GetKvServerMessage().GetSetBlobArgs(); set != nil {
			mark := ""
			if cursorAccountStateFromBlob(set.GetBlobData()) != nil {
				mark = " state"
			}
			id := hex.EncodeToString(set.GetBlobId())
			if len(id) > 12 {
				id = id[:12]
			}
			return fmt.Sprintf("kv-set bytes=%d id=%s%s", len(set.GetBlobData()), id, mark)
		}
		if get := msg.GetKvServerMessage().GetGetBlobArgs(); get != nil {
			rawID := get.GetBlobId()
			id := hex.EncodeToString(rawID)
			if len(id) > 16 {
				id = id[:16]
			}
			_, ok := cursorAccountBlob(rawID)
			return fmt.Sprintf("kv-get len=%d %s hit=%t", len(rawID), id, ok)
		}
		return "kv"
	case msg.GetExecServerMessage() != nil:
		return "exec"
	case msg.GetExecServerControlMessage() != nil:
		return "exec-control"
	case msg.GetInteractionQuery() != nil:
		return "interaction-query"
	case msg.GetInteractionUpdate() != nil:
		update := msg.GetInteractionUpdate()
		switch {
		case update.GetTextDelta() != nil:
			return "text"
		case update.GetThinkingDelta() != nil:
			return "thinking"
		case update.GetTurnEnded() != nil:
			return "turn-ended"
		default:
			return "interaction-other"
		}
	default:
		unknown := len(msg.ProtoReflect().GetUnknown())
		if unknown > 0 {
			return fmt.Sprintf("unknown-bytes=%d", unknown)
		}
		return "empty"
	}
}

func cursorAccountRememberState(conversationID string, state *agentv1.ConversationStateStructure) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" || state == nil {
		return
	}
	cloned, _ := proto.Clone(state).(*agentv1.ConversationStateStructure)
	if cloned == nil {
		return
	}
	cursorAgentMemory.mu.Lock()
	cloned.RootPromptMessagesJson = appendMissingBlobIDs(cloned.RootPromptMessagesJson, cursorAgentMemory.messages[conversationID])
	cursorAgentMemory.states[conversationID] = cloned
	cursorAgentMemory.mu.Unlock()
	raw, err := proto.Marshal(cloned)
	if err != nil {
		return
	}
	path := cursorAccountStatePath(conversationID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o600)
}

func cursorAccountRememberMessageBlob(conversationID string, id []byte) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" || len(id) == 0 {
		return
	}
	copied := append([]byte(nil), id...)
	cursorAgentMemory.mu.Lock()
	cursorAgentMemory.messages[conversationID] = appendMissingBlobIDs(cursorAgentMemory.messages[conversationID], [][]byte{copied})
	if state := cursorAgentMemory.states[conversationID]; state != nil {
		state.RootPromptMessagesJson = appendMissingBlobIDs(state.RootPromptMessagesJson, [][]byte{copied})
	}
	cursorAgentMemory.mu.Unlock()
}

func appendMissingBlobIDs(existing [][]byte, extra [][]byte) [][]byte {
	for _, id := range extra {
		found := false
		for _, current := range existing {
			if bytes.Equal(current, id) {
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, append([]byte(nil), id...))
		}
	}
	return existing
}

func cursorAccountLoadState(conversationID string) *agentv1.ConversationStateStructure {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil
	}
	cursorAgentMemory.mu.Lock()
	if state := cursorAgentMemory.states[conversationID]; state != nil {
		cloned, _ := proto.Clone(state).(*agentv1.ConversationStateStructure)
		cursorAgentMemory.mu.Unlock()
		return cloned
	}
	cursorAgentMemory.mu.Unlock()
	path := cursorAccountStatePath(conversationID)
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil
	}
	state := &agentv1.ConversationStateStructure{}
	if err := proto.Unmarshal(raw, state); err != nil {
		return nil
	}
	cursorAccountRememberState(conversationID, state)
	return state
}

func cursorAccountRememberBlob(id, data []byte) {
	if len(id) == 0 {
		return
	}
	key := hex.EncodeToString(id)
	copied := append([]byte(nil), data...)
	cursorAgentMemory.mu.Lock()
	cursorAgentMemory.blobs[key] = copied
	cursorAgentMemory.mu.Unlock()
	path := cursorAccountBlobPath(key)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, copied, 0o600)
}

func cursorAccountPrintablePrefix(data []byte, limit int) string {
	if len(data) > limit {
		data = data[:limit]
	}
	out := make([]rune, 0, len(data))
	for _, b := range data {
		if b >= 32 && b < 127 {
			out = append(out, rune(b))
		} else {
			out = append(out, '.')
		}
	}
	return string(out)
}

func cursorAccountStateFromBlob(data []byte) *agentv1.ConversationStateStructure {
	state := &agentv1.ConversationStateStructure{}
	if err := proto.Unmarshal(data, state); err != nil {
		return nil
	}
	if state.GetMode() == agentv1.AgentMode_AGENT_MODE_UNSPECIFIED {
		return nil
	}
	if len(state.GetTurns()) == 0 && len(state.GetRootPromptMessagesJson()) == 0 {
		return nil
	}
	for _, root := range state.GetRootPromptMessagesJson() {
		if len(root) != 32 {
			return nil
		}
	}
	return state
}

func cursorAccountBlobSummary() string {
	cursorAgentMemory.mu.Lock()
	defer cursorAgentMemory.mu.Unlock()
	parts := make([]string, 0, len(cursorAgentMemory.blobs))
	for key, data := range cursorAgentMemory.blobs {
		state := &agentv1.ConversationStateStructure{}
		kind := "raw"
		if err := proto.Unmarshal(data, state); err == nil && (len(state.GetTurns()) > 0 || len(state.GetRootPromptMessagesJson()) > 0 || state.GetMode() != agentv1.AgentMode_AGENT_MODE_UNSPECIFIED) {
			var fields []string
			state.ProtoReflect().Range(func(field protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
				fields = append(fields, string(field.Name()))
				return true
			})
			kind = fmt.Sprintf("state turns=%d root=%d mode=%s fields=%s", len(state.GetTurns()), len(state.GetRootPromptMessagesJson()), state.GetMode(), strings.Join(fields, "+"))
			for index, root := range state.GetRootPromptMessagesJson() {
				hexPrefix := hex.EncodeToString(root)
				if len(hexPrefix) > 16 {
					hexPrefix = hexPrefix[:16]
				}
				kind += fmt.Sprintf(" r%d=%d:%s", index, len(root), hexPrefix)
			}
		}
		prefix := cursorAccountPrintablePrefix(data, 60)
		parts = append(parts, fmt.Sprintf("%s bytes=%d %s prefix=%q", key[:min(8, len(key))], len(data), kind, prefix))
	}
	return strings.Join(parts, "; ")
}

func cursorAccountBlob(id []byte) ([]byte, bool) {
	if data, ok := cursorAccountBlobExact(id); ok {
		return data, true
	}
	// GetBlob sends a longer id whose first field is the 32-byte id from SetBlob.
	if len(id) >= 34 && id[0] == 0x0a && id[1] == 0x20 {
		if data, ok := cursorAccountBlobExact(id[2:34]); ok {
			return data, true
		}
	}
	return nil, false
}

func cursorAccountBlobExact(id []byte) ([]byte, bool) {
	if len(id) == 0 {
		return nil, false
	}
	key := hex.EncodeToString(id)
	cursorAgentMemory.mu.Lock()
	if data, ok := cursorAgentMemory.blobs[key]; ok {
		cursorAgentMemory.mu.Unlock()
		return append([]byte(nil), data...), true
	}
	cursorAgentMemory.mu.Unlock()
	path := cursorAccountBlobPath(key)
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	cursorAccountRememberBlob(id, data)
	return data, true
}

func cursorAccountStatePath(conversationID string) string {
	root := strings.TrimSpace(appdata.HistoryRootPath())
	if root == "" || strings.TrimSpace(conversationID) == "" {
		return ""
	}
	return filepath.Join(root, conversationID, "cursor-agent-state.bin")
}

func cursorAccountBlobPath(key string) string {
	root := strings.TrimSpace(appdata.HistoryRootPath())
	if root == "" || key == "" {
		return ""
	}
	return filepath.Join(root, "cursor-agent-blobs", key)
}
