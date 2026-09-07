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
)

func TestProviderWrapperPassesThroughProviderSemantics(t *testing.T) {
	t.Parallel()

	provider := newCountingProvider()
	provider.capabilities["capable-model"] = llm.ProviderCapabilities{ToolCalling: true}
	provider.chatResponses["ok-model"] = llm.ChatResponse{
		Content:      "ok",
		Model:        "ok-model",
		Usage:        llmtestutil.MustReportedUsage(llm.Usage{}),
		FinishReason: llm.FinishStop,
	}
	wrapped := NewProviderWrapper(provider, NewCircuitBreaker(Config{FailureThreshold: 1}))

	if wrapped.Name() != provider.Name() {
		t.Fatalf("Name() = %q, want provider name", wrapped.Name())
	}
	if !wrapped.Capabilities("capable-model").ToolCalling {
		t.Fatal("Capabilities() did not pass through provider capability")
	}

	got, err := wrapped.Chat(context.Background(), &llm.ChatRequest{Model: "ok-model"})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got.Content != "ok" {
		t.Fatalf("Chat() content = %q, want ok", got.Content)
	}
	if provider.ChatCalls() != 1 {
		t.Fatalf("provider chat calls = %d, want 1", provider.ChatCalls())
	}
}

func TestProviderWrapperOpensCircuitOnErrUpstream(t *testing.T) {
	t.Parallel()

	provider := newCountingProvider()
	provider.chatErrors["upstream-model"] = fmt.Errorf("provider unavailable: %w", llm.ErrUpstream)
	provider.chatResponses["ok-model"] = llm.ChatResponse{Content: "ok", Usage: llmtestutil.MustReportedUsage(llm.Usage{}), FinishReason: llm.FinishStop}
	breaker := NewCircuitBreaker(Config{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Minute,
	})
	wrapped := NewProviderWrapper(provider, breaker)

	_, err := wrapped.Chat(context.Background(), &llm.ChatRequest{Model: "upstream-model"})
	if !errors.Is(err, llm.ErrUpstream) {
		t.Fatalf("upstream Chat() error = %v, want preserve llm.ErrUpstream", err)
	}
	if breaker.State() != StateOpen {
		t.Fatalf("breaker State() = %q, want %q", breaker.State(), StateOpen)
	}

	_, err = wrapped.Chat(context.Background(), &llm.ChatRequest{Model: "ok-model"})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open circuit Chat() error = %v, want ErrCircuitOpen", err)
	}
	if provider.ChatCalls() != 1 {
		t.Fatalf("provider chat calls = %d, want 1 because second call fast-fails", provider.ChatCalls())
	}
}

func TestProviderWrapperDoesNotOpenCircuitOnCallerError(t *testing.T) {
	t.Parallel()

	callerErr := errors.New("openai chat request failed with status 400")
	provider := newCountingProvider()
	provider.chatErrors["bad-request-model"] = callerErr
	provider.chatResponses["ok-model"] = llm.ChatResponse{Content: "ok", Usage: llmtestutil.MustReportedUsage(llm.Usage{}), FinishReason: llm.FinishStop}
	breaker := NewCircuitBreaker(Config{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Minute,
	})
	wrapped := NewProviderWrapper(provider, breaker)

	_, err := wrapped.Chat(context.Background(), &llm.ChatRequest{Model: "bad-request-model"})
	if !errors.Is(err, ErrProviderRejected) {
		t.Fatalf("caller Chat() error = %v, want stable rejection classification", err)
	}
	if breaker.State() != StateClosed {
		t.Fatalf("breaker State() = %q, want %q after caller error", breaker.State(), StateClosed)
	}

	got, err := wrapped.Chat(context.Background(), &llm.ChatRequest{Model: "ok-model"})
	if err != nil {
		t.Fatalf("next Chat() error = %v", err)
	}
	if got.Content != "ok" {
		t.Fatalf("next Chat() content = %q, want ok", got.Content)
	}
	if provider.ChatCalls() != 2 {
		t.Fatalf("provider chat calls = %d, want both calls to reach provider", provider.ChatCalls())
	}
}

