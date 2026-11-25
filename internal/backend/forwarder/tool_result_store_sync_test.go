package forwarder

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
)

const storeSyncRequestID = "req-store-sync"
const storeSyncConversationID = "conv-store-sync"

// newStoreBackedStream wires a stream to a real history store whose
// conversation already has some persisted history.
func newStoreBackedStream(t *testing.T) (*Service, *ActiveStream, *ConversationFileStore) {
	t.Helper()
	store := NewConversationFileStore(t.TempDir())
	if _, err := store.CreateConversation(storeSyncConversationID, agentv1.AgentMode_AGENT_MODE_AGENT, "", "", ""); err != nil {
		t.Fatalf("CreateConversation() error = %v", err)
	}
	persisted, _, err := store.AppendEntries(storeSyncConversationID, []HistoryEntry{
		newAssistantTextEntry(1, storeSyncRequestID, "earlier history", "", ""),
	})
	if err != nil {
		t.Fatalf("AppendEntries() error = %v", err)
	}

	broker := NewStreamBroker()
	stream, err := broker.OpenStream(storeSyncRequestID, storeSyncConversationID, 1, "m", "m", agentv1.AgentMode_AGENT_MODE_AGENT, "run a tool")
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	stream.mu.Lock()
	stream.CheckpointConversation = cloneConversationFile(persisted)
	syncCheckpointPersistBaseline(stream, persisted)
	stream.TimerTokens = make(map[string]uint64)
	stream.StreamTimers = make(map[string]*time.Timer)
	stream.mu.Unlock()

	service := newTestService(broker, func() time.Duration { return time.Second }, nil)
	service.store = store
	t.Cleanup(func() {
		// Let the debounced persist finish before the temp dir is removed.
		_ = service.flushCheckpointPersistSync(stream, storeSyncConversationID)
		stream.mu.Lock()
		if stream.CheckpointPublishTimer != nil {
			stream.CheckpointPublishTimer.Stop()
		}
		stream.mu.Unlock()
	})
	return service, stream, store
}

func storedToolResultCount(t *testing.T, store *ConversationFileStore) int {
	t.Helper()
	conversation, err := store.LoadConversation(storeSyncConversationID)
	if err != nil || conversation == nil {
		t.Fatalf("LoadConversation() = %v, %v", conversation, err)
	}
	count := 0
	for _, entry := range conversation.Entries {
		if entry.Kind == "tool_result" {
			count++
		}
	}
	return count
}

