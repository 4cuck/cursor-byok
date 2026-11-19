package modeladapter

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	mu         sync.Mutex
	states     map[string]*agentv1.ConversationStateStructure
	turnStates map[string]map[int64]*agentv1.ConversationStateStructure
	blobs      map[string][]byte
}

const cursorAccountMaxTurnSnapshots = 20

var cursorAgentMemory = &cursorAccountMemory{
	states:     map[string]*agentv1.ConversationStateStructure{},
	turnStates: map[string]map[int64]*agentv1.ConversationStateStructure{},
	blobs:      map[string][]byte{},
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

// HasCursorAccountState reports whether a non-empty upstream Cursor checkpoint
// exists for conversationID. Header-only pre-turn bootstrap stubs (0 turns and
// only system/<user_info> blobs) do not count as saved conversation memory.
func HasCursorAccountState(conversationID string) bool {
	state := cursorAccountLoadState(conversationID)
	return cursorAccountStateHasConversation(state)
}

// CursorAccountUsedTokens returns the upstream token count stored in the saved Cursor checkpoint.
func CursorAccountUsedTokens(conversationID string) uint32 {
	state := cursorAccountLoadState(conversationID)
	if state == nil || state.GetTokenDetails() == nil {
		return 0
	}
	return state.GetTokenDetails().GetUsedTokens()
}

// CursorAccountTokenDetails returns a clone of the upstream token details stored in the saved Cursor checkpoint.
func CursorAccountTokenDetails(conversationID string) *agentv1.ConversationTokenDetails {
	state := cursorAccountLoadState(conversationID)
	if state == nil || state.GetTokenDetails() == nil {
		return nil
	}
	cloned, _ := proto.Clone(state.GetTokenDetails()).(*agentv1.ConversationTokenDetails)
	return cloned
}

// ClearCursorAccountState deletes the cached and persisted upstream Cursor checkpoint for conversationID.
func ClearCursorAccountState(conversationID string) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return
	}
	cursorAgentMemory.mu.Lock()
	delete(cursorAgentMemory.states, conversationID)
	cursorAgentMemory.mu.Unlock()
	if path := cursorAccountStatePath(conversationID); path != "" {
		_ = os.Remove(path)
	}
}

// ClearCursorAccountStateAndSnapshots deletes both the active upstream Cursor
// checkpoint and all per-turn pre-turn snapshots for conversationID. Used when
// canonical history is rewritten (such as compaction) so a future rewind cannot
// restore a pre-compaction upstream checkpoint.
func ClearCursorAccountStateAndSnapshots(conversationID string) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return
	}
	ClearCursorAccountState(conversationID)
	cursorAgentMemory.mu.Lock()
	delete(cursorAgentMemory.turnStates, conversationID)
	cursorAgentMemory.mu.Unlock()
	if dir := cursorAccountTurnStatesDir(conversationID); dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// SnapshotCursorAccountStateBeforeTurn records the current valid upstream
// Cursor checkpoint right before turnSeq begins. If a snapshot for turnSeq
// already exists, it is preserved so repeated retries or model switches on the
// same failed/aborted turn always keep the original pre-turn checkpoint.
func SnapshotCursorAccountStateBeforeTurn(conversationID string, turnSeq int64) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" || turnSeq <= 0 {
		return
	}
	if existing := cursorAccountLoadTurnState(conversationID, turnSeq); cursorAccountStateHasConversation(existing) {
		return
	}
	state := cursorAccountLoadState(conversationID)
	if !cursorAccountStateHasConversation(state) {
		return
	}
	cloned, _ := proto.Clone(state).(*agentv1.ConversationStateStructure)
	if cloned == nil {
		return
	}
	sanitizeCursorAccountState(cloned)
	if !cursorAccountStateHasConversation(cloned) {
		return
	}
	cursorAgentMemory.mu.Lock()
	byTurn := cursorAgentMemory.turnStates[conversationID]
	if byTurn == nil {
		byTurn = map[int64]*agentv1.ConversationStateStructure{}
		cursorAgentMemory.turnStates[conversationID] = byTurn
	}
	byTurn[turnSeq] = cloned
	cursorAgentMemory.mu.Unlock()

	if path := cursorAccountTurnStatePath(conversationID, turnSeq); path != "" {
		if raw, err := proto.Marshal(cloned); err == nil {
			if os.MkdirAll(filepath.Dir(path), 0o755) == nil {
				_ = os.WriteFile(path, raw, 0o600)
			}
		}
	}
	pruneCursorAccountTurnStates(conversationID, cursorAccountMaxTurnSnapshots)
}