type countingProvider struct {
	mu            sync.Mutex
	chatCalls     int
	capabilities  map[string]llm.ProviderCapabilities
	chatResponses map[string]llm.ChatResponse
	chatErrors    map[string]error
}

func newCountingProvider() *countingProvider {
	return &countingProvider{
		capabilities:  make(map[string]llm.ProviderCapabilities),
		chatResponses: make(map[string]llm.ChatResponse),
		chatErrors:    make(map[string]error),
	}
}

func (p *countingProvider) Name() string {
	return "counting-provider"
}

func (p *countingProvider) Capabilities(model string) llm.ProviderCapabilities {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capabilities[model]
}

func (p *countingProvider) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.chatCalls++
	if err, ok := p.chatErrors[req.Model]; ok {
		return nil, err
	}
	response, ok := p.chatResponses[req.Model]
	if !ok {
		return nil, fmt.Errorf("missing response for model %q", req.Model)
	}
	return &response, nil
}

func (p *countingProvider) ChatStream(ctx context.Context, req *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	return nil, fmt.Errorf("counting provider stream is not used by provider wrapper tests")
}

func (p *countingProvider) ChatCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chatCalls
}

func TestProviderWrapperRecordsOneFactPerNetworkAttempt(t *testing.T) {
	t.Run("first attempt succeeds", func(t *testing.T) {
		usage := llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5})
		provider := &providerAttemptScriptedProvider{
			steps: []providerAttemptStep{{
				response: &llm.ChatResponse{Content: "ok", Model: "actual-model", Usage: usage, FinishReason: llm.FinishStop},
			}},
		}
		observer := &providerAttemptRecorder{}
		sleepCalls := 0
		wrapper := newProviderAttemptTestWrapper(provider, observer, 2, func(context.Context, time.Duration) error {
			sleepCalls++
			return nil
		})

		response, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "requested-model"})
		if err != nil || response == nil || response.Content != "ok" {
			t.Fatalf("Chat() response=%#v error=%v, want the provider success unchanged", response, err)
		}
		facts := observer.Facts()
		if provider.Calls() != 1 || sleepCalls != 0 || len(facts) != 1 {
			t.Fatalf("adapter calls=%d sleeps=%d facts=%d, want 1/0/1", provider.Calls(), sleepCalls, len(facts))
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider:       "attempt-scripted",
			requestedModel: "requested-model",
			actualModel:    "actual-model",
			outcome:        "succeeded",
			usage:          llm.UsageReported,
		})
	})

	t.Run("retry and backoff create facts only for adapter calls", func(t *testing.T) {
		provider := &providerAttemptScriptedProvider{
			steps: []providerAttemptStep{
				{err: fmt.Errorf("temporary upstream failure: %w", llm.ErrUpstream)},
				{response: &llm.ChatResponse{
					Content: "recovered", Model: "fallback-model",
					Usage: llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5}),
				}},
			},
		}
		observer := &providerAttemptRecorder{}
		var delays []time.Duration
		wrapper := newProviderAttemptTestWrapper(provider, observer, 2, func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		})

		response, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "requested-model"})
		if err != nil || response == nil || response.Content != "recovered" {
			t.Fatalf("Chat() response=%#v error=%v, want retry recovery unchanged", response, err)
		}
		facts := observer.Facts()
		if provider.Calls() != 2 || len(facts) != 2 || !reflect.DeepEqual(delays, []time.Duration{time.Second}) {
			t.Fatalf("adapter calls=%d facts=%d delays=%v, want 2/2/[1s]", provider.Calls(), len(facts), delays)
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "failed", usage: llm.UsageUnavailable,
		})
		assertProviderAttemptFact(t, facts[1], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", actualModel: "fallback-model", outcome: "succeeded", usage: llm.UsageReported,
		})
	})

	t.Run("cancelled backoff does not invent another attempt", func(t *testing.T) {
		provider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{{err: fmt.Errorf("temporary: %w", llm.ErrUpstream)}}}
		observer := &providerAttemptRecorder{}
		sleepCalls := 0
		wrapper := newProviderAttemptTestWrapper(provider, observer, 2, func(context.Context, time.Duration) error {
			sleepCalls++
			return context.Canceled
		})

		_, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "requested-model"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Chat() error=%v, want backoff cancellation", err)
		}
		facts := observer.Facts()
		if provider.Calls() != 1 || sleepCalls != 1 || len(facts) != 1 {
			t.Fatalf("adapter calls=%d sleeps=%d facts=%d, want 1/1/1", provider.Calls(), sleepCalls, len(facts))
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "failed", usage: llm.UsageUnavailable,
		})
	})
}

