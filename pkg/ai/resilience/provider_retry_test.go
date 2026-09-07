package resilience

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	llmtestutil "github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
	"github.com/ashjazz/Longtermism/pkg/ai/obs/testutil"
)

func TestProviderWrapperExecutesRetryPolicyOncePerRequest(t *testing.T) {
	provider := &retryScriptedProvider{
		chatErrors: []error{
			fmt.Errorf("temporary one: %w", llm.ErrUpstream),
			fmt.Errorf("temporary two: %w", llm.ErrUpstream),
		},
		chatResponse: &llm.ChatResponse{Content: "recovered", Model: "model", Usage: llmtestutil.MustReportedUsage(llm.Usage{})},
	}
	var delays []time.Duration
	wrapper := NewProviderWrapper(
		provider,
		NewCircuitBreaker(Config{FailureThreshold: 1}),
		WithExecutionPolicy(ProviderExecutionPolicy{Timeout: time.Minute, RetryMax: 2, RetryBackoff: time.Second}),
		withProviderRuntime(context.WithTimeout, func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		}),
	)

	response, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "model"})
	if err != nil || response.Content != "recovered" {
		t.Fatalf("Chat() response=%#v error=%v", response, err)
	}
	if provider.chatCalls != 3 || !reflect.DeepEqual(delays, []time.Duration{time.Second, 3 * time.Second}) {
		t.Fatalf("calls=%d delays=%v, want calls=3 delays=[1s 3s]", provider.chatCalls, delays)
	}
}

func TestProviderWrapperSharesOneDeadlineAndDoesNotRetryCallerErrors(t *testing.T) {
	t.Run("one deadline covers all retry attempts", func(t *testing.T) {
		provider := &retryScriptedProvider{
			chatErrors: []error{
				fmt.Errorf("temporary one: %w", llm.ErrUpstream),
				fmt.Errorf("temporary two: %w", llm.ErrUpstream),
			},
			chatResponse: &llm.ChatResponse{Content: "recovered", Model: "model", Usage: llmtestutil.MustReportedUsage(llm.Usage{})},
		}
		withTimeoutCalls := 0
		wrapper := NewProviderWrapper(
			provider,
			NewCircuitBreaker(Config{FailureThreshold: 1}),
			WithExecutionPolicy(ProviderExecutionPolicy{Timeout: time.Minute, RetryMax: 2, RetryBackoff: time.Second}),
			withProviderRuntime(func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
				withTimeoutCalls++
				return context.WithTimeout(parent, timeout)
			}, func(context.Context, time.Duration) error { return nil }),
		)

		if _, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "model"}); err != nil {
			t.Fatalf("Chat() error=%v", err)
		}
		if withTimeoutCalls != 1 || provider.chatCalls != 3 {
			t.Fatalf("deadline constructions=%d calls=%d, want one deadline and three attempts", withTimeoutCalls, provider.chatCalls)
		}
	})

	t.Run("caller error is neither retried nor counted as an upstream failure", func(t *testing.T) {
		provider := &retryScriptedProvider{chatErrors: []error{errors.New("invalid request body must not escape")}}
		sleepCalls := 0
		wrapper := NewProviderWrapper(
			provider,
			NewCircuitBreaker(Config{FailureThreshold: 1}),
			WithExecutionPolicy(ProviderExecutionPolicy{Timeout: time.Minute, RetryMax: 2, RetryBackoff: time.Second}),
			withProviderRuntime(context.WithTimeout, func(context.Context, time.Duration) error { sleepCalls++; return nil }),
		)

		_, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "model"})
		if !errors.Is(err, ErrProviderRejected) || provider.chatCalls != 1 || sleepCalls != 0 {
			t.Fatalf("error=%v calls=%d sleeps=%d, want rejected classification, 1 call, 0 sleeps", err, provider.chatCalls, sleepCalls)
		}
	})
}

