package modeladapter

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"cursor/internal/logger"
)

// cursorAccountAppendFunc sends one client frame of an open RunSSE stream.
type cursorAccountAppendFunc func(ctx context.Context, seq int64, payload []byte) error

// cursorAccountAppendError is a failed BidiAppend. status is the HTTP status,
// or 0 when no response arrived.
type cursorAccountAppendError struct {
	status int
	err    error
}

func (e *cursorAccountAppendError) Error() string { return e.err.Error() }

func (e *cursorAccountAppendError) Unwrap() error { return e.err }

// The IDE's BidiTransport tries a frame up to five times with full-jitter
// backoff (1s doubling, 30s cap) and keeps the seqno, so the server drops a
// frame it already has. Tests shorten the backoff.
const cursorAccountAppendAttempts = 5

var cursorAccountAppendBackoff = func(failures int) time.Duration {
	ceiling := 30 * time.Second
	if failures < 6 {
		ceiling = min(time.Second<<(failures-1), ceiling)
	}
	return rand.N(ceiling)
}

// cursorAccountAuthRecheck is how often a stream asks the account manager for
// its current token. The manager renews five minutes before expiry, so a turn
// that runs past the token's lifetime keeps signing frames with a live token.
var cursorAccountAuthRecheck = 30 * time.Second

// cursorAccountStreamAppender delivers the client half of one RunSSE stream.
// A long turn outlives the access token it started with: frames use the
// manager's current token, a 401 renews it once, and transient failures are
// retried the way the IDE retries them.
type cursorAccountStreamAppender struct {
	gateway   *cursorAccountGateway
	accountID string
	requestID string

	mu            sync.Mutex
	authorization string
	checkedAt     time.Time
}

func (gateway *cursorAccountGateway) newStreamAppender(accountID, requestID, authorization string) *cursorAccountStreamAppender {
	return &cursorAccountStreamAppender{
		gateway:       gateway,
		accountID:     accountID,
		requestID:     requestID,
		authorization: authorization,
		checkedAt:     time.Now(),
	}
}

func (appender *cursorAccountStreamAppender) append(ctx context.Context, seq int64, payload []byte) error {
	authorization := appender.current(ctx)
	renewed := false
	failures := 0
	for {
		err := appendCursorAccount(ctx, authorization, appender.requestID, seq, payload)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cursorAccountAuthRejected(err) {
			if renewed {
				return err
			}
			renewed = true
			next, renewErr := appender.renew(ctx, authorization)
			if renewErr != nil {
				logger.Warnf("cursor account append token was rejected and not renewed request_id=%s seq=%d: %v", appender.requestID, seq, renewErr)
				return err
			}
			logger.Infof("cursor account append renewed the access token request_id=%s seq=%d", appender.requestID, seq)
			authorization = next
			continue
		}
		failures++
		if !cursorAccountAppendRetryable(err) || failures >= cursorAccountAppendAttempts {
			return err
		}
		delay := cursorAccountAppendBackoff(failures)
		logger.Warnf("cursor account append failed request_id=%s seq=%d attempt=%d/%d; retrying in %s: %v", appender.requestID, seq, failures, cursorAccountAppendAttempts, delay, err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		authorization = appender.current(ctx)
	}
}

// current returns the token for the next frame, asking the account manager
// again once cursorAccountAuthRecheck has passed.
func (appender *cursorAccountStreamAppender) current(ctx context.Context) string {
	appender.mu.Lock()
	defer appender.mu.Unlock()
	if time.Since(appender.checkedAt) < cursorAccountAuthRecheck {
		return appender.authorization
	}
	appender.checkedAt = time.Now()
	fresh, err := appender.gateway.authorize(ctx, appender.accountID)
	if err != nil {
		logger.Warnf("cursor account append keeps the previous token request_id=%s: %v", appender.requestID, err)
		return appender.authorization
	}
	if strings.TrimSpace(fresh) != "" {
		appender.authorization = fresh
	}
	return appender.authorization
}

// renew returns a token other than the rejected one. Another frame or the
// manager may already hold one; otherwise an API-key account exchanges its
// key again, as the IDE does when its token stops working.
func (appender *cursorAccountStreamAppender) renew(ctx context.Context, rejected string) (string, error) {
	appender.mu.Lock()
	defer appender.mu.Unlock()
	if appender.authorization != rejected && strings.TrimSpace(appender.authorization) != "" {
		return appender.authorization, nil
	}
	appender.checkedAt = time.Now()
	if fresh, err := appender.gateway.authorize(ctx, appender.accountID); err == nil && fresh != rejected && strings.TrimSpace(fresh) != "" {
		appender.authorization = fresh
		return fresh, nil
	}
	if !cursorAccountUsesAPIKey(appender.gateway.auth, appender.accountID) {
		return "", errors.New("the account manager has no newer token")
	}
	fresh, err := cursorAccountForceRefresh(ctx, appender.gateway.auth, appender.accountID)
	if err != nil {
		return "", err
	}
	appender.authorization = fresh
	return fresh, nil
}

// cursorAccountAppendRetryable follows the IDE's status mapping: HTTP 400
// becomes Internal and 429/502/503/504 become Unavailable, both retried. A
// request that got no response is a transport failure and is retried too.
func cursorAccountAppendRetryable(err error) bool {
	var appendErr *cursorAccountAppendError
	if !errors.As(err, &appendErr) {
		return false
	}
	switch appendErr.status {
	case 0, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// cursorAccountAppendRefusedPayload reports an append status meaning Cursor
// read the frame and refused its content, as it does for an oversized or
// malformed conversation state.
func cursorAccountAppendRefusedPayload(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}
