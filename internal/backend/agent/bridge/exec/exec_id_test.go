package execbridge

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
)

// Pending execs are keyed by exec id. A model that asks for several tools in
// one step has them opened back to back, and on Windows the wall clock does
// not advance between two such opens. An id taken from the clock alone then
// repeats, the second exec replaces the first as the awaited one, and the
// first result matches nothing: the turn hangs until the user cancels it.

// holdExecIDClock stops the clock NewExecID reads, the way the Windows clock
// stands still between two timer ticks. It stops at an instant the real clock
// cannot reach, so an id built from the real clock is told apart on every
// platform, including the ones whose clock would never repeat.
func holdExecIDClock(t *testing.T) int64 {
	t.Helper()
	heldAt := time.Date(2200, time.January, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	clock, last := execIDClock, lastExecIDStamp.Load()
	execIDClock = func() int64 { return heldAt }
	t.Cleanup(func() {
		execIDClock = clock
		lastExecIDStamp.Store(last)
	})
	return heldAt
}

func requireFreshExecIDs(t *testing.T, heldAt int64, ids ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("two execs opened back to back share the id %q: the second replaces the first as the awaited exec and the first result is lost", id)
		}
		seen[id] = true
		stamp, err := strconv.ParseInt(id[strings.LastIndex(id, "-")+1:], 10, 64)
		if err != nil || stamp < heldAt {
			t.Fatalf("exec id %q was not issued by NewExecID, so it repeats whenever the clock stands still", id)
		}
	}
}

func TestNewExecIDStaysUniqueWhileTheClockStandsStill(t *testing.T) {
	heldAt := holdExecIDClock(t)

	ids := make([]string, 0, 1000)
	for range 1000 {
		id := NewExecID("exec-read-")
		if !strings.HasPrefix(id, "exec-read-") {
			t.Fatalf("exec id %q lost its prefix", id)
		}
		ids = append(ids, id)
	}

	requireFreshExecIDs(t, heldAt, ids...)
}

func TestNewExecIDStaysUniqueAcrossGoroutines(t *testing.T) {
	heldAt := holdExecIDClock(t)
	const workers, perWorker = 8, 500

	issued := make(chan string, workers*perWorker)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for range perWorker {
				issued <- NewExecID(nativeExecIDPrefix)
			}
		}()
	}
	group.Wait()
	close(issued)

	ids := make([]string, 0, workers*perWorker)
	for id := range issued {
		ids = append(ids, id)
	}
	requireFreshExecIDs(t, heldAt, ids...)
}

// Execs forwarded from a Cursor account all share one prefix, so any two of
// them collide, whatever tools they are.
func TestNativeExecsOpenedBackToBackGetDistinctIDs(t *testing.T) {
	heldAt := holdExecIDClock(t)
	bridge := NewBridge()

	upstream := []*agentv1.ExecServerMessage{
		{Id: 11, ExecId: "upstream-read-1", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: `c:\repo\token_usage.go`}}},
		{Id: 12, ExecId: "upstream-read-2", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: `c:\repo\provider_cache.go`}}},
		{Id: 13, ExecId: "upstream-grep", Message: &agentv1.ExecServerMessage_GrepArgs{GrepArgs: &agentv1.GrepArgs{Pattern: "cache_read"}}},
	}
	ids := make([]string, 0, len(upstream))
	for index, exec := range upstream {
		message, pending, err := bridge.OpenNativeExec(exec, runtimecore.ToolInvocation{CallID: fmt.Sprintf("call-%d", index)})
		if err != nil {
			t.Fatalf("OpenNativeExec() error = %v", err)
		}
		if sent := message.GetExecServerMessage().GetExecId(); sent != pending.ExecID {
			t.Fatalf("exec sent as %q but awaited as %q", sent, pending.ExecID)
		}
		ids = append(ids, pending.ExecID)
	}

	requireFreshExecIDs(t, heldAt, ids...)
}

// A model on the user's own API key reaches the same map through OpenExec,
// where two calls of the same tool share a prefix.
func TestModelToolCallsOpenedBackToBackGetDistinctIDs(t *testing.T) {
	tools := []runtimecore.ToolInvocation{
		{ToolName: "Read", ArgsJSON: []byte(`{"path":"c:\\repo\\token_usage.go"}`)},
		{ToolName: "Grep", ArgsJSON: []byte(`{"pattern":"cache_read"}`)},
		{ToolName: "Glob", ArgsJSON: []byte(`{"glob_pattern":"**/*.go"}`)},
		{ToolName: "Shell", ArgsJSON: []byte(`{"command":"go test ./..."}`)},
		{ToolName: "Delete", ArgsJSON: []byte(`{"path":"c:\\repo\\scratch.txt"}`)},
	}
	for _, tool := range tools {
		t.Run(tool.ToolName, func(t *testing.T) {
			heldAt := holdExecIDClock(t)
			bridge := NewBridge()

			ids := make([]string, 0, 2)
			for call := 1; call <= 2; call++ {
				tool.CallID = fmt.Sprintf("call-%d", call)
				message, pending, err := bridge.OpenExec(OpenExecContext{ConversationID: "conv"}, tool)
				if err != nil {
					t.Fatalf("OpenExec() error = %v", err)
				}
				if sent := message.GetExecServerMessage().GetExecId(); sent != pending.ExecID {
					t.Fatalf("exec sent as %q but awaited as %q", sent, pending.ExecID)
				}
				ids = append(ids, pending.ExecID)
			}

			requireFreshExecIDs(t, heldAt, ids...)
		})
	}
}

func TestExecuteHooksOpenedBackToBackGetDistinctIDs(t *testing.T) {
	heldAt := holdExecIDClock(t)
	bridge := NewBridge()

	ids := make([]string, 0, 2)
	for range 2 {
		_, pending, err := bridge.OpenExecuteHook(&agentv1.ExecuteHookRequest{}, "execute_hook_pre_compact")
		if err != nil {
			t.Fatalf("OpenExecuteHook() error = %v", err)
		}
		ids = append(ids, pending.ExecID)
	}

	requireFreshExecIDs(t, heldAt, ids...)
}