func TestProviderWrapperRecordsEveryTerminalProviderFailure(t *testing.T) {
	const rawBodyCanary = "raw-provider-error-body-must-not-cross-attempt-port"
	tests := []struct {
		name       string
		attemptErr error
		wantErrIs  error
	}{
		{
			name:       "final 429",
			attemptErr: errors.Join(fmt.Errorf("429 body %s", rawBodyCanary), llm.ErrRateLimit, llm.ErrUpstream),
			wantErrIs:  llm.ErrRateLimit,
		},
		{
			name:       "final 5xx",
			attemptErr: fmt.Errorf("503 body %s: %w", rawBodyCanary, llm.ErrUpstream),
			wantErrIs:  llm.ErrUpstream,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{
				{err: tt.attemptErr}, {err: tt.attemptErr}, {err: tt.attemptErr},
			}}
			observer := &providerAttemptRecorder{}
			var delays []time.Duration
			wrapper := newProviderAttemptTestWrapper(provider, observer, 2, func(_ context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				return nil
			})

			_, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "requested-model"})
			if !errors.Is(err, tt.wantErrIs) || strings.Contains(fmt.Sprint(err), rawBodyCanary) {
				t.Fatalf("Chat() error=%v, want stable %v classification without raw body", err, tt.wantErrIs)
			}
			facts := observer.Facts()
			if provider.Calls() != 3 || len(facts) != 3 || !reflect.DeepEqual(delays, []time.Duration{time.Second, 3 * time.Second}) {
				t.Fatalf("adapter calls=%d facts=%d delays=%v, want 3/3/[1s 3s]", provider.Calls(), len(facts), delays)
			}
			for index, fact := range facts {
				assertProviderAttemptFact(t, fact, providerAttemptFactExpectation{
					provider: "attempt-scripted", requestedModel: "requested-model", outcome: "failed", usage: llm.UsageUnavailable,
				})
				if strings.Contains(fmt.Sprintf("%#v", fact), rawBodyCanary) {
					t.Fatalf("attempt fact %d leaked the provider error body", index)
				}
			}

			// Circuit-open is a local fast-fail, not a provider network attempt.
			_, err = wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "requested-model"})
			if !errors.Is(err, ErrCircuitOpen) || provider.Calls() != 3 || len(observer.Facts()) != 3 {
				t.Fatalf("fast-fail error=%v calls=%d facts=%d, want circuit-open with no new attempt", err, provider.Calls(), len(observer.Facts()))
			}
		})
	}
}

