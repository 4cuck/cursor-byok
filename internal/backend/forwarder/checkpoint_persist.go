package forwarder

import (
	"strings"
	"time"
)

const checkpointPersistDebounce = 250 * time.Millisecond

func maxEntrySeq(entries []HistoryEntry) int64 {
	var maxSeq int64
	for _, entry := range entries {
		if entry.Seq > maxSeq {
			maxSeq = entry.Seq
		}
	}
	return maxSeq
}

func syncCheckpointPersistBaseline(stream *ActiveStream, conversation *ConversationFile) {
	if stream == nil {
		return
	}
	stream.CheckpointLastPersistedEntrySeq = maxEntrySeq(conversationEntries(conversation))
}

func conversationEntries(conversation *ConversationFile) []HistoryEntry {
	if conversation == nil {
		return nil
	}
	return conversation.Entries
}

func (service *Service) scheduleCheckpointPersist(stream *ActiveStream, conversationID string) {
	if service == nil || service.store == nil || stream == nil {
		return
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return
	}
	stream.mu.Lock()
	if stream.CheckpointPersistTimer != nil {
		stream.CheckpointPersistTimer.Stop()
	}
	conversationIDCopy := conversationID
	stream.CheckpointPersistTimer = time.AfterFunc(checkpointPersistDebounce, func() {
		_ = service.persistCheckpointDelta(stream, conversationIDCopy)
	})
	stream.mu.Unlock()
}

func (service *Service) flushCheckpointPersistSync(stream *ActiveStream, conversationID string) error {
	if service == nil || service.store == nil || stream == nil {
		return nil
	}
	stream.mu.Lock()
	if stream.CheckpointPersistTimer != nil {
		stream.CheckpointPersistTimer.Stop()
		stream.CheckpointPersistTimer = nil
	}
	stream.mu.Unlock()
	return service.persistCheckpointDelta(stream, conversationID)
}

func (service *Service) persistCheckpointDelta(stream *ActiveStream, conversationID string) error {
	if service == nil || service.store == nil || stream == nil {
		return nil
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil
	}

	// 全程持有持久化互斥（锁序 persistMu → stream.mu）：定时器回调与同步 flush
	// 并发进入时，后进入方会在前者完成后重新快照 CheckpointLastPersistedEntrySeq，
	// pending 为空直接跳过落盘；否则两侧会各写一遍同一批条目（磁盘重复 + 内存
	// checkpoint 被含重复条目的版本覆盖）。
	stream.CheckpointPersistMu.Lock()
	defer stream.CheckpointPersistMu.Unlock()

	stream.mu.Lock()
	if stream.CheckpointConversation == nil {
		stream.mu.Unlock()
		return nil
	}
	conversation := cloneConversationFile(stream.CheckpointConversation)
	lastPersisted := stream.CheckpointLastPersistedEntrySeq
	// Everything up to snapshotMaxSeq is what this run can account for. The
	// actor keeps appending while the store is being written; those entries
	// have higher sequence numbers and belong to the next run.
	snapshotMaxSeq := maxEntrySeq(conversation.Entries)
	var pending []HistoryEntry
	for _, entry := range conversation.Entries {
		if entry.Seq > lastPersisted {
			pending = append(pending, entry)
		}
	}
	stream.mu.Unlock()

	if len(pending) > 0 {
		persisted, assigned, err := service.store.AppendEntries(conversationID, resetEntrySequences(pending))
		if err != nil {
			return err
		}
		stream.mu.Lock()
		if stream.CheckpointConversation != nil && persisted != nil {
			if len(stream.CheckpointConversation.Entries) >= len(persisted.Entries) {
				mergeConversationMetadata(stream.CheckpointConversation, persisted)
			} else {
				stream.CheckpointConversation = cloneConversationFile(persisted)
			}
		}
		stream.CheckpointLastPersistedEntrySeq = maxEntrySeq(assigned)
		conversation = cloneConversationFile(stream.CheckpointConversation)
		stream.mu.Unlock()
	}

	if conversation == nil {
		return nil
	}
	if err := service.syncConversationRecord(conversationID, conversation); err != nil {
		return err
	}

	stream.mu.Lock()
	if len(pending) == 0 {
		// Re-baseline (a compaction may have renumbered the entries), but only
		// up to what this run saw. Using the current maximum here would count
		// entries appended during the write as persisted, and they would never
		// reach the store.
		stream.CheckpointLastPersistedEntrySeq = snapshotMaxSeq
	}
	appendedDuringRun := maxEntrySeq(conversationEntries(stream.CheckpointConversation)) > snapshotMaxSeq
	timerArmed := stream.CheckpointPersistTimer != nil
	if !appendedDuringRun && timerArmed {
		stream.CheckpointPersistTimer.Stop()
		stream.CheckpointPersistTimer = nil
	}
	stream.mu.Unlock()
	if appendedDuringRun {
		// Those entries scheduled their own run; make sure it happens. The
		// timer that led to this run has fired and does not count.
		service.scheduleCheckpointPersist(stream, conversationID)
	}
	return nil
}