// RestoreCursorAccountStateBeforeTurn rewinds the upstream Cursor checkpoint
// to the state recorded right before targetTurnSeq (matching Cursor IDE's
// per-bubble checkpoint restore on retry/edit/model switch). Future turn
// snapshots (> targetTurnSeq) are discarded. If a valid snapshot for
// targetTurnSeq exists, it becomes the active checkpoint (and the snapshot is
// kept for subsequent retries) and true is returned. Otherwise the active
// checkpoint is cleared and false is returned so the next Cursor call rebuilds
// RootPromptMessagesJson from canonical conversation history.
func RestoreCursorAccountStateBeforeTurn(conversationID string, targetTurnSeq int64) bool {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return false
	}
	deleteCursorAccountTurnStatesAfter(conversationID, targetTurnSeq)
	if targetTurnSeq > 0 {
		if snapshot := cursorAccountLoadTurnState(conversationID, targetTurnSeq); cursorAccountStateHasConversation(snapshot) {
			cursorAccountRememberState(conversationID, snapshot)
			return true
		}
	}
	deleteCursorAccountTurnState(conversationID, targetTurnSeq)
	ClearCursorAccountState(conversationID)
	return false
}

func cursorAccountLoadTurnState(conversationID string, turnSeq int64) *agentv1.ConversationStateStructure {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" || turnSeq <= 0 {
		return nil
	}
	cursorAgentMemory.mu.Lock()
	if byTurn := cursorAgentMemory.turnStates[conversationID]; byTurn != nil {
		if state := byTurn[turnSeq]; state != nil {
			cloned, _ := proto.Clone(state).(*agentv1.ConversationStateStructure)
			cursorAgentMemory.mu.Unlock()
			sanitizeCursorAccountState(cloned)
			return cloned
		}
	}
	cursorAgentMemory.mu.Unlock()
	path := cursorAccountTurnStatePath(conversationID, turnSeq)
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
	sanitizeCursorAccountState(state)
	if !cursorAccountStateHasConversation(state) {
		return nil
	}
	cloned, _ := proto.Clone(state).(*agentv1.ConversationStateStructure)
	if cloned == nil {
		return state
	}
	cursorAgentMemory.mu.Lock()
	byTurn := cursorAgentMemory.turnStates[conversationID]
	if byTurn == nil {
		byTurn = map[int64]*agentv1.ConversationStateStructure{}
		cursorAgentMemory.turnStates[conversationID] = byTurn
	}
	byTurn[turnSeq] = cloned
	cursorAgentMemory.mu.Unlock()
	out, _ := proto.Clone(cloned).(*agentv1.ConversationStateStructure)
	if out == nil {
		return cloned
	}
	return out
}

func deleteCursorAccountTurnState(conversationID string, turnSeq int64) {
	if conversationID == "" || turnSeq <= 0 {
		return
	}
	cursorAgentMemory.mu.Lock()
	if byTurn := cursorAgentMemory.turnStates[conversationID]; byTurn != nil {
		delete(byTurn, turnSeq)
		if len(byTurn) == 0 {
			delete(cursorAgentMemory.turnStates, conversationID)
		}
	}
	cursorAgentMemory.mu.Unlock()
	if path := cursorAccountTurnStatePath(conversationID, turnSeq); path != "" {
		_ = os.Remove(path)
	}
}

