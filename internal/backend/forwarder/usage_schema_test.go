package forwarder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Up to schema 2 a provider call served by a Cursor account was stored with
// Cursor's whole-prompt input figure, cache tokens included, while every other
// provider's input was stored without them. Totals, cache hit rate and cost
// treat input as not including them, so those rows counted their cache tokens
// twice. Schema 3 stores them like the rest; a file written before that is
// converted once, and a copy of it is kept.

func usageCall(id, provider string, at time.Time, input, output, cacheRead, cacheWrite int64) usageFileEvent {
	return usageFileEvent{
		EventID:          id,
		Kind:             usageEventKindProvider,
		Status:           "completed",
		At:               at,
		Provider:         provider,
		InputTokens:      input,
		OutputTokens:     output,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		TotalTokens:      input + output + cacheRead + cacheWrite,
		UsagePresent:     true,
	}
}

// writeSchema2UsageFile stores events the way a schema 2 build left them,
// newest first, with totals and daily buckets that add up.
func writeSchema2UsageFile(t *testing.T, events ...usageFileEvent) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), usageFileName)
	doc := usageFileDocument{SchemaVersion: 2, UpdatedAt: time.Now().UTC(), Daily: []usageFileDaily{}}
	for _, event := range events {
		applyUsageFileDelta(&doc, event.At, usageFileEventDelta(event))
		doc.RecentEvents = append(doc.RecentEvents, event)
	}
	if err := writeJSONFileAtomic(path, doc); err != nil {
		t.Fatalf("write schema 2 usage file: %v", err)
	}
	return path
}

func readUsageFileFromDisk(t *testing.T, path string) usageFileDocument {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read usage file: %v", err)
	}
	var doc usageFileDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode usage file: %v", err)
	}
	return doc
}

func storedEvent(t *testing.T, doc usageFileDocument, id string) usageFileEvent {
	t.Helper()
	for _, event := range doc.RecentEvents {
		if event.EventID == id {
			return event
		}
	}
	t.Fatalf("event %q is not in the usage file", id)
	return usageFileEvent{}
}