func TestProviderWrapperRecordsStreamNetworkAttempts(t *testing.T) {
	t.Run("first stream adapter attempt succeeds", func(t *testing.T) {
		streamUsage := &llm.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}
		provider := &providerAttemptScriptedProvider{streamSteps: []providerAttemptStreamStep{{
			stream: providerAttemptStream(llm.ChatChunk{
				DeltaContent: "streamed", FinishReason: llm.FinishStop, Usage: streamUsage,
			}),
		}}}
		observer := &providerAttemptRecorder{}
		sleepCalls := 0
		wrapper := newProviderAttemptTestWrapper(provider, observer, 2, func(context.Context, time.Duration) error {
			sleepCalls++
			return nil
		})

		chunks, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "requested-model"})
		if err != nil {
			t.Fatalf("ChatStream() error=%v", err)
		}
		var content string
		for chunk := range chunks {
			content += chunk.DeltaContent
		}
		facts := observer.Facts()
		if content != "streamed" || provider.StreamCalls() != 1 || sleepCalls != 0 || len(facts) != 1 {
			t.Fatalf("content=%q adapter calls=%d sleeps=%d facts=%d, want streamed/1/0/1", content, provider.StreamCalls(), sleepCalls, len(facts))
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "succeeded", usage: llm.UsageReported,
		})
		streamUsage.InputTokens = 999
		retained, ok := facts[0].Usage().Summary()
		if !ok || retained.InputTokens != 7 || retained.OutputTokens != 3 || retained.TotalTokens != 10 {
			t.Fatalf("immutable stream usage=%#v ok=%v, want original 7/3/10 snapshot", retained, ok)
		}
	})

	t.Run("stream first-event retry records the terminal outcome for each adapter attempt", func(t *testing.T) {
		provider := &providerAttemptScriptedProvider{streamSteps: []providerAttemptStreamStep{
			{stream: providerAttemptStream(llm.ChatChunk{Err: fmt.Errorf("first SSE event failed: %w", llm.ErrUpstream)})},
			{stream: providerAttemptStream(llm.ChatChunk{DeltaContent: "recovered"})},
		}}
		observer := &providerAttemptRecorder{}
		var delays []time.Duration
		wrapper := newProviderAttemptTestWrapper(provider, observer, 2, func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		})

		chunks, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "requested-model"})
		if err != nil {
			t.Fatalf("ChatStream() error=%v", err)
		}
		for range chunks {
		}
		facts := observer.Facts()
		if provider.StreamCalls() != 2 || len(facts) != 2 || !reflect.DeepEqual(delays, []time.Duration{time.Second}) {
			t.Fatalf("adapter calls=%d facts=%d delays=%v, want 2/2/[1s]", provider.StreamCalls(), len(facts), delays)
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "failed", usage: llm.UsageUnavailable,
		})
		assertProviderAttemptFact(t, facts[1], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "succeeded", usage: llm.UsageUnavailable,
		})
	})

	t.Run("stream failure after partial output records one failed attempt", func(t *testing.T) {
		const rawErrorCanary = "stream-terminal-provider-body-canary"
		provider := &providerAttemptScriptedProvider{streamSteps: []providerAttemptStreamStep{{
			stream: providerAttemptStream(
				llm.ChatChunk{DeltaContent: "partial"},
				llm.ChatChunk{Err: fmt.Errorf("terminal %s: %w", rawErrorCanary, llm.ErrUpstream)},
			),
		}}}
		observer := &providerAttemptRecorder{}
		wrapper := newProviderAttemptTestWrapper(provider, observer, 2, func(context.Context, time.Duration) error {
			t.Fatal("a terminal failure after partial output must not be retried")
			return nil
		})

		chunks, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "requested-model"})
		if err != nil {
			t.Fatalf("ChatStream() error=%v", err)
		}
		var terminal error
		for chunk := range chunks {
			if chunk.Err != nil {
				terminal = chunk.Err
			}
		}
		facts := observer.Facts()
		if !errors.Is(terminal, llm.ErrUpstream) || strings.Contains(fmt.Sprint(terminal), rawErrorCanary) ||
			provider.StreamCalls() != 1 || len(facts) != 1 {
			t.Fatalf("terminal=%v adapter calls=%d facts=%d, want sanitized upstream/1/1", terminal, provider.StreamCalls(), len(facts))
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "failed", usage: llm.UsageUnavailable,
		})
		if strings.Contains(fmt.Sprintf("%#v", facts[0]), rawErrorCanary) {
			t.Fatal("stream terminal provider body leaked into attempt fact")
		}
	})
}

