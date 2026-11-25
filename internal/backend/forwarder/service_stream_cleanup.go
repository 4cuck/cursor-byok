package forwarder

import "strings"

const provider400RecoveryMaxEntries = 512

func (service *Service) registerStreamLifecycleHooks() {
	if service == nil || service.broker == nil {
		return
	}
	service.broker.SetStreamRemovedHook(service.handleStreamRemoved)
	if service.appendSeq != nil {
		service.appendSeq.live = service.requestStreamIsOpen
	}
}

// requestStreamIsOpen reports whether the request has a stream that has not
// ended. The append sequence of such a request must not be forgotten.
func (service *Service) requestStreamIsOpen(requestID string) bool {
	if service == nil || service.broker == nil {
		return false
	}
	stream, ok := service.broker.Get(requestID)
	if !ok || stream == nil {
		return false
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return !isTerminalStreamStatus(stream.Status)
}

func (service *Service) handleStreamRemoved(info StreamRemovedInfo) {
	if service == nil {
		return
	}
	conversationID := strings.TrimSpace(info.ConversationID)
	requestID := strings.TrimSpace(info.RequestID)
	if conversationID != "" {
		service.clearConversationActivity(conversationID)
	}
	if requestID != "" {
		service.purgeProvider400RecoveryForRequest(requestID)
	}
	if conversationID != "" && requestID != "" && service.runQueue != nil &&
		service.runQueue.IsOwner(conversationID, requestID) {
		service.finishConversationTurn(conversationID, requestID)
	}
}

func (service *Service) reconcileStaleRunQueueOwner(conversationID string) {
	if service == nil || service.runQueue == nil || service.broker == nil {
		return
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return
	}
	ownerRequestID := service.runQueue.Owner(conversationID)
	if ownerRequestID == "" {
		return
	}
	if _, ok := service.broker.Get(ownerRequestID); ok {
		return
	}
	service.finishConversationTurn(conversationID, ownerRequestID)
}

func (service *Service) clearConversationActivity(conversationID string) {
	conversationID = strings.TrimSpace(conversationID)
	if service == nil || conversationID == "" {
		return
	}
	service.conversationActivityMu.Lock()
	delete(service.conversationLastActivity, conversationID)
	service.conversationActivityMu.Unlock()
}

func (service *Service) purgeProvider400RecoveryForRequest(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if service == nil || requestID == "" {
		return
	}
	prefix := requestID + ":"
	service.provider400RecoveryMu.Lock()
	defer service.provider400RecoveryMu.Unlock()
	if service.provider400RecoveryTurns == nil {
		return
	}
	for key := range service.provider400RecoveryTurns {
		if key == requestID || strings.HasPrefix(key, prefix) {
			delete(service.provider400RecoveryTurns, key)
		}
	}
}

func (service *Service) evictProvider400RecoveryLocked(maxDelete int) {
	if service == nil || service.provider400RecoveryTurns == nil || maxDelete <= 0 {
		return
	}
	for key := range service.provider400RecoveryTurns {
		delete(service.provider400RecoveryTurns, key)
		maxDelete--
		if maxDelete <= 0 {
			return
		}
	}
}