func TestProviderWrapperSanitizesNonRetryableProviderDetails(t *testing.T) {
	const secret = "non-retryable-provider-body-secret"
	provider := &retryScriptedProvider{chatErrors: []error{fmt.Errorf("provider rejected request %s", secret)}}
	wrapper := NewProviderWrapper(provider, NewCircuitBreaker(Config{FailureThreshold: 1}), WithExecutionPolicy(DefaultProviderExecutionPolicy()))

	_, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "model"})
	if !errors.Is(err, ErrProviderRejected) || strings.Contains(fmt.Sprint(err), secret) {
		t.Fatalf("error=%v, want sanitized provider rejection", err)
	}
}

func TestProviderWrapperRetriesOnlyBeforeStreamProducesChunk(t *testing.T) {
	stream := make(chan llm.ChatChunk, 2)
	stream <- llm.ChatChunk{DeltaContent: "partial"}
	stream <- llm.ChatChunk{Err: fmt.Errorf("terminal: %w", llm.ErrUpstream)}
	close(stream)
	provider := &retryScriptedProvider{
		streamErrors: []error{fmt.Errorf("initial failure: %w", llm.ErrUpstream)},
		stream:       stream,
	}
	var delays []time.Duration
	wrapper := NewProviderWrapper(
		provider,
		NewCircuitBreaker(Config{FailureThreshold: 1}),
		WithExecutionPolicy(ProviderExecutionPolicy{Timeout: time.Minute, RetryMax: 2, RetryBackoff: time.Second}),
		withProviderRuntime(context.WithTimeout, func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		}),
	)

	chunks, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "model"})
	if err != nil {
		t.Fatalf("ChatStream() error=%v", err)
	}
	var terminal error
	for chunk := range chunks {
		if chunk.Err != nil {
			terminal = chunk.Err
		}
	}
	if provider.streamCalls != 2 || !reflect.DeepEqual(delays, []time.Duration{time.Second}) {
		t.Fatalf("stream calls=%d delays=%v, want calls=2 delays=[1s]", provider.streamCalls, delays)
	}
	if !errors.Is(terminal, llm.ErrUpstream) {
		t.Fatalf("terminal error=%v, want upstream classification", terminal)
	}
}

func TestProviderWrapperRetriesAnUpstreamFirstChunkBeforeForwarding(t *testing.T) {
	failedStream := make(chan llm.ChatChunk, 1)
	failedStream <- llm.ChatChunk{Err: fmt.Errorf("first SSE event failed: %w", llm.ErrUpstream)}
	close(failedStream)
	recoveredStream := make(chan llm.ChatChunk, 1)
	recoveredStream <- llm.ChatChunk{DeltaContent: "recovered"}
	close(recoveredStream)
	provider := &retryScriptedProvider{streams: []<-chan llm.ChatChunk{failedStream, recoveredStream}}
	var delays []time.Duration
	wrapper := NewProviderWrapper(
		provider,
		NewCircuitBreaker(Config{FailureThreshold: 1}),
		WithExecutionPolicy(ProviderExecutionPolicy{Timeout: time.Minute, RetryMax: 2, RetryBackoff: time.Second}),
		withProviderRuntime(context.WithTimeout, func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil }),
	)

	chunks, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "model"})
	if err != nil {
		t.Fatalf("ChatStream() error=%v", err)
	}
	chunk := <-chunks
	if chunk.DeltaContent != "recovered" || provider.streamCalls != 2 || !reflect.DeepEqual(delays, []time.Duration{time.Second}) {
		t.Fatalf("chunk=%#v calls=%d delays=%v, want recovered chunk, two calls, one delay", chunk, provider.streamCalls, delays)
	}
}

