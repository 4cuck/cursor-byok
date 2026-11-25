package modeladapter

import (
	"context"
	"errors"
	"fmt"
)

// ErrMidStreamInterrupted 标记流已向客户端转发部分内容后中断。
// 此类错误绝不能在 router 层整体重试：重发必然导致内容重复输出，
// 严重时同一工具调用会被二次执行（shell/写文件等副作用重复）。
var ErrMidStreamInterrupted = errors.New("mid-stream interruption after partial content has been forwarded")

// ErrIncompleteToolCallArgs 标记上游 SSE 在工具调用 JSON 参数传输完成前断流。
// 工具从未被 dispatch（JSON 不完整，无法解析），因此重放请求是安全的——
// 即使部分 delta 事件已转发给客户端，也不存在副作用重复风险。
var ErrIncompleteToolCallArgs = errors.New("incomplete tool call arguments")

// midStreamInterruptedError 包装「已产出后中断」错误并附加 ErrMidStreamInterrupted 标记，
// 供 router 层用 errors.Is 识别并跳过整体重试。
func midStreamInterruptedError(err error) error {
	if err == nil {
		return nil
	}
	var refusal *cursorAccountRefusalError
	if errors.As(err, &refusal) {
		return &refusedMidStreamError{cause: err}
	}
	return fmt.Errorf("upstream stream interrupted mid-response (already forwarded partial content, will not reconnect to avoid duplicates): %w: %w", ErrMidStreamInterrupted, err)
}

// refusedMidStreamError is a refusal from Cursor's server that arrived after
// part of the response had been forwarded. It carries ErrMidStreamInterrupted,
// so the call is not sent again, but it reads as the refusal alone. Nothing
// was interrupted: the server declined the call, and its text is what tells
// the user what to do (pay an invoice, raise a limit, accept a policy). Behind
// the wording for a lost connection that text is easy to miss, and retrying
// only gets the same refusal.
type refusedMidStreamError struct {
	cause error
}

func (e *refusedMidStreamError) Error() string {
	return e.cause.Error()
}

func (e *refusedMidStreamError) Unwrap() []error {
	return []error{ErrMidStreamInterrupted, e.cause}
}

// streamWithReconnect 提供通用的 pre-output 流式透明重连：
// 已向 sink 转发任何事件后绝不重连（避免重复输出）；仅连接重置类错误可重连，
// 最多 maxStreamReconnects 次，退避复用 providerRetryBaseDelay 指数递增。
// 已产出后的任何失败（不限连接重置）都会附加 ErrMidStreamInterrupted 标记，
// 供 router 层识别并跳过整体重试/failover。
// OpenAI 适配器保留其专属的 prompt_cache_key 适配版本（openai.go），本 helper
// 供 Anthropic/Gemini 使用。
func streamWithReconnect(ctx context.Context, sink func(ModelEvent) error, safety ReplaySafety, stream func(int, func(ModelEvent) error) error) error {
	var connectionAttempt int
	for {
		emitted := false
		wrappedSink := func(event ModelEvent) error {
			emitted = true
			return sink(event)
		}
		err := stream(0, wrappedSink)
		if err == nil {
			return nil
		}
		if emitted {
			// 已向客户端产出可见事件后发生任何错误：本轮输出不可重放，
			// 一律标记 mid-stream，禁止 router 层整体重试造成重复输出。
			// 例外：工具调用参数截断（ErrIncompleteToolCallArgs）时，工具从未被
			// dispatch，重放不会产生副作用重复，直接返回原始错误供 router 重试。
			if ctx.Err() != nil {
				return err
			}
			if errors.Is(err, ErrIncompleteToolCallArgs) {
				return err
			}
			return midStreamInterruptedError(err)
		}
		if !IsStreamConnectionReset(err) {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
		if !safety.Safe {
			return replayUnsafeDropError(safety, err)
		}
		connectionAttempt++
		if connectionAttempt > maxStreamReconnects {
			return fmt.Errorf("stream reconnect exhausted after %d attempts: %w", maxStreamReconnects, err)
		}
		backoff := providerRetryBaseDelay << (connectionAttempt - 1)
		if backoff > providerRetryMaxDelay {
			backoff = providerRetryMaxDelay
		}
		if sleepErr := sleepWithContext(ctx, backoff); sleepErr != nil {
			return sleepErr
		}
	}
}
