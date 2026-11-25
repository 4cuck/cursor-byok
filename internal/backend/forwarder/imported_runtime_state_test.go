package forwarder

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"cursor/gen/agentv1"
)

// A conversation that was started on Cursor's own agent keeps its todos, its
// plan and its summary in the client's blob store. The conversation state it
// sends carries only their 32-byte ids, exactly as it does for turns and
// prompt messages. Parsing an id as the message itself fails, and that error
// used to fail the whole turn: a chat with a todo list could not be continued
// here at all ("decode imported todo: proto: cannot parse invalid wire-format
// data").

func marshalImported(t *testing.T, message proto.Message) []byte {
	t.Helper()
	raw, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("marshal %T: %v", message, err)
	}
	return raw
}

func importedRuntimeState(t *testing.T, entries []HistoryEntry) (runtimeStateEntryPayload, bool) {
	t.Helper()
	for _, entry := range entries {
		if entry.Kind != "runtime_state" {
			continue
		}
		var payload runtimeStateEntryPayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("decode runtime_state entry: %v", err)
		}
		return payload, true
	}
	return runtimeStateEntryPayload{}, false
}

func importedSummary(t *testing.T, entries []HistoryEntry) (string, bool) {
	t.Helper()
	for _, entry := range entries {
		if entry.Kind != "compacted_summary" {
			continue
		}
		var payload compactionSummaryEntryPayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("decode compacted_summary entry: %v", err)
		}
		return payload.Summary, true
	}
	return "", false
}

func TestImportConversationStateReadsTodosKeptInTheBlobStore(t *testing.T) {
	first := marshalImported(t, &agentv1.TodoItem{Id: "await", Content: "Fix AwaitShell", Status: agentv1.TodoStatus_TODO_STATUS_IN_PROGRESS})
	second := marshalImported(t, &agentv1.TodoItem{Id: "verify", Content: "Run the full suite", Status: agentv1.TodoStatus_TODO_STATUS_PENDING})
	stubDiskKVBlobs(t, map[string][]byte{
		diskBlobHex(t, first):  first,
		diskBlobHex(t, second): second,
	})
	state := &agentv1.ConversationStateStructure{
		Todos: [][]byte{diskBlobID(t, first), diskBlobID(t, second)},
	}

	entries, err := (&Service{}).importConversationState(testConversation(nil), state, nil)

	if err != nil {
		t.Fatalf("importConversationState() error = %v; this error fails the whole turn", err)
	}
	runtimeState, ok := importedRuntimeState(t, entries)
	if !ok {
		t.Fatalf("no runtime_state entry was imported: %#v", entries)
	}
	if len(runtimeState.Todos) != 2 {
		t.Fatalf("imported todos = %d, want 2", len(runtimeState.Todos))
	}
	if runtimeState.Todos[0].GetId() != "await" || runtimeState.Todos[0].GetContent() != "Fix AwaitShell" ||
		runtimeState.Todos[0].GetStatus() != agentv1.TodoStatus_TODO_STATUS_IN_PROGRESS {
		t.Fatalf("first todo = %v", runtimeState.Todos[0])
	}
	if runtimeState.Todos[1].GetId() != "verify" {
		t.Fatalf("second todo = %v", runtimeState.Todos[1])
	}
}

// The blobs can also arrive with the request itself.
func TestImportConversationStateReadsTodosFromPrefetchedBlobs(t *testing.T) {
	stubDiskKVBlobs(t, nil)
	todo := marshalImported(t, &agentv1.TodoItem{Id: "only", Content: "From the request"})
	state := &agentv1.ConversationStateStructure{Todos: [][]byte{diskBlobID(t, todo)}}
	prefetched := []*agentv1.PreFetchedBlob{{Id: diskBlobID(t, todo), Value: todo}}

	entries, err := (&Service{}).importConversationState(testConversation(nil), state, prefetched)

	if err != nil {
		t.Fatalf("importConversationState() error = %v", err)
	}
	runtimeState, ok := importedRuntimeState(t, entries)
	if !ok || len(runtimeState.Todos) != 1 || runtimeState.Todos[0].GetContent() != "From the request" {
		t.Fatalf("imported todos = %v (found=%t), want the prefetched one", runtimeState.Todos, ok)
	}
}

// A todo whose blob cannot be found is dropped. The conversation itself is
// still imported: losing a todo list is not a reason to refuse the chat.
func TestImportConversationStateKeepsTheChatWhenATodoBlobIsMissing(t *testing.T) {
	known := marshalImported(t, &agentv1.TodoItem{Id: "kept", Content: "Still here"})
	question := []byte(`{"role":"user","content":"old question"}`)
	answer := []byte(`{"role":"assistant","content":"old answer"}`)
	stubDiskKVBlobs(t, map[string][]byte{
		diskBlobHex(t, known):    known,
		diskBlobHex(t, question): question,
		diskBlobHex(t, answer):   answer,
	})
	missing := sha256.Sum256([]byte("a todo that is not in the blob store"))
	state := &agentv1.ConversationStateStructure{
		RootPromptMessagesJson: [][]byte{diskBlobID(t, question), diskBlobID(t, answer)},
		Todos:                  [][]byte{missing[:], diskBlobID(t, known)},
	}

	entries, err := (&Service{}).importConversationState(testConversation(nil), state, nil)

	if err != nil {
		t.Fatalf("importConversationState() error = %v; a missing todo must not fail the turn", err)
	}
	messages := 0
	for _, entry := range entries {
		if entry.Kind == "model_message" {
			messages++
		}
	}
	if messages != 2 {
		t.Fatalf("imported %d history messages, want the 2 the chat had", messages)
	}
	runtimeState, ok := importedRuntimeState(t, entries)
	if !ok || len(runtimeState.Todos) != 1 || runtimeState.Todos[0].GetId() != "kept" {
		t.Fatalf("imported todos = %v (found=%t), want only the one whose blob exists", runtimeState.Todos, ok)
	}
}