func TestProviderWrapperStreamTerminalLifecycleCancelsAndSanitizes(t *testing.T) {
	const secret = "provider-response-secret"
	t.Run("terminal chunk error keeps classification without leaking provider payload", func(t *testing.T) {
		stream := make(chan llm.ChatChunk, 2)
		stream <- llm.ChatChunk{DeltaContent: "partial"}
		stream <- llm.ChatChunk{Err: fmt.Errorf("upstream body contains %s: %w", secret, llm.ErrUpstream)}
		close(stream)
		wrapper := NewProviderWrapper(
			&retryScriptedProvider{stream: stream},
			NewCircuitBreaker(Config{FailureThreshold: 1}),
			WithExecutionPolicy(DefaultProviderExecutionPolicy()),
		)

		chunks, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "model"})
		if err != nil {
			t.Fatalf("ChatStream() error=%v", err)
		}
		// 首块（partial）跨过 wrapper 边界后 started 才触发；terminal 错误块
		// 是消费者读到的第二个 chunk，必须保留上游分类且不泄露 provider 原文。
		partial := <-chunks
		if partial.DeltaContent != "partial" || partial.Err != nil {
			t.Fatalf("first chunk=%#v, want the forwarded partial content", partial)
		}
		terminal := <-chunks
		if terminal.Err == nil || strings.Contains(terminal.Err.Error(), secret) || !errors.Is(terminal.Err, llm.ErrUpstream) {
			t.Fatalf("terminal error=%v, want sanitized upstream classification", terminal.Err)
		}
	})

	t.Run("non-retryable terminal detail is projected to a stable rejection", func(t *testing.T) {
		stream := make(chan llm.ChatChunk, 2)
		stream <- llm.ChatChunk{DeltaContent: "partial"}
		stream <- llm.ChatChunk{Err: fmt.Errorf("provider response contains %s", secret)}
		close(stream)
		wrapper := NewProviderWrapper(
			&retryScriptedProvider{stream: stream},
			NewCircuitBreaker(Config{FailureThreshold: 1}),
			WithExecutionPolicy(DefaultProviderExecutionPolicy()),
		)

		chunks, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "model"})
		if err != nil {
			t.Fatalf("ChatStream() error=%v", err)
		}
		<-chunks
		terminal := <-chunks
		if !errors.Is(terminal.Err, ErrProviderRejected) || strings.Contains(fmt.Sprint(terminal.Err), secret) {
			t.Fatalf("terminal error=%v, want sanitized provider rejection", terminal.Err)
		}
	})

	t.Run("caller cancellation releases the request budget even when consumer stops reading", func(t *testing.T) {
		// started 信号在首个 chunk 跨越 wrapper 边界后才发送（replay-safe 重试
		// 窗口的语义）；因此本用例必须预置一个首块让 ChatStream 能返回，随后
		// 调用方在不再读取流的情况下取消——预算释放必须不依赖消费者读取。
		stream := make(chan llm.ChatChunk, 1)
		stream <- llm.ChatChunk{DeltaContent: "first"}
		provider := &retryScriptedProvider{stream: stream}
		budgetReleased := make(chan struct{})
		var releaseOnce sync.Once
		wrapper := NewProviderWrapper(
			provider,
			NewCircuitBreaker(Config{FailureThreshold: 1}),
			WithExecutionPolicy(DefaultProviderExecutionPolicy()),
			withProviderRuntime(func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
				return parent, func() { releaseOnce.Do(func() { close(budgetReleased) }) }
			}, nil),
		)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		if _, err := wrapper.ChatStream(ctx, &llm.ChatRequest{Model: "model"}); err != nil {
			t.Fatalf("ChatStream() error=%v", err)
		}
		cancel()
		select {
		case <-budgetReleased:
		case <-time.After(time.Second):
			t.Fatal("cancelling an unread stream must release its request budget")
		}
	})
}