func TestProviderWrapperClassifiesAttemptTermination(t *testing.T) {
	t.Run("nil stream is an invalid response fact", func(t *testing.T) {
		provider := &providerAttemptScriptedProvider{streamSteps: []providerAttemptStreamStep{{}}}
		observer := &providerAttemptRecorder{}
		wrapper := newProviderAttemptTestWrapper(provider, observer, 0, func(context.Context, time.Duration) error { return nil })

		stream, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "requested-model"})
		if !errors.Is(err, llm.ErrUpstream) || stream != nil {
			t.Fatalf("ChatStream() stream=%v error=%v, want existing upstream business classification", stream, err)
		}
		facts := observer.Facts()
		if provider.StreamCalls() != 1 || len(facts) != 1 {
			t.Fatalf("adapter calls=%d facts=%d, want 1/1", provider.StreamCalls(), len(facts))
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "invalid_response", usage: llm.UsageUnavailable,
		})
	})

	t.Run("provider timeout", func(t *testing.T) {
		deadlineCtx := newProviderAttemptDeadlineContext(context.Background())
		provider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{{
			call: func(callCtx context.Context) (*llm.ChatResponse, error) {
				deadlineCtx.Expire()
				<-callCtx.Done()
				return nil, errors.Join(callCtx.Err(), llm.ErrUpstream)
			},
		}}}
		observer := &providerAttemptRecorder{}
		wrapper := NewProviderWrapper(
			provider,
			NewCircuitBreaker(Config{FailureThreshold: 1, RecoveryTimeout: time.Minute}),
			WithExecutionPolicy(ProviderExecutionPolicy{Timeout: time.Minute, RetryMax: 0, RetryBackoff: time.Second}),
			WithProviderAttemptObserver(observer),
			WithNow(steppingProviderAttemptClock(time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC), 5*time.Millisecond)),
			withProviderRuntime(func(context.Context, time.Duration) (context.Context, context.CancelFunc) {
				return deadlineCtx, func() {}
			}, func(context.Context, time.Duration) error {
				t.Fatal("timeout with retry disabled must not back off")
				return nil
			}),
		)

		_, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "requested-model"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Chat() error=%v, want deadline exceeded", err)
		}
		facts := observer.Facts()
		if provider.Calls() != 1 || len(facts) != 1 {
			t.Fatalf("adapter calls=%d facts=%d, want 1/1", provider.Calls(), len(facts))
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "timeout", usage: llm.UsageUnavailable,
		})
	})

	t.Run("caller cancellation after entering adapter", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		provider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{{
			call: func(callCtx context.Context) (*llm.ChatResponse, error) {
				cancel()
				return nil, callCtx.Err()
			},
		}}}
		observer := &providerAttemptRecorder{}
		wrapper := newProviderAttemptTestWrapper(provider, observer, 0, func(context.Context, time.Duration) error { return nil })

		_, err := wrapper.Chat(ctx, &llm.ChatRequest{Model: "requested-model"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Chat() error=%v, want caller cancellation", err)
		}
		facts := observer.Facts()
		if provider.Calls() != 1 || len(facts) != 1 {
			t.Fatalf("adapter calls=%d facts=%d, want 1/1", provider.Calls(), len(facts))
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "cancelled", usage: llm.UsageUnavailable,
		})
	})

	t.Run("caller cancellation wins over a generic adapter error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		provider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{{
			call: func(context.Context) (*llm.ChatResponse, error) {
				cancel()
				return nil, fmt.Errorf("adapter omitted context cause: %w", llm.ErrUpstream)
			},
		}}}
		observer := &providerAttemptRecorder{}
		wrapper := newProviderAttemptTestWrapper(provider, observer, 0, func(context.Context, time.Duration) error { return nil })

		_, err := wrapper.Chat(ctx, &llm.ChatRequest{Model: "requested-model"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Chat() error=%v, want caller cancellation", err)
		}
		facts := observer.Facts()
		if provider.Calls() != 1 || len(facts) != 1 {
			t.Fatalf("adapter calls=%d facts=%d, want 1/1", provider.Calls(), len(facts))
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "cancelled", usage: llm.UsageUnavailable,
		})
	})

	t.Run("successful adapter result wins over simultaneous caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		usage := llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3})
		provider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{{
			call: func(context.Context) (*llm.ChatResponse, error) {
				cancel()
				return &llm.ChatResponse{Content: "completed", Model: "actual-model", Usage: usage}, nil
			},
		}}}
		observer := &providerAttemptRecorder{}
		wrapper := newProviderAttemptTestWrapper(provider, observer, 0, func(context.Context, time.Duration) error { return nil })

		response, err := wrapper.Chat(ctx, &llm.ChatRequest{Model: "requested-model"})
		if err != nil || response == nil || response.Content != "completed" {
			t.Fatalf("Chat() response=%#v error=%v, want existing successful business result", response, err)
		}
		facts := observer.Facts()
		if provider.Calls() != 1 || len(facts) != 1 {
			t.Fatalf("adapter calls=%d facts=%d, want 1/1", provider.Calls(), len(facts))
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider:       "attempt-scripted",
			requestedModel: "requested-model",
			actualModel:    "actual-model",
			outcome:        "succeeded",
			usage:          llm.UsageReported,
		})
	})

	t.Run("stream timeout wins over a generic adapter setup error", func(t *testing.T) {
		deadlineCtx := newProviderAttemptDeadlineContext(context.Background())
		provider := &providerAttemptScriptedProvider{streamSteps: []providerAttemptStreamStep{{
			call: func(context.Context) (<-chan llm.ChatChunk, error) {
				deadlineCtx.Expire()
				return nil, fmt.Errorf("adapter omitted context cause: %w", llm.ErrUpstream)
			},
		}}}
		observer := &providerAttemptRecorder{}
		wrapper := NewProviderWrapper(
			provider,
			NewCircuitBreaker(Config{FailureThreshold: 1, RecoveryTimeout: time.Minute}),
			WithExecutionPolicy(ProviderExecutionPolicy{Timeout: time.Minute, RetryMax: 0, RetryBackoff: time.Second}),
			WithProviderAttemptObserver(observer),
			WithNow(steppingProviderAttemptClock(time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC), 5*time.Millisecond)),
			withProviderRuntime(func(context.Context, time.Duration) (context.Context, context.CancelFunc) {
				return deadlineCtx, func() {}
			}, func(context.Context, time.Duration) error { return nil }),
		)

		stream, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "requested-model"})
		if !errors.Is(err, context.DeadlineExceeded) || stream != nil {
			t.Fatalf("ChatStream() stream=%v error=%v, want deadline exceeded", stream, err)
		}
		facts := observer.Facts()
		if provider.StreamCalls() != 1 || len(facts) != 1 {
			t.Fatalf("adapter calls=%d facts=%d, want 1/1", provider.StreamCalls(), len(facts))
		}
		assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
			provider: "attempt-scripted", requestedModel: "requested-model", outcome: "timeout", usage: llm.UsageUnavailable,
		})
	})

	tests := []struct {
		name       string
		step       providerAttemptStep
		wantErrIs  error
		wantErrRaw string
	}{
		{name: "nil response", step: providerAttemptStep{}},
		{
			name:       "invalid response",
			step:       providerAttemptStep{err: fmt.Errorf("invalid provider payload secret-body: %w", llm.ErrInvalidResponse)},
			wantErrIs:  llm.ErrInvalidResponse,
			wantErrRaw: "secret-body",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{tt.step}}
			observer := &providerAttemptRecorder{}
			wrapper := newProviderAttemptTestWrapper(provider, observer, 0, func(context.Context, time.Duration) error { return nil })

			response, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "requested-model"})
			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) || strings.Contains(fmt.Sprint(err), tt.wantErrRaw) {
					t.Fatalf("Chat() error=%v, want stable %v classification without raw body", err, tt.wantErrIs)
				}
			} else if err != nil || response != nil {
				t.Fatalf("Chat() response=%#v error=%v, observer must not change the existing nil response result", response, err)
			}
			facts := observer.Facts()
			if provider.Calls() != 1 || len(facts) != 1 {
				t.Fatalf("adapter calls=%d facts=%d, want 1/1", provider.Calls(), len(facts))
			}
			assertProviderAttemptFact(t, facts[0], providerAttemptFactExpectation{
				provider: "attempt-scripted", requestedModel: "requested-model", outcome: "invalid_response", usage: llm.UsageUnavailable,
			})
			if tt.wantErrRaw != "" && strings.Contains(fmt.Sprintf("%#v", facts[0]), tt.wantErrRaw) {
				t.Fatal("invalid-response provider body leaked into attempt fact")
			}
		})
	}
}

