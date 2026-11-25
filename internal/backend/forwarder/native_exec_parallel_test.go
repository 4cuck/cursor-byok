package forwarder

import (
	"testing"

	"cursor/gen/agentv1"
)

// A model that asks for several tools in one step has them dispatched back to
// back. Each exec sent to the IDE has to stay awaited until its own result
// comes back. The Cursor account stream waits for every result and does not
// time out on its own, so one lost result leaves the turn hanging until the
// user cancels it.
func TestNativeReadsDispatchedBackToBackBothReturnTheirResults(t *testing.T) {
	service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)

	files := []struct{ callID, path, content string }{
		{"call-read-token-usage", `c:\repo\token_usage.go`, "package forwarder // token usage\n"},
		{"call-read-provider-cache", `c:\repo\provider_cache.go`, "package forwarder // provider cache\n"},
	}
	for index, file := range files {
		_, invocation := nativeReadInvocation(t, uint32(11+index), "upstream-"+file.callID,
			&agentv1.ReadArgs{Path: file.path, ToolCallId: file.callID}, file.callID, false)
		if err := service.dispatchNativeCursorExec(stream, invocation); err != nil {
			t.Fatalf("dispatchNativeCursorExec(%s) error = %v", file.path, err)
		}
	}
	execs := writeFixtureExecs(stream)
	if len(execs) != len(files) {
		t.Fatalf("execs sent to the IDE = %d, want %d", len(execs), len(files))
	}

	// The IDE answers by message id alone, in whatever order the reads finish.
	for index := len(execs) - 1; index >= 0; index-- {
		if err := service.handleExecResult(InboundIntent{
			RequestID: stream.RequestID,
			ExecClientMessage: &agentv1.ExecClientMessage{
				Id:      execs[index].GetId(),
				Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: readSuccess(files[index].content)},
			},
		}); err != nil {
			t.Fatalf("handleExecResult(%s) error = %v", files[index].path, err)
		}
	}

	returned := map[string]string{}
	for range files {
		select {
		case reply := <-stream.CursorToolResults:
			returned[reply.GetExecId()] = reply.GetReadResult().GetSuccess().GetContent()
		default:
			t.Fatalf("upstream received %d of %d read results: the missing one is awaited forever", len(returned), len(files))
		}
	}
	for _, file := range files {
		if got := returned["upstream-"+file.callID]; got != file.content {
			t.Fatalf("upstream result for %s = %q, want %q", file.path, got, file.content)
		}
	}
	stream.mu.Lock()
	stillAwaited := len(stream.PendingExecs)
	stream.mu.Unlock()
	if stillAwaited != 0 {
		t.Fatalf("execs still awaited after both results arrived = %d, want 0", stillAwaited)
	}
}
