package resilience

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
)

type providerAttemptFactExpectation struct {
	provider       string
	requestedModel string
	actualModel    string
	outcome        string
	usage          llm.UsageAvailability
}

func assertProviderAttemptFact(t *testing.T, fact ProviderAttemptFact, want providerAttemptFactExpectation) {
	t.Helper()

	if fact.Provider() != want.provider || fact.RequestedModel() != want.requestedModel || fact.ActualModel() != want.actualModel {
		t.Fatalf("attempt identity=%q/%q/%q, want %q/%q/%q",
			fact.Provider(), fact.RequestedModel(), fact.ActualModel(), want.provider, want.requestedModel, want.actualModel)
	}
	if got := fmt.Sprint(fact.Outcome()); got != want.outcome {
		t.Fatalf("attempt outcome=%q, want %q", got, want.outcome)
	}
	if fact.StartedAt().IsZero() {
		t.Fatal("attempt started_at must be explicit")
	}
	if fact.Duration() != 5*time.Millisecond {
		t.Fatalf("attempt duration=%s, want monotonic fake-clock duration 5ms", fact.Duration())
	}
	if got := fact.Usage().Availability(); got != want.usage {
		t.Fatalf("attempt usage availability=%q, want %q", got, want.usage)
	}
	if got := fmt.Sprint(fact.CostAvailability()); got != "unavailable" {
		t.Fatalf("attempt cost availability=%q, want explicit unavailable", got)
	}
}

func newProviderAttemptTestWrapper(
	provider llm.Provider,
	observer ProviderAttemptObserver,
	retryMax int,
	sleep func(context.Context, time.Duration) error,
) *ProviderWrapper {
	return NewProviderWrapper(
		provider,
		NewCircuitBreaker(Config{FailureThreshold: 1, RecoveryTimeout: time.Minute}),
		WithExecutionPolicy(ProviderExecutionPolicy{Timeout: time.Minute, RetryMax: retryMax, RetryBackoff: time.Second}),
		WithProviderAttemptObserver(observer),
		WithNow(steppingProviderAttemptClock(time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC), 5*time.Millisecond)),
		withProviderRuntime(context.WithTimeout, sleep),
	)
}

func steppingProviderAttemptClock(start time.Time, step time.Duration) func() time.Time {
	var mu sync.Mutex
	next := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now := next
		next = next.Add(step)
		return now
	}
}

type providerAttemptStep struct {
	response *llm.ChatResponse
	err      error
	call     func(context.Context) (*llm.ChatResponse, error)
}

type providerAttemptStreamStep struct {
	stream <-chan llm.ChatChunk
	err    error
	call   func(context.Context) (<-chan llm.ChatChunk, error)
}

type providerAttemptScriptedProvider struct {
	mu          sync.Mutex
	steps       []providerAttemptStep
	streamSteps []providerAttemptStreamStep
	calls       int
	streamCalls int
}

func (*providerAttemptScriptedProvider) Name() string { return "attempt-scripted" }

func (*providerAttemptScriptedProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{}
}

func (p *providerAttemptScriptedProvider) Chat(ctx context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	p.mu.Lock()
	index := p.calls
	p.calls++
	if index >= len(p.steps) {
		p.mu.Unlock()
		return nil, fmt.Errorf("unexpected provider adapter attempt %d", index+1)
	}
	step := p.steps[index]
	p.mu.Unlock()

	if step.call != nil {
		return step.call(ctx)
	}
	return step.response, step.err
}

func (p *providerAttemptScriptedProvider) ChatStream(ctx context.Context, _ *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	index := p.streamCalls
	p.streamCalls++
	if index >= len(p.streamSteps) {
		return nil, fmt.Errorf("unexpected provider stream adapter attempt %d", index+1)
	}
	step := p.streamSteps[index]
	if step.call != nil {
		return step.call(ctx)
	}
	return step.stream, step.err
}

func (p *providerAttemptScriptedProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *providerAttemptScriptedProvider) StreamCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streamCalls
}

func providerAttemptStream(chunks ...llm.ChatChunk) <-chan llm.ChatChunk {
	stream := make(chan llm.ChatChunk, len(chunks))
	for _, chunk := range chunks {
		stream <- chunk
	}
	close(stream)
	return stream
}

type providerAttemptDeadlineContext struct {
	parent context.Context
	done   chan struct{}
	once   sync.Once
}

func newProviderAttemptDeadlineContext(parent context.Context) *providerAttemptDeadlineContext {
	return &providerAttemptDeadlineContext{parent: parent, done: make(chan struct{})}
}

func (c *providerAttemptDeadlineContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *providerAttemptDeadlineContext) Done() <-chan struct{} { return c.done }

func (c *providerAttemptDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return c.parent.Err()
	}
}

func (c *providerAttemptDeadlineContext) Value(key any) any { return c.parent.Value(key) }

func (c *providerAttemptDeadlineContext) Expire() {
	c.once.Do(func() { close(c.done) })
}

type providerAttemptRecorder struct {
	mu         sync.Mutex
	facts      []ProviderAttemptFact
	err        error
	panicValue any
}

func (r *providerAttemptRecorder) ObserveProviderAttempt(fact ProviderAttemptFact) error {
	r.mu.Lock()
	r.facts = append(r.facts, fact)
	err := r.err
	panicValue := r.panicValue
	r.mu.Unlock()

	if panicValue != nil {
		panic(panicValue)
	}
	return err
}

func (r *providerAttemptRecorder) Facts() []ProviderAttemptFact {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ProviderAttemptFact(nil), r.facts...)
}
