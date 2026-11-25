package modeladapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// cursorAccountAgentHostTTL is how long a looked-up agent host is reused. The
// host comes from GetServerConfig and does not change between model passes;
// asking for it before every pass put one extra round trip in front of every
// first token.
const cursorAccountAgentHostTTL = 10 * time.Minute

type cursorAccountAgentHostEntry struct {
	host      string
	fetchedAt time.Time
}

// cursorAccountAgentHostCache remembers the agent host per credential for one
// gateway. The zero value is ready to use.
type cursorAccountAgentHostCache struct {
	mu      sync.Mutex
	entries map[string]cursorAccountAgentHostEntry
}

// cursorAccountAgentHostKey scopes a remembered host to one backend and one
// credential, without keeping the credential itself as a map key.
func cursorAccountAgentHostKey(authorization string) string {
	sum := sha256.Sum256([]byte(cursorAccountBackendRoot + "\x00" + authorization))
	return hex.EncodeToString(sum[:])
}

// host returns the agent host for this credential. A looked-up host is
// remembered for a while; a failed lookup is not, and falls back to the
// default host for this pass only.
func (cache *cursorAccountAgentHostCache) host(ctx context.Context, authorization string) string {
	key := cursorAccountAgentHostKey(authorization)
	cache.mu.Lock()
	entry, ok := cache.entries[key]
	cache.mu.Unlock()
	if ok && time.Since(entry.fetchedAt) < cursorAccountAgentHostTTL {
		return entry.host
	}
	host, ok := lookupCursorAccountAgentHost(ctx, authorization)
	if !ok {
		return cursorAccountAgentHostDefault
	}
	cache.mu.Lock()
	if cache.entries == nil {
		cache.entries = make(map[string]cursorAccountAgentHostEntry)
	}
	cache.entries[key] = cursorAccountAgentHostEntry{host: host, fetchedAt: time.Now()}
	cache.mu.Unlock()
	return host
}

// forget drops the remembered host for a credential so the next pass looks it
// up again. Used when a stream could not be opened on the remembered host.
func (cache *cursorAccountAgentHostCache) forget(authorization string) {
	cache.mu.Lock()
	delete(cache.entries, cursorAccountAgentHostKey(authorization))
	cache.mu.Unlock()
}
