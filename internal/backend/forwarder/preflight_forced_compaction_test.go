package forwarder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"cursor/internal/appdata"
	modeladapter "cursor/internal/backend/agent/model"
	legacyruntime "cursor/internal/runtime"

	"google.golang.org/protobuf/proto"
)

// preflightOverflowCompiler 返回多条巨大的消息，使 guardCompiledConversationForProvider
// 截断后（每消息 120000 字符）估算 token 数仍远超窗口（64000），从而在 driveProvider
// 中稳定触发 auto force-retry 穷尽 / preflight 超限路径。
type preflightOverflowCompiler struct{}

func (preflightOverflowCompiler) Compile(_ *ConversationFile, mode agentv1.AgentMode, _ string, _ string, _ string) (CompiledConversation, error) {
	big := strings.Repeat("x", 600_000)
	return CompiledConversation{
		Mode: mode,
		Messages: []modeladapter.Message{
			{Role: "user", Content: big},
			{Role: "user", Content: big},
			{Role: "user", Content: big},
			{Role: "user", Content: big},
			{Role: "user", Content: big},
			{Role: "user", Content: big},
		},
	}, nil
}

func (preflightOverflowCompiler) DerivePromptContexts(_ *ConversationFile, _ agentv1.AgentMode, _ string) ([]PromptContextMessage, error) {
	return nil, nil
}

