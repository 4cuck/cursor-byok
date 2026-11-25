package forwarder

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cursor/internal/historymetrics"
	"cursor/internal/logger"
)

const (
	usageFileName = "usage.json"
	// See convertCursorAccountInputs for files written before this schema.
	usageFileSchemaVersion = historymetrics.UsageFileSchemaVersion
	usageRecentEventLimit  = 10_000

	usageEventKindProvider = "provider_call"
	usageEventKindTurn     = "turn_finalized"
	usageTurnStatusDone    = "completed"

	// 防抖批量写入配置
	usageWriteDebounceMs = 2000 // 2秒内的写入合并为一次
	usageWriteMaxBatch   = 50   // 或累积50个事件立即写入
	// 磁盘持续故障时 pending 的硬上限：溢出丢最旧的一半，防止无界增长拖垮内存。
	usagePendingEventsHardLimit = 10_000
)

type UsageFileStore struct {
	path string

	// 批量写入优化
	mu            sync.Mutex
	pendingEvents []usageFileEvent
	debounceTimer *time.Timer
}

type usageFileDocument struct {
	SchemaVersion int                       `json:"schema_version"`
	UpdatedAt     time.Time                 `json:"updated_at"`
	Totals        usageFileTotals           `json:"totals"`
	Daily         []usageFileDaily          `json:"daily"`
	RecentEvents  []usageFileEvent          `json:"recent_events"`
	EventIndex    map[string]usageFileEvent `json:"-"`
	// convertedFromSchema is the schema the file on disk still has when this
	// document was converted while reading it; zero when nothing was converted.
	convertedFromSchema int
}

type usageFileTotals struct {
	ProviderCalls     int64 `json:"provider_calls"`
	TurnsTotal        int64 `json:"turns_total"`
	ValidTurnsTotal   int64 `json:"valid_turns_total"`
	InvalidTurnsTotal int64 `json:"invalid_turns_total"`
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CacheReadTokens   int64 `json:"cache_read_tokens"`
	CacheWriteTokens  int64 `json:"cache_write_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
}

type usageFileDaily struct {
	Date              string `json:"date"`
	ProviderCalls     int64  `json:"provider_calls"`
	TurnsTotal        int64  `json:"turns_total"`
	ValidTurnsTotal   int64  `json:"valid_turns_total"`
	InvalidTurnsTotal int64  `json:"invalid_turns_total"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	TotalTokens       int64  `json:"total_tokens"`
}

