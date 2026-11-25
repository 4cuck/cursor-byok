package modeladapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"cursor/gen/aiserverv1"

	"google.golang.org/protobuf/proto"
)

// serverConfigAPI is a stand-in for api2 that answers GetServerConfig and
// counts how often it was asked.
func serverConfigAPI(t *testing.T, agentHost string, status *atomic.Int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cursorAccountServerConfigPath {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		if status != nil && status.Load() != 0 {
			w.WriteHeader(int(status.Load()))
			return
		}
		raw, _ := proto.Marshal(&aiserverv1.GetServerConfigResponse{
			AgentUrlConfig: &aiserverv1.AgentUrlConfig{AgentnUrl: agentHost},
		})
		_, _ = w.Write(raw)
	}))
	t.Cleanup(api.Close)
	previous := cursorAccountBackendRoot
	cursorAccountBackendRoot = api.URL
	t.Cleanup(func() { cursorAccountBackendRoot = previous })
	return api, hits
}

// Every model pass used to ask api2 where the agent host is before it could
// open its stream: one extra round trip in front of every first token. The
// answer does not change from one pass to the next.
func TestCursorAccountAgentHostIsLookedUpOncePerAccount(t *testing.T) {
	_, hits := serverConfigAPI(t, "https://agent.example", nil)
	ctx := context.Background()
	gateway := &cursorAccountGateway{}

	for i := 0; i < 3; i++ {
		if got := gateway.agentHosts.host(ctx, "Bearer token-a"); got != "https://agent.example" {
			t.Fatalf("pass %d: agent host = %q, want https://agent.example", i, got)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("GetServerConfig was requested %d times for 3 passes on one account, want 1", got)
	}

	// Another account is a different question.
	if got := gateway.agentHosts.host(ctx, "Bearer token-b"); got != "https://agent.example" {
		t.Fatalf("second account: agent host = %q", got)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("GetServerConfig requests = %d after a second account, want 2", got)
	}
}

// A failed lookup falls back to the default host for that pass only.
func TestCursorAccountAgentHostDoesNotRememberAFailedLookup(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusServiceUnavailable)
	_, hits := serverConfigAPI(t, "https://agent.example", status)
	ctx := context.Background()
	gateway := &cursorAccountGateway{}

	if got := gateway.agentHosts.host(ctx, "Bearer token-c"); got != cursorAccountAgentHostDefault {
		t.Fatalf("failed lookup: agent host = %q, want the default", got)
	}

	status.Store(0)
	if got := gateway.agentHosts.host(ctx, "Bearer token-c"); got != "https://agent.example" {
		t.Fatalf("after recovery: agent host = %q, want the looked-up host (the failure must not be cached)", got)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("GetServerConfig requests = %d, want 2", got)
	}
}

// When the stream cannot be opened on the remembered host, the next pass asks
// again instead of repeating the same host for the rest of the cache lifetime.
func TestCursorAccountAgentHostIsLookedUpAgainAfterBeingForgotten(t *testing.T) {
	_, hits := serverConfigAPI(t, "https://agent.example", nil)
	ctx := context.Background()
	gateway := &cursorAccountGateway{}

	_ = gateway.agentHosts.host(ctx, "Bearer token-d")
	gateway.agentHosts.forget("Bearer token-d")
	_ = gateway.agentHosts.host(ctx, "Bearer token-d")

	if got := hits.Load(); got != 2 {
		t.Fatalf("GetServerConfig requests = %d, want 2: a forgotten host must be looked up again", got)
	}
}