func TestProviderWrapperEmitsDeadlineTerminalChunkAfterPartialOutput(t *testing.T) {
	stream := make(chan llm.ChatChunk, 2)
	stream <- llm.ChatChunk{DeltaContent: "partial"}
	stream <- llm.ChatChunk{DeltaContent: "second partial"}
	provider := &retryScriptedProvider{stream: stream}
	wrapper := NewProviderWrapper(
		provider,
		NewCircuitBreaker(Config{FailureThreshold: 1}),
		WithExecutionPolicy(ProviderExecutionPolicy{Timeout: 10 * time.Millisecond, RetryMax: 0, RetryBackoff: time.Second}),
	)

	chunks, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "model"})
	if err != nil {
		t.Fatalf("ChatStream() error=%v", err)
	}
	if chunk := <-chunks; chunk.DeltaContent != "partial" {
		t.Fatalf("first chunk=%#v, want partial output", chunk)
	}
	if chunk := <-chunks; chunk.DeltaContent != "second partial" {
		t.Fatalf("second chunk=%#v, want second partial output", chunk)
	}
	select {
	case terminal := <-chunks:
		if !errors.Is(terminal.Err, context.DeadlineExceeded) {
			t.Fatalf("terminal chunk=%#v, want deadline exceeded", terminal)
		}
	case <-time.After(time.Second):
		t.Fatal("request deadline must become an explicit stream terminal event")
	}
}

func TestProviderWrapperRecordsOneOutcomeWhenStreamTerminates(t *testing.T) {
	stream := make(chan llm.ChatChunk, 2)
	stream <- llm.ChatChunk{DeltaContent: "partial"}
	stream <- llm.ChatChunk{Err: fmt.Errorf("terminal provider failure: %w", llm.ErrUpstream)}
	close(stream)
	recorder := testutil.NewRecorder()
	identity := obs.NewCorrelationIdentity(
		"request-stream-terminal",
		obs.WithServiceSpan("service-trace-stream-terminal", "span-stream-terminal"),
		obs.WithAITraceID("ai-trace-stream-terminal"),
	)
	breaker := NewCircuitBreaker(Config{FailureThreshold: 1})
	wrapper := NewProviderWrapper(
		&retryScriptedProvider{stream: stream},
		breaker,
		WithExecutionPolicy(DefaultProviderExecutionPolicy()),
		WithTracer(recorder),
		WithFeature("llm_generation"),
	)

	chunks, err := wrapper.ChatStream(obs.ContextWithCorrelationIdentity(context.Background(), identity), &llm.ChatRequest{Model: "model"})
	if err != nil {
		t.Fatalf("ChatStream() error=%v", err)
	}
	for range chunks {
	}
	if breaker.State() != StateOpen {
		t.Fatalf("breaker state=%q, want %q after terminal upstream failure", breaker.State(), StateOpen)
	}
	recorder.AssertCount(t, 1)
	recorder.AssertTrace(t, 0, func(t *testing.T, trace obs.Trace) {
		if trace.OutcomeStatus != "failure" || trace.FailureStatus != string(obs.FailureUpstream) {
			t.Fatalf("stream terminal outcome=%q/%q, want failure/upstream", trace.OutcomeStatus, trace.FailureStatus)
		}
	})
}

type retryScriptedProvider struct {
	chatErrors   []error
	chatResponse *llm.ChatResponse
	streamErrors []error
	stream       <-chan llm.ChatChunk
	streams      []<-chan llm.ChatChunk
	chatCalls    int
	streamCalls  int
}

func (*retryScriptedProvider) Name() string { return "retry-scripted" }

func (*retryScriptedProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{}
}

func (p *retryScriptedProvider) Chat(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
	p.chatCalls++
	if index := p.chatCalls - 1; index < len(p.chatErrors) && p.chatErrors[index] != nil {
		return nil, p.chatErrors[index]
	}
	return p.chatResponse, nil
}

func (p *retryScriptedProvider) ChatStream(context.Context, *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	p.streamCalls++
	if index := p.streamCalls - 1; index < len(p.streamErrors) && p.streamErrors[index] != nil {
		return nil, p.streamErrors[index]
	}
	if index := p.streamCalls - 1; index < len(p.streams) {
		return p.streams[index], nil
	}
	return p.stream, nil
}
