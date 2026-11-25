package modeladapter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Keep the synthetic upstream open until every exec reply (and stream_close)
// has been appended. The text frame is a barrier after all incoming requests.
func runCursorWireExchange(t *testing.T, requests, results []string, answerQueries bool) ([]ModelEvent, []*agentv1.AgentClientMessage) {
	t.Helper()
	var input bytes.Buffer
	for _, raw := range append(append([]string{}, requests...), `{"interactionUpdate":{"textDelta":{"text":"ready"}}}`) {
		m := &agentv1.AgentServerMessage{}
		if err := protojson.Unmarshal([]byte(raw), m); err != nil {
			t.Fatal(err)
		}
		writeCursorAgentFrame(&input, m)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	execs := make(chan *agentv1.ExecClientMessage, len(results))
	queries := make(chan *agentv1.InteractionResponse)
	queryAppended := make(chan struct{}, 1)
	var calls []ModelEvent
	// Frames are sent without waiting for one another and may arrive in any
	// order. They are recorded with their sequence number and read back in
	// that order, which is the order the server processes them in.
	recorder := &appendRecorder{}
	appendFrame := func(ctx context.Context, seq int64, payload []byte) error {
		if err := recorder.append(ctx, seq, payload); err != nil {
			return err
		}
		m := &agentv1.AgentClientMessage{}
		if proto.Unmarshal(payload, m) == nil && m.GetInteractionResponse() != nil {
			queryAppended <- struct{}{}
		}
		return nil
	}
	err := readCursorAccountStream(ctx, appendFrame, "req", "conv", "model", "cursor", 0, io.MultiReader(&input, reader), nil, execs, queries, nil, func(e ModelEvent) error {
		if e.ToolInvocation != nil {
			calls = append(calls, e)
		}
		if e.Text != "ready" {
			return nil
		}
		if answerQueries {
			for i := len(calls) - 1; i >= 0; i-- {
				if len(calls[i].ToolInvocation.NativeInteraction) == 0 {
					continue
				}
				q := &agentv1.InteractionQuery{}
				if err := proto.Unmarshal(calls[i].ToolInvocation.NativeInteraction, q); err != nil {
					return err
				}
				response := &agentv1.InteractionResponse{Id: q.Id}
				if q.GetWebFetchRequestQuery() != nil {
					response.Result = &agentv1.InteractionResponse_WebFetchRequestResponse{WebFetchRequestResponse: &agentv1.WebFetchRequestResponse{Result: &agentv1.WebFetchRequestResponse_Approved_{Approved: &agentv1.WebFetchRequestResponse_Approved{}}}}
				} else {
					response.Result = &agentv1.InteractionResponse_WebSearchRequestResponse{WebSearchRequestResponse: &agentv1.WebSearchRequestResponse{Result: &agentv1.WebSearchRequestResponse_Approved_{Approved: &agentv1.WebSearchRequestResponse_Approved{}}}}
				}
				select {
				case queries <- response:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-queryAppended:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		for _, raw := range results {
			m := &agentv1.ExecClientMessage{}
			if err := protojson.Unmarshal([]byte(raw), m); err != nil {
				return err
			}
			execs <- m
		}
		close(execs)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	delivered := recorder.delivered()
	frames := make([]*agentv1.AgentClientMessage, 0, len(delivered))
	for index, frame := range delivered {
		if want := int64(index + 1); frame.seq != want {
			t.Fatalf("frame %d has sequence number %d, want %d", index, frame.seq, want)
		}
		frames = append(frames, frame.msg)
	}
	return calls, frames
}

func assertCursorWireFrames(t *testing.T, got []*agentv1.AgentClientMessage, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d: %v", len(got), len(want), got)
	}
	for i, raw := range want {
		m := &agentv1.AgentClientMessage{}
		if err := protojson.Unmarshal([]byte(raw), m); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(got[i], m) {
			t.Errorf("frame %d: got %v; want %v", i, got[i], m)
		}
	}
}

func TestCursorWireOutOfOrderExecs(t *testing.T) {
	calls, frames := runCursorWireExchange(t, []string{
		`{"execServerMessage":{"id":3,"fetchArgs":{"toolCallId":"fetch-card"}}}`,
		`{"execServerMessage":{"id":5,"grepArgs":{"glob":"*.go","toolCallId":"glob-card"}}}`,
		`{"execServerMessage":{"id":8,"execId":"wire-write","writeArgs":{"toolCallId":"write-card"}}}`,
	}, []string{
		`{"id":8,"execId":"wire-write","writeResult":{}}`,
		`{"id":5,"grepResult":{}}`,
		`{"id":3,"fetchResult":{}}`,
	}, false)
	if len(calls) != 3 {
		t.Fatalf("got %d calls", len(calls))
	}
	for i, want := range []string{"fetch-card", "glob-card", "write-card"} {
		if calls[i].ToolInvocation.CallID != want || calls[i].ToolCallID != want {
			t.Errorf("call %d: got %q, want %q", i, calls[i].ToolInvocation.CallID, want)
		}
	}
	assertCursorWireFrames(t, frames,
		`{"execClientMessage":{"id":8,"execId":"wire-write","writeResult":{}}}`, `{"execClientControlMessage":{"streamClose":{"id":8}}}`,
		`{"execClientMessage":{"id":5,"grepResult":{}}}`, `{"execClientControlMessage":{"streamClose":{"id":5}}}`,
		`{"execClientMessage":{"id":3,"fetchResult":{}}}`, `{"execClientControlMessage":{"streamClose":{"id":3}}}`)
}

func TestCursorWireBurstAndChunks(t *testing.T) {
	var requests, results, want []string
	for id := 1; id <= 10; id++ {
		requests = append(requests, fmt.Sprintf(`{"execServerMessage":{"id":%d,"shellStreamArgs":{}}}`, id))
	}
	for id := 10; id >= 1; id-- {
		for _, chunk := range []string{`"stdout":{"data":"chunk"}`, `"exit":{}`} {
			r := fmt.Sprintf(`{"id":%d,"shellStream":{%s}}`, id, chunk)
			results = append(results, r)
			want = append(want, `{"execClientMessage":`+r+`}`)
		}
		want = append(want, fmt.Sprintf(`{"execClientControlMessage":{"streamClose":{"id":%d}}}`, id))
	}
	_, frames := runCursorWireExchange(t, requests, results, false)
	assertCursorWireFrames(t, frames, want...)
}

func TestCursorWireLegacyMissingIdentity(t *testing.T) {
	t.Run("single stream across chunks", func(t *testing.T) {
		_, frames := runCursorWireExchange(t, []string{`{"execServerMessage":{"id":19,"execId":"wire","shellStreamArgs":{}}}`}, []string{
			`{"shellStream":{"stdout":{"data":"one"}}}`, `{"shellStream":{"exit":{}}}`,
		}, false)
		assertCursorWireFrames(t, frames, `{"execClientMessage":{"id":19,"execId":"wire","shellStream":{"stdout":{"data":"one"}}}}`, `{"execClientMessage":{"id":19,"execId":"wire","shellStream":{"exit":{}}}}`, `{"execClientControlMessage":{"streamClose":{"id":19}}}`)
	})
	t.Run("ambiguous", func(t *testing.T) {
		_, frames := runCursorWireExchange(t, []string{`{"execServerMessage":{"id":1,"fetchArgs":{}}}`, `{"execServerMessage":{"id":2,"fetchArgs":{}}}`}, []string{`{"fetchResult":{}}`}, false)
		assertCursorWireFrames(t, frames)
	})
	t.Run("partial identities cannot steal a close", func(t *testing.T) {
		_, frames := runCursorWireExchange(t, []string{`{"execServerMessage":{"id":7,"execId":"one","writeArgs":{}}}`, `{"execServerMessage":{"id":7,"execId":"two","writeArgs":{}}}`, `{"execServerMessage":{"id":9,"execId":"other","fetchArgs":{}}}`}, []string{
			`{"id":7,"writeResult":{}}`, `{"id":9,"execId":"one","writeResult":{}}`,
			`{"execId":"two","writeResult":{}}`, `{"id":7,"execId":"two","writeResult":{}}`,
			`{"id":9,"fetchResult":{}}`, `{"id":7,"execId":"one","writeResult":{}}`,
		}, false)
		assertCursorWireFrames(t, frames, `{"execClientMessage":{"id":7,"execId":"two","writeResult":{}}}`, `{"execClientMessage":{"id":9,"execId":"other","fetchResult":{}}}`, `{"execClientControlMessage":{"streamClose":{"id":9}}}`, `{"execClientMessage":{"id":7,"execId":"one","writeResult":{}}}`, `{"execClientControlMessage":{"streamClose":{"id":7}}}`)
	})
	t.Run("already completed slot cannot steal next reply", func(t *testing.T) {
		_, frames := runCursorWireExchange(t, []string{`{"execServerMessage":{"id":1,"execId":"a","fetchArgs":{}}}`, `{"execServerMessage":{"id":2,"execId":"b","writeArgs":{}}}`}, []string{`{"id":1,"execId":"a","fetchResult":{}}`, `{"writeResult":{}}`}, false)
		assertCursorWireFrames(t, frames, `{"execClientMessage":{"id":1,"execId":"a","fetchResult":{}}}`, `{"execClientControlMessage":{"streamClose":{"id":1}}}`, `{"execClientMessage":{"id":2,"execId":"b","writeResult":{}}}`, `{"execClientControlMessage":{"streamClose":{"id":2}}}`)
	})
}

func TestCursorWireDedupAndDispatch(t *testing.T) {
	requests := []string{
		`{"interactionQuery":{"id":41,"webFetchRequestQuery":{"args":{"toolCallId":"same","url":"https://fixture.invalid/"}}}}`,
		`{"execServerMessage":{"id":41,"execId":"cloud","fetchArgs":{"toolCallId":"same"}}}`,
		`{"interactionQuery":{"id":42,"webSearchRequestQuery":{"args":{"toolCallId":"same","searchTerm":"same text"}}}}`,
		`{"interactionQuery":{"id":43,"webSearchRequestQuery":{"args":{"toolCallId":"same","searchTerm":"same text"}}}}`,
		`{"execServerMessage":{"id":44,"execId":"cloud","fetchArgs":{"toolCallId":"same"}}}`,
		`{"interactionQuery":{"id":45,"webFetchRequestQuery":{"args":{"toolCallId":"same","url":"https://fixture.invalid/"}}}}`,
	}
	calls, frames := runCursorWireExchange(t, append(append([]string{}, requests...), requests...), []string{`{"id":44,"execId":"cloud","fetchResult":{}}`, `{"id":41,"execId":"cloud","fetchResult":{}}`}, true)
	if len(calls) != len(requests) {
		t.Fatalf("got %d invocations for %d distinct wire requests sent twice", len(calls), len(requests))
	}
	used := map[string]bool{}
	for i, e := range calls {
		call := e.ToolInvocation
		if call.CallID == "" || used[call.CallID] || e.ToolCallID != call.CallID {
			t.Fatalf("call %d has missing, reused or inconsistent dispatch ID %q", i, call.CallID)
		}
		used[call.CallID] = true
		want := &agentv1.AgentServerMessage{}
		if err := protojson.Unmarshal([]byte(requests[i]), want); err != nil {
			t.Fatal(err)
		}
		var got, original proto.Message
		var raw []byte
		if q := want.GetInteractionQuery(); q != nil {
			got, original, raw = &agentv1.InteractionQuery{}, q, call.NativeInteraction
		} else {
			got, original, raw = &agentv1.ExecServerMessage{}, want.GetExecServerMessage(), call.NativeExec
		}
		if err := proto.Unmarshal(raw, got); err != nil || !proto.Equal(got, original) {
			t.Fatalf("call %d changed native envelope: got %v, want %v (err=%v)", i, got, original, err)
		}
	}
	assertCursorWireFrames(t, frames,
		`{"interactionResponse":{"id":45,"webFetchRequestResponse":{"approved":{}}}}`, `{"interactionResponse":{"id":43,"webSearchRequestResponse":{"approved":{}}}}`,
		`{"interactionResponse":{"id":42,"webSearchRequestResponse":{"approved":{}}}}`, `{"interactionResponse":{"id":41,"webFetchRequestResponse":{"approved":{}}}}`,
		`{"execClientMessage":{"id":44,"execId":"cloud","fetchResult":{}}}`, `{"execClientControlMessage":{"streamClose":{"id":44}}}`,
		`{"execClientMessage":{"id":41,"execId":"cloud","fetchResult":{}}}`, `{"execClientControlMessage":{"streamClose":{"id":41}}}`)
}

func TestCursorWireZeroIDExecCorrelatesEvenWithConcurrentExec(t *testing.T) {
	_, frames := runCursorWireExchange(t, []string{
		`{"execServerMessage":{"writeArgs":{"path":"C:/tmp/attachment.png"}}}`,
		`{"execServerMessage":{"id":1,"execId":"other","fetchArgs":{"toolCallId":"fetch-1"}}}`,
	}, []string{
		`{"writeResult":{}}`,
		`{"id":1,"execId":"other","fetchResult":{}}`,
	}, false)
	assertCursorWireFrames(t, frames,
		`{"execClientMessage":{"writeResult":{}}}`,
		`{"execClientControlMessage":{"streamClose":{}}}`,
		`{"execClientMessage":{"id":1,"execId":"other","fetchResult":{}}}`,
		`{"execClientControlMessage":{"streamClose":{"id":1}}}`)
}

func TestCursorWireZeroIDShellStreamSendsStreamClose(t *testing.T) {
	_, frames := runCursorWireExchange(t, []string{
		`{"execServerMessage":{"execId":"shell-0","shellStreamArgs":{"command":"echo ok","toolCallId":"shell-call-0"}}}`,
	}, []string{
		`{"execId":"shell-0","shellStream":{"stdout":{"data":"ok\n"}}}`,
		`{"execId":"shell-0","shellStream":{"exit":{"code":0}}}`,
	}, false)
	assertCursorWireFrames(t, frames,
		`{"execClientMessage":{"execId":"shell-0","shellStream":{"stdout":{"data":"ok\n"}}}}`,
		`{"execClientMessage":{"execId":"shell-0","shellStream":{"exit":{}}}}`,
		`{"execClientControlMessage":{"streamClose":{}}}`)
}