// Older clients, and this backend's own checkpoints, carry the todo inline.
func TestImportConversationStateStillReadsInlineTodos(t *testing.T) {
	stubDiskKVBlobs(t, nil)
	state := &agentv1.ConversationStateStructure{
		Todos: [][]byte{marshalImported(t, &agentv1.TodoItem{Id: "inline", Content: "Carried in the state itself"})},
	}

	entries, err := (&Service{}).importConversationState(testConversation(nil), state, nil)

	if err != nil {
		t.Fatalf("importConversationState() error = %v", err)
	}
	runtimeState, ok := importedRuntimeState(t, entries)
	if !ok || len(runtimeState.Todos) != 1 || runtimeState.Todos[0].GetId() != "inline" {
		t.Fatalf("imported todos = %v (found=%t), want the inline one", runtimeState.Todos, ok)
	}
}

// An inline todo can happen to be exactly as long as a blob id.
func TestImportConversationStateInlineTodoOfBlobIDLengthIsNotMistakenForAnID(t *testing.T) {
	stubDiskKVBlobs(t, nil)
	var inline []byte
	for size := 1; size <= 64; size++ {
		inline = marshalImported(t, &agentv1.TodoItem{Id: "t", Content: strings.Repeat("x", size)})
		if len(inline) == sha256.Size {
			break
		}
	}
	if len(inline) != sha256.Size {
		t.Fatal("test could not construct a 32-byte inline todo")
	}
	state := &agentv1.ConversationStateStructure{Todos: [][]byte{inline}}

	entries, err := (&Service{}).importConversationState(testConversation(nil), state, nil)

	if err != nil {
		t.Fatalf("importConversationState() error = %v", err)
	}
	runtimeState, ok := importedRuntimeState(t, entries)
	if !ok || len(runtimeState.Todos) != 1 || runtimeState.Todos[0].GetId() != "t" {
		t.Fatalf("imported todos = %v (found=%t), want the 32-byte inline todo", runtimeState.Todos, ok)
	}
}

func TestImportConversationStateReadsThePlanKeptInTheBlobStore(t *testing.T) {
	plan := marshalImported(t, &agentv1.ConversationPlan{Plan: "1. Reproduce\n2. Fix\n3. Verify"})
	stubDiskKVBlobs(t, map[string][]byte{diskBlobHex(t, plan): plan})
	state := &agentv1.ConversationStateStructure{Plan: diskBlobID(t, plan)}

	entries, err := (&Service{}).importConversationState(testConversation(nil), state, nil)

	if err != nil {
		t.Fatalf("importConversationState() error = %v; this error fails the whole turn", err)
	}
	runtimeState, ok := importedRuntimeState(t, entries)
	if !ok || runtimeState.PlanText != "1. Reproduce\n2. Fix\n3. Verify" {
		t.Fatalf("imported plan = %q (found=%t)", runtimeState.PlanText, ok)
	}
}

func TestImportConversationStateKeepsTheChatWhenThePlanBlobIsMissing(t *testing.T) {
	stubDiskKVBlobs(t, nil)
	missing := sha256.Sum256([]byte("a plan that is not in the blob store"))
	state := &agentv1.ConversationStateStructure{
		Plan:  missing[:],
		Todos: [][]byte{marshalImported(t, &agentv1.TodoItem{Id: "inline", Content: "Still imported"})},
	}

	entries, err := (&Service{}).importConversationState(testConversation(nil), state, nil)

	if err != nil {
		t.Fatalf("importConversationState() error = %v; a missing plan must not fail the turn", err)
	}
	runtimeState, ok := importedRuntimeState(t, entries)
	if !ok || runtimeState.PlanText != "" || len(runtimeState.Todos) != 1 {
		t.Fatalf("runtime state = %+v (found=%t), want the todo and no plan", runtimeState, ok)
	}
}

// A long chat has been summarized. When its messages cannot be restored, the
// summary is the history that is left, and it is a blob id as well.
func TestImportConversationStateReadsTheSummaryKeptInTheBlobStore(t *testing.T) {
	summary := marshalImported(t, &agentv1.ConversationSummary{Summary: "The user is fixing AwaitShell."})
	stubDiskKVBlobs(t, map[string][]byte{diskBlobHex(t, summary): summary})
	state := &agentv1.ConversationStateStructure{Summary: diskBlobID(t, summary)}

	entries, err := (&Service{}).importConversationState(testConversation(nil), state, nil)

	if err != nil {
		t.Fatalf("importConversationState() error = %v; this error fails the whole turn", err)
	}
	text, ok := importedSummary(t, entries)
	if !ok || text != "The user is fixing AwaitShell." {
		t.Fatalf("imported summary = %q (found=%t)", text, ok)
	}
}

func TestImportConversationStateKeepsTheChatWhenTheSummaryBlobIsMissing(t *testing.T) {
	stubDiskKVBlobs(t, nil)
	missing := sha256.Sum256([]byte("a summary that is not in the blob store"))
	state := &agentv1.ConversationStateStructure{Summary: missing[:]}

	entries, err := (&Service{}).importConversationState(testConversation(nil), state, nil)

	if err != nil {
		t.Fatalf("importConversationState() error = %v; a missing summary must not fail the turn", err)
	}
	if text, ok := importedSummary(t, entries); ok {
		t.Fatalf("imported a summary %q from a blob that does not exist", text)
	}
}
