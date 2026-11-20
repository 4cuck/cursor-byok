package modeladapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

func TestOpenAIChatRelayTermination(t *testing.T) {
	const tool = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{}"}}]}}]}` + "\n\n"
	const text = `data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"
	const finish = `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"
	const stop = `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	const done = "data: [DONE]\n\n"
	const usage = `data: {"choices":[],"usage":{"completion_tokens":7}}` + "\n\n"
	readErr := errors.New("relay read failed")
	for _, tt := range []struct {
		name, stream     string
		tailErr, wantErr error
		wantTools        int
		wantFinish       string
		wantTokens       int64
	}{
		{name: "read error", stream: tool, tailErr: readErr, wantErr: readErr},
		{name: "read timeout", stream: tool, tailErr: os.ErrDeadlineExceeded, wantErr: os.ErrDeadlineExceeded},
		{name: "blank finish before timeout", stream: tool + strings.Replace(stop, `"stop"`, `" "`, 1), tailErr: os.ErrDeadlineExceeded, wantErr: os.ErrDeadlineExceeded},
		{name: "unconfirmed tool EOF", stream: tool, wantErr: io.ErrUnexpectedEOF},
		{name: "usage alone does not confirm tool", stream: tool + usage, wantErr: io.ErrUnexpectedEOF},
		{name: "earlier finish cannot confirm later tool", stream: stop + tool, wantErr: io.ErrUnexpectedEOF},
		{name: "finish before read failure", stream: tool + finish, tailErr: readErr, wantErr: readErr, wantTools: 1},
		{name: "finish then usage", stream: tool + finish + usage + done, wantTools: 1, wantFinish: "tool_calls", wantTokens: 7},
		{name: "relay stop promotes tool", stream: tool + stop, wantTools: 1, wantFinish: "tool_calls"},
		{name: "done alone confirms tool", stream: tool + done, wantTools: 1},
		{name: "text only EOF", stream: text, wantFinish: "stop"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var body io.Reader = strings.NewReader(tt.stream)
			if tt.tailErr != nil {
				body = io.MultiReader(body, iotest.ErrReader(tt.tailErr))
			}
			adapter := &OpenAIAdapter{client: relayStreamTestClient(body)}
			tools, finishes := 0, 0
			err := adapter.streamChatCompletions(context.Background(), StreamRequest{
				OpenAIEndpoint: "/v1/chat/completions", ModelCallID: "call-test",
				Tools: []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{}}}}`)},
			}, "https://relay.invalid", "test", "gpt-test", 0, false, func(event ModelEvent) error {
				switch event.Kind {
				case ModelEventKindToolLikeCompleted:
					tools++
					if call := event.ToolInvocation; call == nil || call.ToolName != "Read" || string(call.ArgsJSON) != "{}" {
						t.Errorf("unexpected tool: %+v", call)
					}
				case ModelEventKindTurnFinished:
					finishes++
					if event.FinishReason != tt.wantFinish || event.OutputTokens != tt.wantTokens {
						t.Errorf("finish = %q, tokens = %d", event.FinishReason, event.OutputTokens)
					}
				}
				return nil
			})
			wantFinishes := 0
			if tt.wantFinish != "" {
				wantFinishes = 1
			}
			if !errors.Is(err, tt.wantErr) || tools != tt.wantTools || finishes != wantFinishes {
				t.Fatalf("err=%v, tools=%d, finishes=%d; want %v, %d, %d", err, tools, finishes, tt.wantErr, tt.wantTools, wantFinishes)
			}
		})
	}
}

func TestCompletedOpenAIToolArgsJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		wantSub string // 期望输出中包含的片段（为空则不检查）
	}{
		{
			name:  "正常对象",
			input: `{"paths": ["e:\\MyProject\\a.go", "b.go"]}`,
		},
		{
			name:  "空参数",
			input: ``,
		},
		{
			name:    "非对象",
			input:   `null`,
			wantErr: true,
		},
		{
			name:    "残缺JSON",
			input:   `{"paths": ["e:\MyProject`,
			wantErr: true,
		},
		{
			// 真实故障样本：上游模型重复拼接多份对象草稿，前三份 Windows 路径反斜杠漏转义
			name: "重复对象草稿+漏转义",
			input: `{"paths": ["e:\MyProject\cursor-byok\frontend\src\components\DelegationTaskStrip.vue"]}` +
				`{"paths": ["e:\MyProject\cursor-byok\frontend\src\components\DelegationTaskStrip.vue"]}` +
				`{"paths": ["e:\MyProject\cursor-byok\frontend\src\components\DelegationTaskStrip.vue"]}` +
				`{"paths": ["e:\\MyProject\\cursor-byok\\frontend\\src\\components\\DelegationTaskStrip.vue"]}`,
			wantSub: `e:\\MyProject\\cursor-byok\\frontend\\src\\components\\DelegationTaskStrip.vue`,
		},
		{
			// 单个漏转义路径：修复反斜杠后应可解析
			name:    "单个漏转义路径",
			input:   `{"pattern": "e:\MyProject\x"}`,
			wantSub: `e:\\MyProject\\x`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := &openAIToolAccumulator{Name: "ReadLints"}
			_, _ = acc.Args.WriteString(tt.input)
			out, err := completedOpenAIToolArgsJSON(acc)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望解析失败，实际成功: %s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if tt.wantSub != "" && !strings.Contains(string(out), tt.wantSub) {
				t.Fatalf("输出缺少期望片段 %q，实际: %s", tt.wantSub, out)
			}
		})
	}
}

func TestRepairOpenAIToolArgsEscapes(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`e:\MyProject\x`, `e:\\MyProject\\x`}, // 非法转义补斜杠
		{`e:\\MyProject`, `e:\\MyProject`},     // 合法转义不变
		{`line\nbreak`, `line\nbreak`},         // 合法 \n 不变
		{`\u4e2d\u6587`, `\u4e2d\u6587`},       // unicode 转义不变
		{`a\\b\M`, `a\\b\\M`},                  // 合法对 + 非法单
	}
	for _, c := range cases {
		if got := repairOpenAIToolArgsEscapes(c.in); got != c.want {
			t.Fatalf("repair(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