func TestProviderAttemptObserverFailureDoesNotChangeBusinessResult(t *testing.T) {
	const observerCanary = "observer-private-failure-must-not-escape"
	tests := []struct {
		name     string
		observer func() *providerAttemptRecorder
	}{
		{
			name: "observer returns error",
			observer: func() *providerAttemptRecorder {
				return &providerAttemptRecorder{err: errors.New(observerCanary)}
			},
		},
		{
			name: "observer panics",
			observer: func() *providerAttemptRecorder {
				return &providerAttemptRecorder{panicValue: observerCanary}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+" on retry recovery", func(t *testing.T) {
			wantUsage := llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
			wantResponse := &llm.ChatResponse{
				Content: "recovered", Model: "actual-model", Usage: wantUsage, FinishReason: llm.FinishStop,
			}
			provider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{
				{err: fmt.Errorf("temporary: %w", llm.ErrUpstream)},
				{response: wantResponse},
			}}
			observer := tt.observer()
			sleepCalls := 0
			wrapper := newProviderAttemptTestWrapper(provider, observer, 1, func(context.Context, time.Duration) error {
				sleepCalls++
				return nil
			})

			response, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "requested-model"})
			if err != nil || response != wantResponse || response.Content != "recovered" ||
				response.Model != "actual-model" || response.FinishReason != llm.FinishStop ||
				response.Usage.Availability() != llm.UsageReported {
				t.Fatalf("Chat() response=%#v error=%v, observer failure changed retry recovery", response, err)
			}
			usage, ok := response.Usage.Summary()
			if !ok || usage.InputTokens != 1 || usage.OutputTokens != 1 || usage.TotalTokens != 2 {
				t.Fatalf("Chat() usage=%#v ok=%v, observer failure changed provider usage", usage, ok)
			}
			if provider.Calls() != 2 || sleepCalls != 1 || len(observer.Facts()) != 2 {
				t.Fatalf("adapter calls=%d sleeps=%d observer calls=%d, want 2/1/2", provider.Calls(), sleepCalls, len(observer.Facts()))
			}
		})

		t.Run(tt.name+" on terminal failure", func(t *testing.T) {
			provider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{{err: fmt.Errorf("upstream failed: %w", llm.ErrUpstream)}}}
			observer := tt.observer()
			wrapper := newProviderAttemptTestWrapper(provider, observer, 0, func(context.Context, time.Duration) error { return nil })

			_, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "requested-model"})
			if !errors.Is(err, llm.ErrUpstream) || strings.Contains(fmt.Sprint(err), observerCanary) {
				t.Fatalf("Chat() error=%v, observer failure changed terminal business error", err)
			}
			if provider.Calls() != 1 || len(observer.Facts()) != 1 {
				t.Fatalf("adapter calls=%d observer calls=%d, want 1/1", provider.Calls(), len(observer.Facts()))
			}
			_, err = wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "requested-model"})
			if !errors.Is(err, ErrCircuitOpen) || provider.Calls() != 1 || len(observer.Facts()) != 1 {
				t.Fatalf("post-failure error=%v adapter calls=%d observer calls=%d, want circuit-open/1/1", err, provider.Calls(), len(observer.Facts()))
			}
		})

		t.Run(tt.name+" on stream retry", func(t *testing.T) {
			provider := &providerAttemptScriptedProvider{streamSteps: []providerAttemptStreamStep{
				{stream: providerAttemptStream(llm.ChatChunk{Err: fmt.Errorf("first event failed: %w", llm.ErrUpstream)})},
				{stream: providerAttemptStream(llm.ChatChunk{DeltaContent: "recovered"})},
			}}
			observer := tt.observer()
			sleepCalls := 0
			wrapper := newProviderAttemptTestWrapper(provider, observer, 1, func(context.Context, time.Duration) error {
				sleepCalls++
				return nil
			})

			chunks, err := wrapper.ChatStream(context.Background(), &llm.ChatRequest{Model: "requested-model"})
			if err != nil {
				t.Fatalf("ChatStream() error=%v, observer failure changed stream setup", err)
			}
			var content string
			var terminal error
			for chunk := range chunks {
				content += chunk.DeltaContent
				if chunk.Err != nil {
					terminal = chunk.Err
				}
			}
			if content != "recovered" || terminal != nil || strings.Contains(fmt.Sprint(terminal), observerCanary) ||
				provider.StreamCalls() != 2 || sleepCalls != 1 || len(observer.Facts()) != 2 {
				t.Fatalf("content=%q terminal=%v adapter calls=%d sleeps=%d observer calls=%d, want recovered/nil/2/1/2",
					content, terminal, provider.StreamCalls(), sleepCalls, len(observer.Facts()))
			}
		})
	}
}
