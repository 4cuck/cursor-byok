package cursoraccount

import (
	"fmt"
	"strings"
	"time"

	"cursor/internal/controlcenter"
)

// CursorAccountLoginSession is the public start handle for browser login.
type CursorAccountLoginSession struct {
	SessionID       string `json:"sessionId"`
	State           string `json:"state"`
	ExpiresAtUnixMS int64  `json:"expiresAtUnixMs"`
}

// CursorAccountLoginStatus is the public poll snapshot for browser login.
type CursorAccountLoginStatus struct {
	SessionID string `json:"sessionId"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

func (manager *Manager) BeginLogin() (CursorAccountLoginSession, error) {
	status, err := manager.StartLogin()
	if err != nil {
		return CursorAccountLoginSession{}, err
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return CursorAccountLoginSession{
		SessionID:       manager.loginSessionID,
		State:           status.State,
		ExpiresAtUnixMS: manager.loginExpiresAt,
	}, nil
}

func (manager *Manager) LoginStatus(sessionID string) (CursorAccountLoginStatus, error) {
	if manager == nil {
		return CursorAccountLoginStatus{}, fmt.Errorf("Cursor account service is not initialized")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return CursorAccountLoginStatus{}, fmt.Errorf("Invalid login session")
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.loginSessionID != sessionID {
		return CursorAccountLoginStatus{}, fmt.Errorf("Invalid login session")
	}
	if manager.loginExpiresAt > 0 && time.Now().UnixMilli() > manager.loginExpiresAt && manager.state == StateWaiting {
		return CursorAccountLoginStatus{SessionID: sessionID, State: StateError, Error: "Sign-in timed out"}, nil
	}
	return CursorAccountLoginStatus{
		SessionID: sessionID,
		State:     manager.state,
		Error:     manager.lastError,
	}, nil
}

func (manager *Manager) CancelLogin(sessionID string) (controlcenter.OperationResult, error) {
	if manager == nil {
		return controlcenter.OperationResult{}, fmt.Errorf("Cursor account service is not initialized")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return controlcenter.OperationResult{}, fmt.Errorf("Invalid login session")
	}
	manager.mu.Lock()
	if manager.loginSessionID != sessionID {
		manager.mu.Unlock()
		return controlcenter.OperationResult{}, fmt.Errorf("Invalid login session")
	}
	cancel := manager.loginCancel
	manager.loginCancel = nil
	manager.loginGeneration++
	manager.loginSessionID = ""
	manager.loginExpiresAt = 0
	if manager.state == StateWaiting {
		manager.state = StateSignedOut
		manager.lastError = ""
	}
	manager.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return controlcenter.OperationResult{
		OperationID:      sessionID,
		State:            "succeeded",
		FinishedAtUnixMS: time.Now().UnixMilli(),
	}, nil
}