func deleteCursorAccountTurnStatesAfter(conversationID string, targetTurnSeq int64) {
	if conversationID == "" {
		return
	}
	cursorAgentMemory.mu.Lock()
	if byTurn := cursorAgentMemory.turnStates[conversationID]; byTurn != nil {
		for turn := range byTurn {
			if turn > targetTurnSeq {
				delete(byTurn, turn)
			}
		}
		if len(byTurn) == 0 {
			delete(cursorAgentMemory.turnStates, conversationID)
		}
	}
	cursorAgentMemory.mu.Unlock()
	dir := cursorAccountTurnStatesDir(conversationID)
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if turn, ok := parseCursorAccountTurnStateFilename(entry.Name()); ok && turn > targetTurnSeq {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

func pruneCursorAccountTurnStates(conversationID string, maxKeep int) {
	if conversationID == "" || maxKeep <= 0 {
		return
	}
	turnsSet := map[int64]struct{}{}
	cursorAgentMemory.mu.Lock()
	if byTurn := cursorAgentMemory.turnStates[conversationID]; byTurn != nil {
		for turn := range byTurn {
			turnsSet[turn] = struct{}{}
		}
	}
	cursorAgentMemory.mu.Unlock()
	dir := cursorAccountTurnStatesDir(conversationID)
	if dir != "" {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				if turn, ok := parseCursorAccountTurnStateFilename(entry.Name()); ok {
					turnsSet[turn] = struct{}{}
				}
			}
		}
	}
	if len(turnsSet) <= maxKeep {
		return
	}
	turns := make([]int64, 0, len(turnsSet))
	for turn := range turnsSet {
		turns = append(turns, turn)
	}
	sort.Slice(turns, func(i, j int) bool { return turns[i] < turns[j] })
	for _, turn := range turns[:len(turns)-maxKeep] {
		deleteCursorAccountTurnState(conversationID, turn)
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
	sanitizeCursorAccountState(cloned)
	// Cursor emits an initial header-only checkpoint ([system, <user_info>],
	// turns=0) at the start of a turn before any user/assistant exchange is
	// committed. Never persist that empty bootstrap stub or let it overwrite a
	// completed checkpoint if the stream is canceled or interrupted early.
	if !cursorAccountStateHasConversation(cloned) {
		return
	}
	cursorAgentMemory.mu.Lock()
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

// cursorAccountStateHasConversation reports whether state actually records at
// least one turn, summary, or non-scaffolding conversation message rather than
// only the pre-turn [system, <user_info>] bootstrap header.
func cursorAccountStateHasConversation(state *agentv1.ConversationStateStructure) bool {
	if state == nil {
		return false
	}
	if len(state.GetTurns()) > 0 ||
		len(state.GetSummary()) > 0 ||
		len(state.GetSummaryArchive()) > 0 ||
		len(state.GetSummaryArchives()) > 0 ||
		len(state.GetRecentUserMessageIds()) > 0 ||
		state.GetRecentUserMessageIdsOlderTurnCount() > 0 {
		return true
	}
	for _, id := range state.GetRootPromptMessagesJson() {
		if len(id) == 0 {
			continue
		}
		data, ok := cursorAccountBlob(id)
		if !ok || len(data) == 0 {
			// Unknown/unbacked root ID (such as a unit-test fixture): treat as non-empty.
			return true
		}
		role, empty, isUserInfo, isSummary, parsed := inspectCursorRootMessageBlob(data)
		if !parsed {
			return true
		}
		if empty {
			continue
		}
		if isSummary {
			return true
		}
		switch role {
		case "system":
			continue
		case "user":
			if !isUserInfo {
				return true
			}
		case "assistant", "tool":
			return true
		default:
			return true
		}
	}
	return false
}

// cursorAccountStateCoversHistory reports whether state is a non-empty
// checkpoint rather than a pre-turn [system, <user_info>] bootstrap stub.
func cursorAccountStateCoversHistory(state *agentv1.ConversationStateStructure, _ []*agentv1.ConversationHistoryMessage) bool {
	return cursorAccountStateHasConversation(state)
}

type cursorRootBlobMeta struct {
	id         []byte
	role       string
	empty      bool
	isUserInfo bool
	isSummary  bool
}

// sanitizeCursorAccountState repairs checkpoints whose root_prompt_messages_json
// was polluted by raw KV SetBlob writes (foreign system prompts, empty assistant
// placeholders, duplicate <user_info> headers, or pre-compaction messages
// re-appended ahead of a newer <summary> block).
func sanitizeCursorAccountState(state *agentv1.ConversationStateStructure) bool {
	if state == nil || len(state.RootPromptMessagesJson) == 0 {
		return false
	}
	cleaned, changed := sanitizeCursorAccountRootMessages(state.RootPromptMessagesJson)
	if changed {
		state.RootPromptMessagesJson = cleaned
	}
	return changed
}

func sanitizeCursorAccountRootMessages(ids [][]byte) ([][]byte, bool) {
	if len(ids) == 0 {
		return ids, false
	}
	kept := make([]cursorRootBlobMeta, 0, len(ids))
	changed := false
	seen := make(map[string]struct{}, len(ids))
	for index, id := range ids {
		if len(id) == 0 {
			changed = true
			continue
		}
		hexID := hex.EncodeToString(id)
		meta := cursorRootBlobMeta{id: append([]byte(nil), id...)}
		if data, ok := cursorAccountBlob(id); ok && len(data) > 0 {
			role, empty, isUserInfo, isSummary, parsed := inspectCursorRootMessageBlob(data)
			if parsed {
				meta.role = role
				meta.empty = empty
				meta.isUserInfo = isUserInfo
				meta.isSummary = isSummary
				if index > 0 && role == "system" {
					changed = true
					continue
				}
				if role == "assistant" && empty {
					changed = true
					continue
				}
				if len(kept) > 1 && role == "user" && isUserInfo {
					changed = true
					continue
				}
			}
		}
		if _, dup := seen[hexID]; dup && (meta.role == "system" || meta.isUserInfo || meta.isSummary) {
			changed = true
			continue
		}
		seen[hexID] = struct{}{}
		kept = append(kept, meta)
	}
	latestSummaryIdx := -1
	for index := len(kept) - 1; index > 0; index-- {
		if kept[index].isSummary {
			latestSummaryIdx = index
			break
		}
	}
	if latestSummaryIdx > 0 {
		headerEnd := 0
		if headerEnd < latestSummaryIdx && kept[headerEnd].role == "system" {
			headerEnd++
		}
		if headerEnd < latestSummaryIdx && kept[headerEnd].role == "user" && kept[headerEnd].isUserInfo {
			headerEnd++
		}
		if headerEnd < latestSummaryIdx {
			collapsed := make([]cursorRootBlobMeta, 0, headerEnd+(len(kept)-latestSummaryIdx))
			collapsed = append(collapsed, kept[:headerEnd]...)
			collapsed = append(collapsed, kept[latestSummaryIdx:]...)
			kept = collapsed
			changed = true
		}
	}
	if !changed {
		return ids, false
	}
	out := make([][]byte, 0, len(kept))
	for _, item := range kept {
		out = append(out, item.id)
	}
	return out, true
}

func inspectCursorRootMessageBlob(data []byte) (role string, empty bool, isUserInfo bool, isSummary bool, ok bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false, false, false, false
	}
	var envelope struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return "", false, false, false, false
	}
	role = strings.ToLower(strings.TrimSpace(envelope.Role))
	if role == "" {
		return "", false, false, false, false
	}
	rawContent := bytes.TrimSpace(envelope.Content)
	if len(rawContent) == 0 || bytes.Equal(rawContent, []byte("null")) || bytes.Equal(rawContent, []byte("[]")) || bytes.Equal(rawContent, []byte(`""`)) {
		return role, true, false, false, true
	}
	var text string
	if err := json.Unmarshal(rawContent, &text); err == nil {
		trimmedText := strings.TrimSpace(sanitizeCursorAccountTextDelta(text))
		if trimmedText == "" {
			return role, true, false, false, true
		}
		isUserInfo = isCursorBootstrapScaffoldingText(trimmedText)
		isSummary = role == "user" && strings.HasPrefix(trimmedText, "<summary>")
		return role, false, isUserInfo, isSummary, true
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(rawContent, &parts); err == nil {
		if len(parts) == 0 {
			return role, true, false, false, true
		}
		var firstPart struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(parts[0], &firstPart) == nil {
			trimmedText := strings.TrimSpace(sanitizeCursorAccountTextDelta(firstPart.Text))
			isUserInfo = isCursorBootstrapScaffoldingText(trimmedText)
			isSummary = role == "user" && strings.HasPrefix(trimmedText, "<summary>")
			if role == "assistant" && len(parts) == 1 && (firstPart.Type == "" || firstPart.Type == "text") && trimmedText == "" {
				return role, true, false, false, true
			}
		}
		return role, false, isUserInfo, isSummary, true
	}
	return role, false, false, false, true
}

func isCursorBootstrapScaffoldingText(trimmedText string) bool {
	if strings.HasPrefix(trimmedText, "<user_info>") {
		return true
	}
	if strings.HasPrefix(trimmedText, "<system_reminder>") && !strings.Contains(trimmedText, "<user_query>") {
		return true
	}
	return false
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
		sanitizeCursorAccountState(cloned)
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
	cloned, _ := proto.Clone(state).(*agentv1.ConversationStateStructure)
	if cloned == nil {
		return state
	}
	sanitizeCursorAccountState(cloned)
	return cloned
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
	copied := append([]byte(nil), data...)
	cursorAgentMemory.mu.Lock()
	cursorAgentMemory.blobs[key] = copied
	cursorAgentMemory.mu.Unlock()
	return data, true
}

func cursorAccountStatePath(conversationID string) string {
	root := strings.TrimSpace(appdata.HistoryRootPath())
	if root == "" || strings.TrimSpace(conversationID) == "" {
		return ""
	}
	return filepath.Join(root, conversationID, "cursor-agent-state.bin")
}

func cursorAccountTurnStatesDir(conversationID string) string {
	root := strings.TrimSpace(appdata.HistoryRootPath())
	if root == "" || strings.TrimSpace(conversationID) == "" {
		return ""
	}
	return filepath.Join(root, conversationID, "cursor-agent-turn-states")
}

func cursorAccountTurnStatePath(conversationID string, turnSeq int64) string {
	dir := cursorAccountTurnStatesDir(conversationID)
	if dir == "" || turnSeq <= 0 {
		return ""
	}
	return filepath.Join(dir, fmt.Sprintf("before-turn-%d.bin", turnSeq))
}

func parseCursorAccountTurnStateFilename(name string) (int64, bool) {
	if !strings.HasPrefix(name, "before-turn-") || !strings.HasSuffix(name, ".bin") {
		return 0, false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(name, "before-turn-"), ".bin")
	if raw == "" {
		return 0, false
	}
	turn, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || turn <= 0 {
		return 0, false
	}
	return turn, true
}

func cursorAccountBlobPath(key string) string {
	root := strings.TrimSpace(appdata.HistoryRootPath())
	if root == "" || key == "" {
		return ""
	}
	return filepath.Join(root, "cursor-agent-blobs", key)
}