func TestBuildForcedPreflightCompactionPlanCompactsAllPriorTurns(t *testing.T) {
	service := NewService(t.TempDir(), nilResolver{})
	conversation := contextProjectionTestConversation(t, 4) // turns 1..4, request-1..request-4
	before := cloneConversationFile(conversation)
	stream := &ActiveStream{
		ConversationID: conversation.ConversationID,
		ModelID:        "model-a",
		ModelName:      "model-a",
		TurnSeq:        4,
		RequestID:      "request-4",
		LatestUserText: "question 4",
	}
	compiled := CompiledConversation{
		Messages: []modeladapter.Message{{Role: "user", Content: strings.Repeat("x", 600_000)}},
	}

	plan, err := service.buildForcedPreflightCompactionPlan(stream, conversation, compiled)
	if err != nil {
		t.Fatalf("buildForcedPreflightCompactionPlan() error = %v", err)
	}
	if plan == nil {
		t.Fatal("buildForcedPreflightCompactionPlan() = nil, want plan")
	}
	if got, want := plan.Trigger, compactionPreflightForcedTrigger; got != want {
		t.Fatalf("plan.Trigger = %q, want %q", got, want)
	}
	if !plan.PreserveCurrentTurnInputs {
		t.Fatal("plan.PreserveCurrentTurnInputs = false, want true (current turn must survive compaction)")
	}
	if got, want := plan.CompactTurnCount, int32(3); got != want {
		t.Fatalf("plan.CompactTurnCount = %d, want %d (all prior turns)", got, want)
	}
	if got, want := plan.RequestSource, compactionRequestSourcePromptAsset; got != want {
		t.Fatalf("plan.RequestSource = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(conversation.Entries, before.Entries) {
		t.Fatal("buildForcedPreflightCompactionPlan() mutated canonical conversation entries")
	}
}

func TestBuildForcedPreflightCompactionPlanNilWhenNothingToCompact(t *testing.T) {
	service := NewService(t.TempDir(), nilResolver{})
	conversation := contextProjectionTestConversation(t, 1) // only current turn
	conversation.CurrentTurnSeq = 1
	conversation.CurrentRequestID = "request-1"
	stream := &ActiveStream{
		ConversationID: conversation.ConversationID,
		ModelID:        "model-a",
		ModelName:      "model-a",
		TurnSeq:        1,
		RequestID:      "request-1",
		LatestUserText: "question 1",
	}
	compiled := CompiledConversation{
		Messages: []modeladapter.Message{{Role: "user", Content: strings.Repeat("x", 600_000)}},
	}
	plan, err := service.buildForcedPreflightCompactionPlan(stream, conversation, compiled)
	if err != nil {
		t.Fatalf("buildForcedPreflightCompactionPlan() error = %v", err)
	}
	if plan != nil {
		t.Fatalf("buildForcedPreflightCompactionPlan() = non-nil plan, want nil (nothing to compact): %#v", plan)
	}
}

func TestEscalateForcedPreflightCompactionSemantics(t *testing.T) {
	newHarness := func(t *testing.T, mutate func(*ActiveStream)) (*Service, *ActiveStream) {
		t.Helper()
		store := NewConversationFileStore(t.TempDir())
		conversation := testConversation(nil)
		appendEntriesInPlace(conversation, []HistoryEntry{
			testUserMessageEntry(t, 1, "request-1", "prior question"),
			newAssistantTextEntry(1, "request-1", "prior answer", "", ""),
			testUserMessageEntry(t, 2, "request-2", "current question"),
			newAssistantTextEntry(2, "request-2", "current answer", "", ""),
		})
		persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
		if err != nil {
			t.Fatalf("SaveConversationWithEntries() error = %v", err)
		}
		provider := &contextProjectionRequestProvider{requests: make(chan ProviderRequest, 1)}
		broker := NewStreamBroker()
		service := newServiceWithDependencies(store, NewHistoryProjector(), contextProjectionLifecycleCompiler{}, provider, broker)
		stream, err := broker.OpenStream(
			"request-2",
			persisted.ConversationID,
			2,
			"model-a",
			"model-a",
			agentv1.AgentMode_AGENT_MODE_AGENT,
			"current question",
		)
		if err != nil {
			t.Fatalf("OpenStream() error = %v", err)
		}
		stream.CheckpointConversation = cloneConversationFile(persisted)
		if mutate != nil {
			mutate(stream)
		}
		return service, stream
	}
	compiled := CompiledConversation{
		Messages: []modeladapter.Message{{Role: "user", Content: strings.Repeat("x", 600_000)}},
	}
	overflowCause := compactionTerminalError{code: compactionOverflowTerminalCode, message: "overflow"}

	t.Run("triggers once and records attempt", func(t *testing.T) {
		service, stream := newHarness(t, nil)
		escalated, err := service.escalateForcedPreflightCompaction(stream, stream.CheckpointConversation, compiled, overflowCause)
		if err != nil {
			t.Fatalf("escalateForcedPreflightCompaction() error = %v", err)
		}
		if !escalated {
			t.Fatal("escalateForcedPreflightCompaction() = false, want true")
		}
		stream.mu.Lock()
		attempts := stream.PreflightForcedCompactionAttempts
		pending := stream.PendingCompaction
		stream.mu.Unlock()
		if attempts != 1 {
			t.Fatalf("PreflightForcedCompactionAttempts = %d, want 1", attempts)
		}
		if pending == nil || pending.Trigger != compactionPreflightForcedTrigger {
			t.Fatalf("PendingCompaction = %#v, want trigger %q", pending, compactionPreflightForcedTrigger)
		}
	})

	t.Run("blocked after attempts exhausted", func(t *testing.T) {
		service, stream := newHarness(t, func(stream *ActiveStream) {
			stream.PreflightForcedCompactionAttempts = preflightForcedCompactionMaxAttempts
		})
		escalated, err := service.escalateForcedPreflightCompaction(stream, stream.CheckpointConversation, compiled, overflowCause)
		if err != nil {
			t.Fatalf("escalateForcedPreflightCompaction() error = %v", err)
		}
		if escalated {
			t.Fatal("escalateForcedPreflightCompaction() = true after attempts exhausted, want false")
		}
	})

	t.Run("ignores non-overflow cause", func(t *testing.T) {
		service, stream := newHarness(t, nil)
		escalated, err := service.escalateForcedPreflightCompaction(stream, stream.CheckpointConversation, compiled, errors.New("some other failure"))
		if err != nil {
			t.Fatalf("escalateForcedPreflightCompaction() error = %v", err)
		}
		if escalated {
			t.Fatal("escalateForcedPreflightCompaction() = true for non-overflow cause, want false")
		}
	})

	t.Run("blocked while manual compaction requested", func(t *testing.T) {
		service, stream := newHarness(t, func(stream *ActiveStream) {
			stream.ManualCompaction = manualCompactionDirective{Requested: true}
		})
		escalated, err := service.escalateForcedPreflightCompaction(stream, stream.CheckpointConversation, compiled, overflowCause)
		if err != nil {
			t.Fatalf("escalateForcedPreflightCompaction() error = %v", err)
		}
		if escalated {
			t.Fatal("escalateForcedPreflightCompaction() = true during manual compaction, want false")
		}
	})

	t.Run("blocked while compaction already pending", func(t *testing.T) {
		service, stream := newHarness(t, func(stream *ActiveStream) {
			stream.PendingCompaction = newPendingCompaction(&compactionPlan{Trigger: contextProjectionTrigger})
		})
		escalated, err := service.escalateForcedPreflightCompaction(stream, stream.CheckpointConversation, compiled, overflowCause)
		if err != nil {
			t.Fatalf("escalateForcedPreflightCompaction() error = %v", err)
		}
		if escalated {
			t.Fatal("escalateForcedPreflightCompaction() = true while compaction pending, want false")
		}
	})

	t.Run("returns false when nothing to compact", func(t *testing.T) {
		service, stream := newHarness(t, nil)
		// 把 conversation 换成只有当前轮（无前序轮次可压缩）。
		store := NewConversationFileStore(t.TempDir())
		conversation := testConversation(nil)
		appendEntriesInPlace(conversation, []HistoryEntry{
			testUserMessageEntry(t, 2, "request-2", "current question"),
			newAssistantTextEntry(2, "request-2", "current answer", "", ""),
		})
		persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
		if err != nil {
			t.Fatalf("SaveConversationWithEntries() error = %v", err)
		}
		escalated, err := service.escalateForcedPreflightCompaction(stream, persisted, compiled, overflowCause)
		if err != nil {
			t.Fatalf("escalateForcedPreflightCompaction() error = %v", err)
		}
		if escalated {
			t.Fatal("escalateForcedPreflightCompaction() = true with nothing to compact, want false")
		}
	})
}

func TestDriveProviderEscalatesToForcedLegacyCompactionWhenAutoExhausted(t *testing.T) {
	store := NewConversationFileStore(t.TempDir())
	conversation := testConversation(nil)
	appendEntriesInPlace(conversation, []HistoryEntry{
		testUserMessageEntry(t, 1, "request-1", "prior question"),
		newAssistantTextEntry(1, "request-1", "prior answer", "", ""),
		testUserMessageEntry(t, 2, "request-2", "current question"),
		newAssistantTextEntry(2, "request-2", "current answer", "", ""),
	})
	conversation.TokenDetailsMaxTokens = projectedConversationMaxTokens
	conversation.CurrentTurnSeq = 2
	conversation.CurrentRequestID = "request-2"
	persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
	if err != nil {
		t.Fatalf("SaveConversationWithEntries() error = %v", err)
	}

	provider := &contextProjectionRequestProvider{requests: make(chan ProviderRequest, 1)}
	broker := NewStreamBroker()
	service := newServiceWithDependencies(store, NewHistoryProjector(), preflightOverflowCompiler{}, provider, broker)
	stream, err := broker.OpenStream(
		"request-2",
		persisted.ConversationID,
		2,
		"model-a",
		"model-a",
		agentv1.AgentMode_AGENT_MODE_AGENT,
		"current question",
	)
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	stream.CheckpointConversation = cloneConversationFile(persisted)

	if err := service.driveProvider(stream); err != nil {
		t.Fatalf("driveProvider() error = %v", err)
	}
	stream.mu.Lock()
	pending := stream.PendingCompaction
	attempts := stream.PreflightForcedCompactionAttempts
	status := stream.Status
	providerActive := stream.ProviderActive
	stream.mu.Unlock()
	if pending == nil || pending.Trigger != compactionPreflightForcedTrigger {
		t.Fatalf("PendingCompaction = %#v, want trigger %q", pending, compactionPreflightForcedTrigger)
	}
	if attempts != 1 {
		t.Fatalf("PreflightForcedCompactionAttempts = %d, want 1", attempts)
	}
	if status == StreamStatusFailed || status == StreamStatusCanceled || status == StreamStatusCompleted {
		t.Fatalf("driveProvider terminalized stream with status %q instead of escalating to forced compaction", status)
	}
	if providerActive {
		t.Fatal("forced compaction unexpectedly started the parent provider")
	}
	select {
	case request := <-provider.requests:
		t.Fatalf("forced compaction unexpectedly sent provider request: %#v", request)
	default:
	}
}

func TestDriveProviderDoesNotEscalateWhenAttemptsExhausted(t *testing.T) {
	store := NewConversationFileStore(t.TempDir())
	conversation := testConversation(nil)
	appendEntriesInPlace(conversation, []HistoryEntry{
		testUserMessageEntry(t, 1, "request-1", "prior question"),
		newAssistantTextEntry(1, "request-1", "prior answer", "", ""),
		testUserMessageEntry(t, 2, "request-2", "current question"),
		newAssistantTextEntry(2, "request-2", "current answer", "", ""),
	})
	conversation.TokenDetailsMaxTokens = projectedConversationMaxTokens
	conversation.CurrentTurnSeq = 2
	conversation.CurrentRequestID = "request-2"
	persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
	if err != nil {
		t.Fatalf("SaveConversationWithEntries() error = %v", err)
	}

	provider := &contextProjectionRequestProvider{requests: make(chan ProviderRequest, 1)}
	broker := NewStreamBroker()
	service := newServiceWithDependencies(store, NewHistoryProjector(), preflightOverflowCompiler{}, provider, broker)
	stream, err := broker.OpenStream(
		"request-2",
		persisted.ConversationID,
		2,
		"model-a",
		"model-a",
		agentv1.AgentMode_AGENT_MODE_AGENT,
		"current question",
	)
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	stream.CheckpointConversation = cloneConversationFile(persisted)
	stream.PreflightForcedCompactionAttempts = preflightForcedCompactionMaxAttempts

	if err := service.driveProvider(stream); err != nil {
		t.Fatalf("driveProvider() error = %v", err)
	}
	stream.mu.Lock()
	status := stream.Status
	pending := stream.PendingCompaction
	stream.mu.Unlock()
	if status != StreamStatusFailed {
		t.Fatalf("stream status = %q, want %q (terminal after escalation exhausted)", status, StreamStatusFailed)
	}
	if pending != nil {
		t.Fatalf("PendingCompaction = %#v, want nil (no escalation after attempts exhausted)", pending)
	}
}

type cursorAccountSavedStateResolver struct {
	nilResolver
}

func (cursorAccountSavedStateResolver) SelectChannelForModel(_ context.Context, modelID string) (*legacyruntime.ResolvedChannel, error) {
	return &legacyruntime.ResolvedChannel{
		ID:                  modelID,
		Source:              legacyruntime.ModelSourceCursorAccount,
		CredentialScope:     legacyruntime.CredentialScopeCursorAccount,
		Model:               "claude-opus-5-5",
		ContextWindowTokens: 1_000_000,
	}, nil
}

func TestDriveProviderSkipsLocalCompactionWhenCursorAccountHasUpstreamCheckpoint(t *testing.T) {
	homeDir, err := os.MkdirTemp("", "cursor-byok-home-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	t.Setenv("CURSOR_BYOK_HOME", homeDir)
	store := NewConversationFileStore(t.TempDir())
	conversation := testConversation(nil)
	appendEntriesInPlace(conversation, []HistoryEntry{
		testUserMessageEntry(t, 1, "request-1", "implement 1M token feature"),
		newAssistantTextEntry(1, "request-1", "partial implementation", "", ""),
		testUserMessageEntry(t, 2, "request-2", "continue"),
	})
	conversation.TokenDetailsMaxTokens = 1_000_000
	conversation.CurrentTurnSeq = 2
	conversation.CurrentRequestID = "request-2"
	persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
	if err != nil {
		t.Fatalf("SaveConversationWithEntries() error = %v", err)
	}

	// Simulate an upstream Cursor checkpoint that already compacted the prompt to 240k tokens.
	checkpoint := &agentv1.ConversationStateStructure{
		Mode: agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		TokenDetails: &agentv1.ConversationTokenDetails{
			UsedTokens: 240_000,
			MaxTokens:  1_000_000,
		},
		Turns: [][]byte{[]byte("turn-1")},
	}
	rawCheckpoint, err := proto.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(appdata.HistoryRootPath(), persisted.ConversationID, "cursor-agent-state.bin")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, rawCheckpoint, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { modeladapter.ClearCursorAccountState(persisted.ConversationID) })

	provider := &contextProjectionRequestProvider{requests: make(chan ProviderRequest, 1)}
	broker := NewStreamBroker()
	service := newServiceWithDependencies(store, NewHistoryProjector(), preflightOverflowCompiler{}, provider, broker)
	service.resolver = cursorAccountSavedStateResolver{}

	stream, err := broker.OpenStream(
		"request-2",
		persisted.ConversationID,
		2,
		"cursor_account1:claude-opus-5-5",
		"Claude Opus 5.5",
		agentv1.AgentMode_AGENT_MODE_AGENT,
		"continue",
	)
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	stream.CheckpointConversation = cloneConversationFile(persisted)

	if err := service.driveProvider(stream); err != nil {
		t.Fatalf("driveProvider() error = %v", err)
	}
	stream.mu.Lock()
	pending := stream.PendingCompaction
	stream.mu.Unlock()
	if pending != nil {
		t.Fatalf("PendingCompaction = %#v, want nil when Cursor upstream checkpoint owns compaction", pending)
	}
	select {
	case req := <-provider.requests:
		if req.ModelID != "cursor_account1:claude-opus-5-5" {
			t.Fatalf("provider request model = %q", req.ModelID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected provider request to start immediately without blocking on local summarization")
	}
}

type failingSummaryThenSuccessProvider struct {
	mu    sync.Mutex
	calls []ProviderRequest
}

func (p *failingSummaryThenSuccessProvider) StartStream(_ context.Context, req ProviderRequest, sink func(modeladapter.ModelEvent) error) error {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	if strings.HasPrefix(req.CompileSummary, "compaction ") {
		return errors.New("summary provider rejected oversized request")
	}
	if err := sink(modeladapter.ModelEvent{Kind: modeladapter.ModelEventKindTextDelta, Text: "continued after fallback compaction"}); err != nil {
		return err
	}
	return sink(modeladapter.ModelEvent{Kind: modeladapter.ModelEventKindTurnFinished, FinishReason: "stop"})
}

func TestPreflightForcedCompactionFallsBackDeterministicallyAndResumesTurn(t *testing.T) {
	store := NewConversationFileStore(t.TempDir())
	conversation := testConversation(nil)
	appendEntriesInPlace(conversation, []HistoryEntry{
		testUserMessageEntry(t, 1, "request-1", "implement large feature"),
		newAssistantTextEntry(1, "request-1", strings.Repeat("completed step ", 200), "", ""),
		testUserMessageEntry(t, 2, "request-2", "continue"),
	})
	conversation.TokenDetailsMaxTokens = projectedConversationMaxTokens
	conversation.CurrentTurnSeq = 2
	conversation.CurrentRequestID = "request-2"
	persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
	if err != nil {
		t.Fatalf("SaveConversationWithEntries() error = %v", err)
	}

	provider := &failingSummaryThenSuccessProvider{}
	broker := NewStreamBroker()
	service := newServiceWithDependencies(store, NewHistoryProjector(), contextProjectionLifecycleCompiler{}, provider, broker)
	stream, err := broker.OpenStream(
		"request-2",
		persisted.ConversationID,
		2,
		"model-a",
		"model-a",
		agentv1.AgentMode_AGENT_MODE_AGENT,
		"continue",
	)
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	stream.CheckpointConversation = cloneConversationFile(persisted)

	plan, err := service.buildForcedPreflightCompactionPlan(stream, persisted, CompiledConversation{
		Messages: []modeladapter.Message{{Role: "user", Content: strings.Repeat("x", 150_000)}},
	})
	if err != nil || plan == nil {
		t.Fatalf("buildForcedPreflightCompactionPlan() plan=%v err=%v", plan, err)
	}
	pending := newPendingCompaction(plan)
	stream.PendingCompaction = pending
	if err := service.startPendingCompactionSummary(stream, pending); err != nil {
		t.Fatalf("startPendingCompactionSummary() error = %v", err)
	}

	ackedBlobs := map[uint32]struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stream.mu.Lock()
		status := stream.Status
		snapshot := append([]StreamEvent(nil), stream.Backlog...)
		stream.mu.Unlock()
		for _, item := range snapshot {
			kv := item.Message.GetKvServerMessage()
			if kv == nil || kv.GetSetBlobArgs() == nil {
				continue
			}
			if _, seen := ackedBlobs[kv.GetId()]; seen {
				continue
			}
			ackedBlobs[kv.GetId()] = struct{}{}
			_ = service.postStreamCommandWait(stream, streamCommand{
				Kind: streamCommandMetadata,
				Intent: InboundIntent{
					Kind:      "kv_result",
					RequestID: stream.RequestID,
					KVClientMessage: &agentv1.KvClientMessage{
						Id:      kv.GetId(),
						Message: &agentv1.KvClientMessage_SetBlobResult{SetBlobResult: &agentv1.SetBlobResult{}},
					},
				},
			})
		}
		if status == StreamStatusCompleted || status == StreamStatusFailed {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	stream.mu.Lock()
	status := stream.Status
	backlog := append([]StreamEvent(nil), stream.Backlog...)
	stream.mu.Unlock()
	if status != StreamStatusCompleted {
		t.Fatalf("stream status = %q, want %q (should recover via fallback summary and complete turn)", status, StreamStatusCompleted)
	}
	provider.mu.Lock()
	callCount := len(provider.calls)
	provider.mu.Unlock()
	if callCount != 2 {
		t.Fatalf("provider call count = %d, want 2 (1 failed summary + 1 resumed turn)", callCount)
	}
	sawSummaryCompleted := false
	for _, item := range backlog {
		if item.Message.GetInteractionUpdate().GetSummaryCompleted() != nil {
			sawSummaryCompleted = true
		}
	}
	if !sawSummaryCompleted {
		t.Fatal("expected SummaryCompletedUpdate to be published so UI exits 'Summarizing chat context'")
	}
}

func TestCancelDuringCompactionPublishesFailedSummaryCompleted(t *testing.T) {
	store := NewConversationFileStore(t.TempDir())
	conversation := testConversation(nil)
	appendEntriesInPlace(conversation, []HistoryEntry{
		testUserMessageEntry(t, 1, "request-1", "prior question"),
		newAssistantTextEntry(1, "request-1", "prior answer", "", ""),
	})
	persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
	if err != nil {
		t.Fatal(err)
	}
	broker := NewStreamBroker()
	service := newServiceWithDependencies(store, NewHistoryProjector(), contextProjectionLifecycleCompiler{}, nil, broker)
	stream, err := broker.OpenStream("request-cancel-compact", persisted.ConversationID, 2, "model-a", "model-a", agentv1.AgentMode_AGENT_MODE_AGENT, "continue")
	if err != nil {
		t.Fatal(err)
	}
	stream.CheckpointConversation = cloneConversationFile(persisted)
	stream.Phase = TurnPhaseCompacting
	stream.PendingCompaction = newPendingCompaction(&compactionPlan{Trigger: compactionPreflightForcedTrigger})

	if err := service.handleCancelIntent(InboundIntent{
		Kind:         "cancel",
		RequestID:    stream.RequestID,
		CancelReason: "composer_abort_controller_aborted",
	}); err != nil {
		t.Fatalf("handleCancelIntent() error = %v", err)
	}

	stream.mu.Lock()
	backlog := append([]StreamEvent(nil), stream.Backlog...)
	stream.mu.Unlock()
	foundFailedSummaryCompleted := false
	for _, item := range backlog {
		completed := item.Message.GetInteractionUpdate().GetSummaryCompleted()
		if completed != nil && completed.GetFailed() {
			foundFailedSummaryCompleted = true
		}
	}
	if !foundFailedSummaryCompleted {
		t.Fatal("expected SummaryCompletedUpdate{Failed: true} on cancel during compaction")
	}
}

func TestDriveProviderCompactsCursorAccountCheckpointWhenAboveBudget(t *testing.T) {
	homeDir, err := os.MkdirTemp("", "cursor-byok-home-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	t.Setenv("CURSOR_BYOK_HOME", homeDir)
	store := NewConversationFileStore(t.TempDir())
	conversation := testConversation(nil)
	appendEntriesInPlace(conversation, []HistoryEntry{
		testUserMessageEntry(t, 1, "request-1", "implement 1M token feature"),
		newAssistantTextEntry(1, "request-1", strings.Repeat("completed step ", 200), "", ""),
		testUserMessageEntry(t, 2, "request-2", "continue"),
	})
	conversation.TokenDetailsMaxTokens = 1_000_000
	conversation.CurrentTurnSeq = 2
	conversation.CurrentRequestID = "request-2"
	persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
	if err != nil {
		t.Fatalf("SaveConversationWithEntries() error = %v", err)
	}

	// Simulate an upstream Cursor checkpoint at 85% (below Cursor's 95% blocking threshold):
	// Cursor's cloud agent server owns summarization until 95% / 5K remaining, so local forced compaction must NOT fire early.
	checkpointBelow := &agentv1.ConversationStateStructure{
		Mode: agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		TokenDetails: &agentv1.ConversationTokenDetails{
			UsedTokens: 850_000,
			MaxTokens:  1_000_000,
		},
		Turns: [][]byte{[]byte("turn-1")},
	}
	rawBelow, err := proto.Marshal(checkpointBelow)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(appdata.HistoryRootPath(), persisted.ConversationID, "cursor-agent-state.bin")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, rawBelow, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { modeladapter.ClearCursorAccountState(persisted.ConversationID) })

	providerBelow := &contextProjectionRequestProvider{requests: make(chan ProviderRequest, 1)}
	brokerBelow := NewStreamBroker()
	serviceBelow := newServiceWithDependencies(store, NewHistoryProjector(), contextProjectionLifecycleCompiler{}, providerBelow, brokerBelow)
	serviceBelow.resolver = cursorAccountSavedStateResolver{}
	streamBelow, err := brokerBelow.OpenStream(
		"request-2-below",
		persisted.ConversationID,
		2,
		"cursor_account1:claude-opus-5-5",
		"Claude Opus 5.5",
		agentv1.AgentMode_AGENT_MODE_AGENT,
		"continue",
	)
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	streamBelow.CheckpointConversation = cloneConversationFile(persisted)
	if err := serviceBelow.driveProvider(streamBelow); err != nil {
		t.Fatalf("driveProvider(85%%) error = %v", err)
	}
	streamBelow.mu.Lock()
	pendingBelow := streamBelow.PendingCompaction
	streamBelow.mu.Unlock()
	if pendingBelow != nil {
		t.Fatalf("PendingCompaction = %#v, want nil at 85%% (below Cursor's 95%% blocking threshold)", pendingBelow)
	}

	// Simulate an upstream Cursor checkpoint that has reached Cursor's 95% blocking threshold (950,362 >= 950,000).
	modeladapter.ClearCursorAccountState(persisted.ConversationID)
	checkpoint := &agentv1.ConversationStateStructure{
		Mode: agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		TokenDetails: &agentv1.ConversationTokenDetails{
			UsedTokens: 950_362,
			MaxTokens:  1_000_000,
		},
		Turns: [][]byte{[]byte("turn-1")},
	}
	rawCheckpoint, err := proto.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, rawCheckpoint, 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &contextProjectionRequestProvider{requests: make(chan ProviderRequest, 1)}
	broker := NewStreamBroker()
	service := newServiceWithDependencies(store, NewHistoryProjector(), contextProjectionLifecycleCompiler{}, provider, broker)
	service.resolver = cursorAccountSavedStateResolver{}

	stream, err := broker.OpenStream(
		"request-2",
		persisted.ConversationID,
		2,
		"cursor_account1:claude-opus-5-5",
		"Claude Opus 5.5",
		agentv1.AgentMode_AGENT_MODE_AGENT,
		"continue",
	)
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	stream.CheckpointConversation = cloneConversationFile(persisted)

	if err := service.driveProvider(stream); err != nil {
		t.Fatalf("driveProvider() error = %v", err)
	}
	stream.mu.Lock()
	pending := stream.PendingCompaction
	phase := stream.Phase
	stream.mu.Unlock()
	if pending == nil || pending.Trigger != compactionPreflightForcedTrigger {
		t.Fatalf("PendingCompaction = %#v, want trigger %q when upstream checkpoint reaches 95%% blocking budget", pending, compactionPreflightForcedTrigger)
	}
	if phase != TurnPhaseCompacting {
		t.Fatalf("stream.Phase = %q, want %q", phase, TurnPhaseCompacting)
	}
}