func TestSchema2UsageFileIsConvertedOnTheNextWrite(t *testing.T) {
	monday := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	tuesday := monday.Add(24 * time.Hour)
	path := writeSchema2UsageFile(t,
		// One recorded 64-step turn: 59.271M is the whole prompt.
		usageCall("req-long::call", "cursor_account", tuesday, 59_271_000, 108_300, 58_365_000, 905_000),
		usageCall("req-slot::call", "cursor_account1", monday, 6_298_000, 35_900, 6_231_000, 0),
		usageCall("req-nocache::call", "cursor_account", monday, 1_200, 25, 0, 0),
		// Own-key providers already store input without cache tokens, also
		// when that input happens to be the larger figure.
		usageCall("req-anthropic::call", "anthropic", monday, 9_000, 1_500, 150_000, 4_000),
		usageCall("req-openai::call", "openai", monday, 70_000, 900, 30_000, 0),
		usageFileEvent{EventID: "turn::conv::7", Kind: usageEventKindTurn, Status: usageTurnStatusDone, At: tuesday,
			InputTokens: 118_542_000, OutputTokens: 216_600, CacheReadTokens: 116_730_000, CacheWriteTokens: 1_810_000},
	)
	before := readUsageFileFromDisk(t, path)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	store := &UsageFileStore{path: path}
	mustUpsert(t, store, usageCall("req-new::call", "cursor_account", tuesday, 400, 2_000, 250_000, 3_000))
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	after := readUsageFileFromDisk(t, path)

	if after.SchemaVersion != 3 {
		t.Fatalf("schema version = %d, want 3", after.SchemaVersion)
	}
	long := storedEvent(t, after, "req-long::call")
	if long.InputTokens != 1_000 || long.CacheReadTokens != 58_365_000 || long.CacheWriteTokens != 905_000 || long.OutputTokens != 108_300 {
		t.Fatalf("long turn = input %d, cache read %d, cache write %d, output %d; want input 1000 and the rest unchanged",
			long.InputTokens, long.CacheReadTokens, long.CacheWriteTokens, long.OutputTokens)
	}
	if long.TotalTokens != 59_379_300 {
		t.Fatalf("long turn total = %d, want 59379300: prompt plus output, cache tokens counted once", long.TotalTokens)
	}
	if slot := storedEvent(t, after, "req-slot::call"); slot.InputTokens != 67_000 || slot.TotalTokens != 6_333_900 {
		t.Fatalf("numbered account slot = input %d, total %d; want 67000 / 6333900", slot.InputTokens, slot.TotalTokens)
	}
	for _, id := range []string{"req-nocache::call", "req-anthropic::call", "req-openai::call", "turn::conv::7"} {
		was, is := storedEvent(t, before, id), storedEvent(t, after, id)
		if is.InputTokens != was.InputTokens || is.TotalTokens != was.TotalTokens {
			t.Fatalf("%s changed: input %d -> %d, total %d -> %d; only Cursor account calls with cache tokens are converted",
				id, was.InputTokens, is.InputTokens, was.TotalTokens, is.TotalTokens)
		}
	}
	if fresh := storedEvent(t, after, "req-new::call"); fresh.InputTokens != 400 || fresh.TotalTokens != 255_400 {
		t.Fatalf("call recorded after the conversion = input %d, total %d; want it stored as given", fresh.InputTokens, fresh.TotalTokens)
	}

	// Totals and days lose exactly the cache tokens that had been counted twice
	// (59.27M on Tuesday, 6.231M on Monday) and gain the new call.
	const doubleCounted, newCallInput, newCallTotal = 59_270_000 + 6_231_000, 400, 255_400
	if got, want := after.Totals.InputTokens, before.Totals.InputTokens-doubleCounted+newCallInput; got != want {
		t.Fatalf("total input = %d, want %d", got, want)
	}
	if got, want := after.Totals.TotalTokens, before.Totals.TotalTokens-doubleCounted+newCallTotal; got != want {
		t.Fatalf("total tokens = %d, want %d", got, want)
	}
	if after.Totals.CacheReadTokens != before.Totals.CacheReadTokens+250_000 || after.Totals.OutputTokens != before.Totals.OutputTokens+2_000 {
		t.Fatalf("cache read / output totals = %d / %d, want only the new call added", after.Totals.CacheReadTokens, after.Totals.OutputTokens)
	}
	days := map[string]usageFileDaily{}
	for _, day := range after.Daily {
		days[day.Date] = day
	}
	if got, want := days["2026-11-23"].InputTokens, int64(67_000+1_200+9_000+70_000); got != want {
		t.Fatalf("Monday input = %d, want %d", got, want)
	}
	if got, want := days["2026-11-24"].InputTokens, int64(1_000+400); got != want {
		t.Fatalf("Tuesday input = %d, want %d", got, want)
	}

	kept, err := os.ReadFile(path + ".schema-2.bak")
	if err != nil {
		t.Fatalf("copy of the file before conversion: %v", err)
	}
	if string(kept) != string(original) {
		t.Fatal("the kept copy differs from the file as it was before conversion")
	}
}

func TestConvertedUsageFileIsNotConvertedAgain(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	path := writeSchema2UsageFile(t, usageCall("req-long::call", "cursor_account", at, 59_271_000, 108_300, 58_365_000, 905_000))
	store := &UsageFileStore{path: path}
	for _, id := range []string{"req-a::call", "req-b::call"} {
		// After conversion, input can legitimately exceed the cache tokens.
		mustUpsert(t, store, usageCall(id, "cursor_account", at, 900_000, 10, 200_000, 0))
		if err := store.Flush(); err != nil {
			t.Fatalf("Flush() error = %v", err)
		}
	}

	doc := readUsageFileFromDisk(t, path)

	if got := storedEvent(t, doc, "req-long::call").InputTokens; got != 1_000 {
		t.Fatalf("converted call input = %d, want 1000", got)
	}
	if got := storedEvent(t, doc, "req-a::call").InputTokens; got != 900_000 {
		t.Fatalf("call recorded under schema 3 has input %d, want 900000 left as recorded", got)
	}
	if want := int64(1_000 + 900_000 + 900_000); doc.Totals.InputTokens != want {
		t.Fatalf("total input = %d, want %d", doc.Totals.InputTokens, want)
	}
}

// Reading follows the same rule before anything has been written back.
func TestLookupOnSchema2UsageFileReturnsConvertedFigures(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	path := writeSchema2UsageFile(t, usageCall("req-long::call", "cursor_account", at, 59_271_000, 108_300, 58_365_000, 905_000))

	got := mustLookup(t, &UsageFileStore{path: path}, "req-long")

	if got.InputTokens != 1_000 || got.TotalTokens != 59_379_300 {
		t.Fatalf("input / total = %d / %d, want 1000 / 59379300", got.InputTokens, got.TotalTokens)
	}
	if _, err := os.Stat(path + ".schema-2.bak"); !os.IsNotExist(err) {
		t.Fatalf("a read must not write anything: %v", err)
	}
}