// The stream's actor handles tool results one at a time, and the model's
// output waits behind them. Handling a result must not wait for the history
// store: reading and rewriting a multi-megabyte history per tool result is
// what made long conversations crawl.
func TestToolResultIsHandledWithoutWaitingForTheHistoryStore(t *testing.T) {
	service, stream, store := newStoreBackedStream(t)

	// Hold the conversation's store lock: any synchronous store access from
	// the tool-result path now blocks until it is released.
	release, err := acquireConversationLock(store.lockPath(storeSyncConversationID))
	if err != nil {
		t.Fatalf("acquireConversationLock() error = %v", err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- service.completeImmediateToolResult(stream, runtimecore.ToolInvocation{
			CallID:      "call-store-sync",
			ToolName:    "ReadLints",
			ArgsJSON:    []byte(`{}`),
			ModelCallID: "mc-store-sync",
		}, "no linter errors", nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("completeImmediateToolResult() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handling a tool result blocked on the history store")
	}
	if got := countHistoryByKind(stream, "tool_result"); got != 1 {
		t.Fatalf("in-memory tool_result entries = %d, want 1", got)
	}

	release()
	released = true

	// Nothing is lost: the result reaches the store shortly afterwards.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && storedToolResultCount(t, store) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := storedToolResultCount(t, store); got != 1 {
		t.Fatalf("stored tool_result entries = %d, want 1", got)
	}
}

func storeLockWaiters(store *ConversationFileStore) int {
	key := filepath.Clean(store.lockPath(storeSyncConversationID))
	conversationProcessLocksMu.Lock()
	defer conversationProcessLocksMu.Unlock()
	if lock := conversationProcessLocks[key]; lock != nil {
		return lock.refs
	}
	return 0
}

// The debounced persist runs beside the actor. An entry appended while a
// persist run is busy writing must still be written: it may not be counted as
// persisted, and its own persist timer may not be canceled.
func TestEntryAppendedDuringAPersistRunIsStillPersisted(t *testing.T) {
	service, stream, store := newStoreBackedStream(t)

	release, err := acquireConversationLock(store.lockPath(storeSyncConversationID))
	if err != nil {
		t.Fatalf("acquireConversationLock() error = %v", err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	// A persist run with nothing pending starts and stalls on the store.
	persistDone := make(chan error, 1)
	go func() { persistDone <- service.persistCheckpointDelta(stream, storeSyncConversationID) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && storeLockWaiters(store) < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	if storeLockWaiters(store) < 2 {
		t.Fatal("the persist run never reached the store")
	}

	// While it is stalled, the actor records a tool result.
	if _, err := service.appendConversationEntries(stream, storeSyncConversationID, []HistoryEntry{
		newToolResultEntry(1, storeSyncRequestID, "call-during-persist", "ReadLints", "{}", "appended while a persist run was writing", "", nil),
	}); err != nil {
		t.Fatalf("appendConversationEntries() error = %v", err)
	}

	release()
	released = true
	if err := <-persistDone; err != nil {
		t.Fatalf("persistCheckpointDelta() error = %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && storedToolResultCount(t, store) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := storedToolResultCount(t, store); got != 1 {
		t.Fatalf("stored tool_result entries = %d, want 1: the entry appended during the persist run was dropped", got)
	}
}

// Syncing conversation metadata is a state.json update. It must not rewrite
// the whole history file, which is megabytes for a long conversation.
func TestSyncConversationRecordLeavesTheHistoryFileAlone(t *testing.T) {
	service, stream, store := newStoreBackedStream(t)
	contextPath := store.contextPath(storeSyncConversationID)
	before, err := os.Stat(contextPath)
	if err != nil {
		t.Fatalf("stat context.json: %v", err)
	}
	time.Sleep(30 * time.Millisecond) // make a rewrite visible in the mtime

	stream.mu.Lock()
	stream.CheckpointConversation.TokenDetailsUsedTokens = 4242
	conversation := cloneConversationFile(stream.CheckpointConversation)
	stream.mu.Unlock()

	if err := service.syncConversationRecord(storeSyncConversationID, conversation); err != nil {
		t.Fatalf("syncConversationRecord() error = %v", err)
	}

	after, err := os.Stat(contextPath)
	if err != nil {
		t.Fatalf("stat context.json: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("context.json was rewritten by a metadata sync (mtime %s -> %s)", before.ModTime().Format(time.RFC3339Nano), after.ModTime().Format(time.RFC3339Nano))
	}
	stored, err := store.LoadConversation(storeSyncConversationID)
	if err != nil || stored == nil {
		t.Fatalf("LoadConversation() = %v, %v", stored, err)
	}
	if stored.TokenDetailsUsedTokens != 4242 {
		t.Fatalf("stored TokenDetailsUsedTokens = %d, want the synced value 4242", stored.TokenDetailsUsedTokens)
	}
	if len(stored.Entries) != 1 {
		t.Fatalf("stored entries = %d, want the existing history untouched", len(stored.Entries))
	}
}

// A conversation that is not on disk yet is still created by the sync.
func TestSyncConversationRecordCreatesAMissingConversation(t *testing.T) {
	store := NewConversationFileStore(t.TempDir())
	service := newTestService(NewStreamBroker(), func() time.Duration { return time.Second }, nil)
	service.store = store

	conversation := &ConversationFile{
		ConversationID:     "conv-new",
		RootConversationID: "conv-new",
		Mode:               "agent",
		NextTurnSeq:        1,
		NextEntrySeq:       1,
	}
	if err := service.syncConversationRecord("conv-new", conversation); err != nil {
		t.Fatalf("syncConversationRecord() error = %v", err)
	}
	stored, err := store.LoadConversation("conv-new")
	if err != nil || stored == nil {
		t.Fatalf("LoadConversation() = %v, %v; the sync must create the conversation", stored, err)
	}
	if stored.Mode != "agent" || stored.RootConversationID != "conv-new" {
		t.Fatalf("stored mode=%q root=%q, want agent/conv-new", stored.Mode, stored.RootConversationID)
	}
}