type usageFileEvent struct {
	EventID          string    `json:"event_id"`
	Kind             string    `json:"kind,omitempty"`
	Status           string    `json:"status,omitempty"`
	At               time.Time `json:"at"`
	Model            string    `json:"model,omitempty"`
	Role             string    `json:"role,omitempty"`
	ParentModel      string    `json:"parent_model,omitempty"`
	LogicalModel     string    `json:"logical_model,omitempty"`
	ProviderModel    string    `json:"provider_model,omitempty"`
	ModelGroupID     string    `json:"model_group_id,omitempty"`
	TaskID           string    `json:"task_id,omitempty"`
	ExecutionMode    string    `json:"execution_mode,omitempty"`
	SupervisorModel  string    `json:"supervisor_model,omitempty"`
	ReviewerModel    string    `json:"reviewer_model,omitempty"`
	Provider         string    `json:"provider,omitempty"`
	BaseURL          string    `json:"base_url,omitempty"`
	GroupName        string    `json:"group_name,omitempty"`
	ErrorCode        string    `json:"error_code,omitempty"`
	Degraded         string    `json:"degraded,omitempty"`
	TTFTMS           int64     `json:"ttft_ms,omitempty"`
	DurationMS       int64     `json:"duration_ms,omitempty"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	UsagePresent     bool      `json:"usage_present"`
}

type usageFileDelta struct {
	providerCalls     int64
	turnsTotal        int64
	validTurnsTotal   int64
	invalidTurnsTotal int64
	inputTokens       int64
	outputTokens      int64
	cacheReadTokens   int64
	cacheWriteTokens  int64
	totalTokens       int64
}

func NewUsageFileStore(historyRoot string) *UsageFileStore {
	return &UsageFileStore{path: filepath.Join(strings.TrimSpace(historyRoot), usageFileName)}
}

func (store *UsageFileStore) UpsertEvent(event usageFileEvent) error {
	if store == nil || strings.TrimSpace(store.path) == "" {
		return nil
	}
	event.EventID = strings.TrimSpace(event.EventID)
	if event.EventID == "" {
		return nil
	}
	event.Kind = normalizeUsageEventKind(event.Kind)
	event.Status = strings.TrimSpace(event.Status)
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	} else {
		event.At = event.At.UTC()
	}
	event.InputTokens = nonNegativeInt64(event.InputTokens)
	event.OutputTokens = nonNegativeInt64(event.OutputTokens)
	event.CacheReadTokens = nonNegativeInt64(event.CacheReadTokens)
	event.CacheWriteTokens = nonNegativeInt64(event.CacheWriteTokens)
	event.TotalTokens = event.InputTokens + event.OutputTokens + event.CacheReadTokens + event.CacheWriteTokens

	// 批量写入优化：将事件加入 pending 队列，防抖后统一写入
	store.mu.Lock()
	defer store.mu.Unlock()

	store.pendingEvents = append(store.pendingEvents, event)

	// 磁盘持续故障时 pending 无上界增长会拖垮内存：超过硬上限丢最旧的一半并告警。
	// 正常路径防抖 2s / 批量上限都会及时清空，永远到不了这里。
	if len(store.pendingEvents) >= usagePendingEventsHardLimit {
		dropped := len(store.pendingEvents) - usagePendingEventsHardLimit/2
		logger.Errorf("forwarder usage pending overflow, dropping oldest events path=%s dropped=%d", store.path, dropped)
		store.pendingEvents = append(make([]usageFileEvent, 0, usagePendingEventsHardLimit), store.pendingEvents[dropped:]...)
	}

	// 达到批量上限立即写入
	if len(store.pendingEvents) >= usageWriteMaxBatch {
		return store.flushPendingEventsLocked()
	}

	// 否则启动/重置防抖定时器
	if store.debounceTimer != nil {
		store.debounceTimer.Stop()
	}
	store.debounceTimer = time.AfterFunc(time.Duration(usageWriteDebounceMs)*time.Millisecond, func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		if err := store.flushPendingEventsLocked(); err != nil {
			logger.Errorf("forwarder usage debounce flush failed path=%s err=%v", store.path, err)
		}
	})

	return nil
}

// Flush 把防抖窗口内尚未落盘的 pending 事件立即写入磁盘，供服务关闭时调用。
// 失败时事件回填 pending 并返回错误（与防抖路径一致，不静默丢弃）。
func (store *UsageFileStore) Flush() error {
	if store == nil || strings.TrimSpace(store.path) == "" {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.debounceTimer != nil {
		store.debounceTimer.Stop()
		store.debounceTimer = nil
	}
	return store.flushPendingEventsLocked()
}

// flushPendingEventsLocked 批量写入所有 pending 事件（调用方必须已持有 store.mu）。
// 事件在写盘成功后才真正出队：锁竞争/磁盘抖动等任一步失败时回填 pending 并重挂
// 防抖定时器重试，避免整批 token/花费统计被静默丢弃（防抖回调本身只打日志）。
func (store *UsageFileStore) flushPendingEventsLocked() error {
	if len(store.pendingEvents) == 0 {
		return nil
	}

	events := store.pendingEvents
	restoreOnFailure := func() {
		store.pendingEvents = events
		if store.debounceTimer != nil {
			store.debounceTimer.Stop()
		}
		store.debounceTimer = time.AfterFunc(time.Duration(usageWriteDebounceMs)*time.Millisecond, func() {
			store.mu.Lock()
			defer store.mu.Unlock()
			if err := store.flushPendingEventsLocked(); err != nil {
				logger.Errorf("forwarder usage debounce flush failed path=%s err=%v", store.path, err)
			}
		})
	}

	if err := os.MkdirAll(filepath.Dir(store.path), 0o755); err != nil {
		restoreOnFailure()
		return fmt.Errorf("create usage directory: %w", err)
	}
	release, err := acquireConversationLock(store.path + ".lock")
	if err != nil {
		restoreOnFailure()
		return err
	}
	defer release()

	doc, err := readUsageFileDocument(store.path)
	if err != nil {
		restoreOnFailure()
		return err
	}
	if doc.convertedFromSchema != 0 {
		// This write replaces a file of an older schema with its converted form.
		if err := keepUsageFileCopy(store.path, doc.convertedFromSchema); err != nil {
			restoreOnFailure()
			return err
		}
	}
	if doc.EventIndex == nil {
		doc.EventIndex = make(map[string]usageFileEvent)
	}

	// 批量应用所有事件
	for _, event := range events {
		oldEvent, found := doc.EventIndex[event.EventID]
		if found {
			applyUsageFileDelta(&doc, oldEvent.At, negateUsageFileDelta(usageFileEventDelta(oldEvent)))
		}
		applyUsageFileDelta(&doc, event.At, usageFileEventDelta(event))
		doc.RecentEvents = upsertRecentUsageEvent(doc.RecentEvents, event)
		if len(doc.RecentEvents) > usageRecentEventLimit {
			dropped := doc.RecentEvents[usageRecentEventLimit:]
			for _, item := range dropped {
				delete(doc.EventIndex, strings.TrimSpace(item.EventID))
			}
			doc.RecentEvents = doc.RecentEvents[:usageRecentEventLimit]
		}
		doc.EventIndex[event.EventID] = event
	}

	doc.UpdatedAt = time.Now().UTC()
	if err := writeJSONFileAtomic(store.path, doc); err != nil {
		restoreOnFailure()
		return err
	}
	store.pendingEvents = nil
	return nil
}

// Reset 清空所有用量统计：Totals、Daily、RecentEvents 全部归零。
// 在进程内锁和文件锁保护下停止延迟写入、丢弃 pending 事件并原子写入空文档。
func (store *UsageFileStore) Reset() error {
	if store == nil || strings.TrimSpace(store.path) == "" {
		return nil
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if store.debounceTimer != nil {
		store.debounceTimer.Stop()
		store.debounceTimer = nil
	}
	store.pendingEvents = nil

	if err := os.MkdirAll(filepath.Dir(store.path), 0o755); err != nil {
		return fmt.Errorf("create usage directory: %w", err)
	}
	release, err := acquireConversationLock(store.path + ".lock")
	if err != nil {
		return err
	}
	defer release()
	doc := usageFileDocument{
		SchemaVersion: usageFileSchemaVersion,
		UpdatedAt:     time.Now().UTC(),
		Daily:         make([]usageFileDaily, 0),
		RecentEvents:  make([]usageFileEvent, 0),
		EventIndex:    make(map[string]usageFileEvent),
	}
	return writeJSONFileAtomic(store.path, doc)
}

func (store *UsageFileStore) LookupEvent(needle string) (usageFileEvent, bool, error) {
	if store == nil || strings.TrimSpace(store.path) == "" {
		return usageFileEvent{}, false, nil
	}
	// 合并防抖窗口内尚未落盘的 pending 事件：RecordEvent 走 2 秒防抖，
	// 紧随其后的 Lookup（如 turn 收口聚合本回合用量）只读磁盘会确定性漏掉
	// 刚记录的 provider_call，聚合结果恒为 0。
	store.mu.Lock()
	pending := append([]usageFileEvent(nil), store.pendingEvents...)
	store.mu.Unlock()
	doc, err := readUsageFileDocument(store.path)
	if err != nil {
		return usageFileEvent{}, false, err
	}
	trimmed := strings.TrimSpace(needle)
	if trimmed == "" {
		return usageFileEvent{}, false, nil
	}
	var aggregate usageFileEvent
	found := false
	events := doc.EventIndex
	if len(events) == 0 {
		events = make(map[string]usageFileEvent, len(doc.RecentEvents))
		for _, event := range doc.RecentEvents {
			if eventID := strings.TrimSpace(event.EventID); eventID != "" {
				events[eventID] = event
			}
		}
	}
	matchAggregate := func(event usageFileEvent) {
		eventID := strings.TrimSpace(event.EventID)
		if eventID != trimmed && !strings.HasPrefix(eventID, trimmed+"::") {
			return
		}
		if !found {
			aggregate = usageFileEvent{EventID: trimmed, At: event.At}
			found = true
		}
		if event.At.After(aggregate.At) {
			aggregate.At = event.At
		}
		aggregate.InputTokens += nonNegativeInt64(event.InputTokens)
		aggregate.OutputTokens += nonNegativeInt64(event.OutputTokens)
		aggregate.CacheReadTokens += nonNegativeInt64(event.CacheReadTokens)
		aggregate.CacheWriteTokens += nonNegativeInt64(event.CacheWriteTokens)
		aggregate.TotalTokens += nonNegativeInt64(event.TotalTokens)
		aggregate.UsagePresent = aggregate.UsagePresent || event.UsagePresent
	}
	// A buffered write replaces the saved event of the same id, and a later
	// buffered write replaces an earlier one: that is how a flush applies them.
	// Adding them up instead counts a call once per write. A provider call is
	// written when its pass completes and again when the turn is finalized, so
	// the turn total read right after came out doubled.
	buffered := make(map[string]usageFileEvent, len(pending))
	for _, event := range pending {
		if eventID := strings.TrimSpace(event.EventID); eventID != "" {
			buffered[eventID] = event
		}
	}
	for eventID, event := range events {
		if _, replaced := buffered[eventID]; !replaced {
			matchAggregate(event)
		}
	}
	for _, event := range buffered {
		matchAggregate(event)
	}
	if found {
		return aggregate, true, nil
	}
	return usageFileEvent{}, false, nil
}

func readUsageFileDocument(path string) (usageFileDocument, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return usageFileDocument{
				SchemaVersion: usageFileSchemaVersion,
				Daily:         make([]usageFileDaily, 0),
				RecentEvents:  make([]usageFileEvent, 0),
				EventIndex:    make(map[string]usageFileEvent),
			}, nil
		}
		return usageFileDocument{}, fmt.Errorf("read usage file: %w", err)
	}
	var doc usageFileDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return usageFileDocument{}, fmt.Errorf("decode usage file: %w", err)
	}
	if doc.SchemaVersion == 0 {
		doc.SchemaVersion = 1
	}
	if doc.SchemaVersion < usageFileSchemaVersion {
		doc.convertedFromSchema = doc.SchemaVersion
		convertCursorAccountInputs(&doc)
		doc.SchemaVersion = usageFileSchemaVersion
	}
	doc.RecentEvents = trimRecentUsageEvents(doc.RecentEvents, usageRecentEventLimit)
	if len(doc.EventIndex) == 0 {
		doc.EventIndex = buildUsageEventIndex(doc.RecentEvents)
	}
	return doc, nil
}

// convertCursorAccountInputs rewrites the provider calls of a file written
// before schema 3.
//
// A call served by a Cursor account was stored with Cursor's input figure,
// which is the whole prompt with cache read and cache write inside it. Every
// other provider's input is stored without its cache tokens, and totals, cache
// hit rate and cost add the cache tokens on top. Those rows therefore counted
// their cache tokens twice, showed about half their cache hit rate, and were
// priced as if the cached part had also been sent at the input rate.
//
// Each such call keeps its cache figures and has them taken out of its input.
// Totals and the day it belongs to are lowered by the same amount. Calls of
// other providers, and calls whose input is too small to contain the cache
// tokens, are left as recorded. So is the share of the totals that belongs to
// calls no longer in the recent list: without the call there is nothing to
// work the correction out from.
func convertCursorAccountInputs(doc *usageFileDocument) {
	for index := range doc.RecentEvents {
		event := &doc.RecentEvents[index]
		if normalizeUsageEventKind(event.Kind) != usageEventKindProvider || !isCursorAccountUsageProvider(event.Provider) {
			continue
		}
		cached := nonNegativeInt64(event.CacheReadTokens) + nonNegativeInt64(event.CacheWriteTokens)
		if cached == 0 || event.InputTokens < cached {
			continue
		}
		applyUsageFileDelta(doc, event.At, usageFileDelta{inputTokens: -cached, totalTokens: -cached})
		event.InputTokens -= cached
		event.TotalTokens = event.InputTokens + nonNegativeInt64(event.OutputTokens) + cached
	}
}

// isCursorAccountUsageProvider reports whether provider is a label the Cursor
// account gateway records: cursor_account, or cursor_account followed by the
// number of an account slot. Calls on the user's own keys carry the protocol
// name instead.
func isCursorAccountUsageProvider(provider string) bool {
	slot, ok := strings.CutPrefix(strings.TrimSpace(provider), "cursor_account")
	if !ok {
		return false
	}
	for _, char := range slot {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// keepUsageFileCopy saves the usage file as it is on disk, still in the given
// schema, before a converted document replaces it. The copy is made once and
// never overwritten, so the figures as first recorded stay recoverable.
func keepUsageFileCopy(path string, schemaVersion int) error {
	copyPath := fmt.Sprintf("%s.schema-%d.bak", path, schemaVersion)
	if _, err := os.Stat(copyPath); err == nil {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read usage file for its pre-conversion copy: %w", err)
	}
	if err := os.WriteFile(copyPath, body, 0o600); err != nil {
		return fmt.Errorf("keep pre-conversion copy of usage file: %w", err)
	}
	return nil
}

func upsertRecentUsageEvent(items []usageFileEvent, event usageFileEvent) []usageFileEvent {
	event.EventID = strings.TrimSpace(event.EventID)
	if event.EventID == "" {
		return items
	}
	next := make([]usageFileEvent, 0, len(items)+1)
	next = append(next, event)
	for _, item := range items {
		if strings.TrimSpace(item.EventID) == event.EventID {
			continue
		}
		next = append(next, item)
	}
	return next
}

func trimRecentUsageEvents(items []usageFileEvent, limit int) []usageFileEvent {
	if limit <= 0 || len(items) <= limit {
		return items
	}
	return items[:limit]
}

func buildUsageEventIndex(items []usageFileEvent) map[string]usageFileEvent {
	index := make(map[string]usageFileEvent, len(items))
	for _, event := range items {
		event.EventID = strings.TrimSpace(event.EventID)
		if event.EventID == "" {
			continue
		}
		event.Kind = normalizeUsageEventKind(event.Kind)
		index[event.EventID] = event
	}
	return index
}

func normalizeUsageEventKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case usageEventKindTurn:
		return usageEventKindTurn
	default:
		return usageEventKindProvider
	}
}

func usageFileEventDelta(event usageFileEvent) usageFileDelta {
	switch normalizeUsageEventKind(event.Kind) {
	case usageEventKindTurn:
		delta := usageFileDelta{turnsTotal: 1}
		if strings.TrimSpace(event.Status) == usageTurnStatusDone {
			delta.validTurnsTotal = 1
		} else {
			delta.invalidTurnsTotal = 1
		}
		return delta
	default:
		return usageFileDelta{
			providerCalls:    1,
			inputTokens:      nonNegativeInt64(event.InputTokens),
			outputTokens:     nonNegativeInt64(event.OutputTokens),
			cacheReadTokens:  nonNegativeInt64(event.CacheReadTokens),
			cacheWriteTokens: nonNegativeInt64(event.CacheWriteTokens),
			totalTokens:      nonNegativeInt64(event.TotalTokens),
		}
	}
}

func negateUsageFileDelta(value usageFileDelta) usageFileDelta {
	return usageFileDelta{
		providerCalls:     -value.providerCalls,
		turnsTotal:        -value.turnsTotal,
		validTurnsTotal:   -value.validTurnsTotal,
		invalidTurnsTotal: -value.invalidTurnsTotal,
		inputTokens:       -value.inputTokens,
		outputTokens:      -value.outputTokens,
		cacheReadTokens:   -value.cacheReadTokens,
		cacheWriteTokens:  -value.cacheWriteTokens,
		totalTokens:       -value.totalTokens,
	}
}

func applyUsageFileDelta(doc *usageFileDocument, at time.Time, delta usageFileDelta) {
	if doc == nil {
		return
	}
	doc.Totals.ProviderCalls = clampNonNegativeInt64(doc.Totals.ProviderCalls + delta.providerCalls)
	doc.Totals.TurnsTotal = clampNonNegativeInt64(doc.Totals.TurnsTotal + delta.turnsTotal)
	doc.Totals.ValidTurnsTotal = clampNonNegativeInt64(doc.Totals.ValidTurnsTotal + delta.validTurnsTotal)
	doc.Totals.InvalidTurnsTotal = clampNonNegativeInt64(doc.Totals.InvalidTurnsTotal + delta.invalidTurnsTotal)
	doc.Totals.InputTokens = clampNonNegativeInt64(doc.Totals.InputTokens + delta.inputTokens)
	doc.Totals.OutputTokens = clampNonNegativeInt64(doc.Totals.OutputTokens + delta.outputTokens)
	doc.Totals.CacheReadTokens = clampNonNegativeInt64(doc.Totals.CacheReadTokens + delta.cacheReadTokens)
	doc.Totals.CacheWriteTokens = clampNonNegativeInt64(doc.Totals.CacheWriteTokens + delta.cacheWriteTokens)
	doc.Totals.TotalTokens = clampNonNegativeInt64(doc.Totals.TotalTokens + delta.totalTokens)

	date := at.UTC().Format("2006-01-02")
	for index := range doc.Daily {
		if doc.Daily[index].Date != date {
			continue
		}
		applyUsageDailyDelta(&doc.Daily[index], delta)
		return
	}
	item := usageFileDaily{Date: date}
	applyUsageDailyDelta(&item, delta)
	doc.Daily = append(doc.Daily, item)
}

func applyUsageDailyDelta(item *usageFileDaily, delta usageFileDelta) {
	if item == nil {
		return
	}
	item.ProviderCalls = clampNonNegativeInt64(item.ProviderCalls + delta.providerCalls)
	item.TurnsTotal = clampNonNegativeInt64(item.TurnsTotal + delta.turnsTotal)
	item.ValidTurnsTotal = clampNonNegativeInt64(item.ValidTurnsTotal + delta.validTurnsTotal)
	item.InvalidTurnsTotal = clampNonNegativeInt64(item.InvalidTurnsTotal + delta.invalidTurnsTotal)
	item.InputTokens = clampNonNegativeInt64(item.InputTokens + delta.inputTokens)
	item.OutputTokens = clampNonNegativeInt64(item.OutputTokens + delta.outputTokens)
	item.CacheReadTokens = clampNonNegativeInt64(item.CacheReadTokens + delta.cacheReadTokens)
	item.CacheWriteTokens = clampNonNegativeInt64(item.CacheWriteTokens + delta.cacheWriteTokens)
	item.TotalTokens = clampNonNegativeInt64(item.TotalTokens + delta.totalTokens)
}

func clampNonNegativeInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
