package forwarder

import "testing"

// Writing an event again under the same id replaces it. Reading has to follow
// the same rule while writes still wait in the two-second write buffer:
// a provider call is recorded when its pass completes and once more when the
// turn is finalized, and the turn total is read straight after.

func newBufferedUsageStore(t *testing.T) *UsageFileStore {
	t.Helper()
	store := NewUsageFileStore(t.TempDir())
	t.Cleanup(func() { _ = store.Flush() })
	return store
}

func recordedCall(input int64) usageFileEvent {
	return usageFileEvent{
		EventID:          "req-1::call-1",
		Kind:             usageEventKindProvider,
		Status:           "completed",
		InputTokens:      input,
		OutputTokens:     108_300,
		CacheReadTokens:  58_365_000,
		CacheWriteTokens: 905_000,
		UsagePresent:     true,
	}
}

func mustUpsert(t *testing.T, store *UsageFileStore, event usageFileEvent) {
	t.Helper()
	if err := store.UpsertEvent(event); err != nil {
		t.Fatalf("UpsertEvent() error = %v", err)
	}
}

func mustLookup(t *testing.T, store *UsageFileStore, id string) usageFileEvent {
	t.Helper()
	event, found, err := store.LookupEvent(id)
	if err != nil || !found {
		t.Fatalf("LookupEvent(%q) found=%v error=%v", id, found, err)
	}
	return event
}

func TestUsageLookupCountsACallRecordedTwiceOnce(t *testing.T) {
	store := newBufferedUsageStore(t)
	mustUpsert(t, store, recordedCall(1_000))
	mustUpsert(t, store, recordedCall(1_000))

	got := mustLookup(t, store, "req-1")

	if got.InputTokens != 1_000 || got.OutputTokens != 108_300 || got.CacheReadTokens != 58_365_000 || got.CacheWriteTokens != 905_000 {
		t.Fatalf("usage of the request = input %d, output %d, cache read %d, cache write %d; want the one call counted once",
			got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheWriteTokens)
	}
}

func TestUsageLookupUsesTheLatestBufferedWriteOfACall(t *testing.T) {
	store := newBufferedUsageStore(t)
	mustUpsert(t, store, recordedCall(1_000))
	mustUpsert(t, store, recordedCall(2_500))

	if got := mustLookup(t, store, "req-1"); got.InputTokens != 2_500 {
		t.Fatalf("input tokens = %d, want 2500 from the latest write", got.InputTokens)
	}
}

func TestUsageLookupPrefersABufferedWriteOverTheSavedOne(t *testing.T) {
	store := newBufferedUsageStore(t)
	mustUpsert(t, store, recordedCall(1_000))
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	mustUpsert(t, store, recordedCall(2_500))

	if got := mustLookup(t, store, "req-1"); got.InputTokens != 2_500 || got.CacheReadTokens != 58_365_000 {
		t.Fatalf("input / cache read = %d / %d, want 2500 / 58365000: the newer write, counted once", got.InputTokens, got.CacheReadTokens)
	}
}

func TestUsageLookupAddsUpDifferentCallsOfARequest(t *testing.T) {
	store := newBufferedUsageStore(t)
	first := recordedCall(1_000)
	second := recordedCall(400)
	second.EventID = "req-1::call-2"
	mustUpsert(t, store, first)
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	mustUpsert(t, store, second)

	if got := mustLookup(t, store, "req-1"); got.InputTokens != 1_400 || got.OutputTokens != 216_600 {
		t.Fatalf("input / output = %d / %d, want 1400 / 216600 across both calls", got.InputTokens, got.OutputTokens)
	}
}

// The sequence a finished turn runs: the pass records its usage, turn
// completion records it again, then the turn row is written from the total.
func TestFinalizedTurnShowsTheUsageOfItsProviderCall(t *testing.T) {
	store := newBufferedUsageStore(t)
	service := &Service{usageStore: store}
	usage := turnUsageSnapshot{
		Provider:         "cursor_account",
		Model:            "claude-fable-5-2",
		InputTokens:      1_000,
		OutputTokens:     108_300,
		CacheReadTokens:  58_365_000,
		CacheWriteTokens: 905_000,
		UsagePresent:     true,
	}
	for range 2 {
		if err := service.recordTurnUsageSnapshot(nil, "", 5, "req-1", "call-1", "completed", usage, "", false); err != nil {
			t.Fatalf("recordTurnUsageSnapshot() error = %v", err)
		}
	}
	if err := service.recordTurnFinalizedSnapshot(nil, "conv-1", 5, "req-1", "completed", ""); err != nil {
		t.Fatalf("recordTurnFinalizedSnapshot() error = %v", err)
	}

	turn := mustLookup(t, store, turnUsageEventID("conv-1", 5, "req-1"))

	if turn.InputTokens != 1_000 || turn.OutputTokens != 108_300 || turn.CacheReadTokens != 58_365_000 || turn.CacheWriteTokens != 905_000 {
		t.Fatalf("turn row = input %d, output %d, cache read %d, cache write %d; want exactly the usage of its one provider call",
			turn.InputTokens, turn.OutputTokens, turn.CacheReadTokens, turn.CacheWriteTokens)
	}
}
