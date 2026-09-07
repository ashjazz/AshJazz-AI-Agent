package resilience

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
)

// ErrProviderRejected is a stable, low-sensitivity projection for a provider response that is
// not retryable but whose original text is untrusted. It prevents upstream response bodies from
// crossing the resilience boundary while allowing callers to distinguish rejection from outage.
var ErrProviderRejected = errors.New("resilience: provider rejected request")

// ProviderAttemptFact 是一次真实底层 provider adapter 调用的低敏、不可变快照。
//
// 字段保持私有，避免 observer 改写既有证据；同时刻意不携带 context、请求、响应或
// 原始 error，防止 prompt、凭据、endpoint 和供应商正文进入指标旁路。
type ProviderAttemptFact struct {
	provider         string
	requestedModel   string
	actualModel      string
	startedAt        time.Time
	duration         time.Duration
	outcome          ProviderAttemptOutcome
	usage            llm.ProviderUsage
	costAvailability ProviderAttemptCostAvailability
}

func (f ProviderAttemptFact) Provider() string { return f.provider }

func (f ProviderAttemptFact) RequestedModel() string { return f.requestedModel }

func (f ProviderAttemptFact) ActualModel() string { return f.actualModel }

func (f ProviderAttemptFact) StartedAt() time.Time { return f.startedAt }

func (f ProviderAttemptFact) Duration() time.Duration { return f.duration }

func (f ProviderAttemptFact) Outcome() ProviderAttemptOutcome { return f.outcome }

func (f ProviderAttemptFact) Usage() llm.ProviderUsage { return f.usage }

func (f ProviderAttemptFact) CostAvailability() ProviderAttemptCostAvailability {
	return f.costAvailability
}

// ProviderAttemptObserver 是 resilience 内核面向 metrics/telemetry adapter 的窄端口。
// observer 的错误或 panic 都不能反向改变模型调用、重试、熔断和流式输出语义。
type ProviderAttemptObserver interface {
	ObserveProviderAttempt(ProviderAttemptFact) error
}

// ProviderWrapper 用断路器保护 llm.Provider。
//
// 这里刻意保持 llm.Provider 接口不变：调用方仍然只看到 Name/Capabilities/Chat/ChatStream。
// wrapper 只在内部判断错误是否属于 llm.ErrUpstream，并据此决定是否计入断路器失败。
type ProviderWrapper struct {
	provider        llm.Provider
	breaker         CircuitBreaker
	tracer          obs.Tracer
	feature         string
	now             func() time.Time
	executionPolicy *ProviderExecutionPolicy
	attemptObserver ProviderAttemptObserver
	withTimeout     func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	sleep           func(context.Context, time.Duration) error
}

// ProviderWrapperOption 描述 provider wrapper 的可选装配项。
type ProviderWrapperOption func(*ProviderWrapper)

// NewProviderWrapper 创建带 resilience 保护的 provider。
func NewProviderWrapper(provider llm.Provider, breaker CircuitBreaker, options ...ProviderWrapperOption) *ProviderWrapper {
	wrapper := &ProviderWrapper{
		provider:    provider,
		breaker:     breaker,
		now:         time.Now,
		withTimeout: context.WithTimeout,
		sleep:       providerSleep,
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		option(wrapper)
	}
	if wrapper.now == nil {
		wrapper.now = time.Now
	}
	return wrapper
}

// WithTracer 配置 provider outcome 的语义观测出口。
func WithTracer(tracer obs.Tracer) ProviderWrapperOption {
	return func(wrapper *ProviderWrapper) {
		wrapper.tracer = tracer
	}
}

// WithFeature 配置 provider trace 的功能维度。
func WithFeature(feature string) ProviderWrapperOption {
	return func(wrapper *ProviderWrapper) {
		wrapper.feature = feature
	}
}

// WithNow 注入时钟，便于测试稳定断言 provider latency。
func WithNow(now func() time.Time) ProviderWrapperOption {
	return func(wrapper *ProviderWrapper) {
		wrapper.now = now
	}
}

// WithExecutionPolicy opts this wrapper into a single request-level timeout/retry lifecycle.
// A zero policy is rejected at call time instead of silently inventing operational semantics.
func WithExecutionPolicy(policy ProviderExecutionPolicy) ProviderWrapperOption {
	return func(wrapper *ProviderWrapper) {
		copy := policy
		wrapper.executionPolicy = &copy
	}
}

// WithProviderAttemptObserver 配置每次真实 provider 网络尝试的事实出口。
func WithProviderAttemptObserver(observer ProviderAttemptObserver) ProviderWrapperOption {
	return func(wrapper *ProviderWrapper) {
		wrapper.attemptObserver = observer
	}
}

// withProviderRuntime is intentionally package-private: test seams belong to resilience, not
// the cmd composition root that resolves production configuration.
func withProviderRuntime(withTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc), sleep func(context.Context, time.Duration) error) ProviderWrapperOption {
	return func(wrapper *ProviderWrapper) {
		if withTimeout != nil {
			wrapper.withTimeout = withTimeout
		}
		if sleep != nil {
			wrapper.sleep = sleep
		}
	}
}

func (p *ProviderWrapper) Name() string {
	if p == nil || p.provider == nil {
		return ""
	}
	return p.provider.Name()
}

func (p *ProviderWrapper) Capabilities(model string) llm.ProviderCapabilities {
	if p == nil || p.provider == nil {
		return llm.ProviderCapabilities{}
	}
	return p.provider.Capabilities(model)
}

func (p *ProviderWrapper) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}

	startedAt := p.now()
	var response *llm.ChatResponse
	ctx = nonNilContext(ctx)
	callResult := p.call(ctx, func(callCtx context.Context) error {
		return p.executeChat(callCtx, req, &response)
	})
	err := sanitizeProviderError(callResult.err())
	p.recordOutcome(ctx, providerOutcomeObservation{
		RequestedModel: requestedModel(req),
		ActualModel:    actualModel(response),
		Err:            err,
		StartedAt:      startedAt,
		EndedAt:        p.now(),
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

func (p *ProviderWrapper) ChatStream(ctx context.Context, req *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	ctx = nonNilContext(ctx)
	// One slot carries a data chunk that the consumer has not yet read; the second guarantees a
	// deadline terminal event can still be observed rather than being mistaken for a clean EOF.
	forwarded := make(chan llm.ChatChunk, 2)
	started := make(chan error, 1)
	startedAt := p.now()

	go p.runStream(ctx, req, forwarded, started, startedAt)
	select {
	case err := <-started:
		if err != nil {
			return nil, err
		}
		return forwarded, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *ProviderWrapper) runStream(ctx context.Context, req *llm.ChatRequest, forwarded chan<- llm.ChatChunk, started chan<- error, startedAt time.Time) {
	defer close(forwarded)
	startSent := false
	callResult := p.call(ctx, func(callCtx context.Context) error {
		return p.executeStream(callCtx, req, forwarded, func(err error) {
			if !startSent {
				started <- err
				startSent = true
			}
		})
	})
	err := sanitizeProviderError(callResult.err())
	if !startSent {
		started <- err
	}
	p.recordOutcome(ctx, providerOutcomeObservation{
		RequestedModel: requestedModel(req),
		Err:            err,
		StartedAt:      startedAt,
		EndedAt:        p.now(),
	})
}

func (p *ProviderWrapper) validate() error {
	if p == nil {
		return fmt.Errorf("provider wrapper is required")
	}
	if p.provider == nil {
		return fmt.Errorf("provider wrapper provider is required")
	}
	if p.breaker == nil {
		return fmt.Errorf("provider wrapper circuit breaker is required")
	}
	if p.executionPolicy != nil {
		if err := p.executionPolicy.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (p *ProviderWrapper) executeChat(ctx context.Context, req *llm.ChatRequest, response **llm.ChatResponse) error {
	executionCtx, cancel := p.executionContext(ctx)
	defer cancel()
	return p.executeWithRetry(executionCtx, func(attemptCtx context.Context) error {
		attempt := p.beginProviderAttempt(req)
		got, err := p.provider.Chat(attemptCtx, req)
		attempt.finishChat(attemptCtx, got, err)
		*response = got
		return err
	})
}

func (p *ProviderWrapper) executeStream(ctx context.Context, req *llm.ChatRequest, forwarded chan<- llm.ChatChunk, signalStarted func(error)) (resultErr error) {
	executionCtx, cancel := p.executionContext(ctx)
	defer cancel()

	source, first, attempt, err := p.openStreamBeforeFirstOutput(executionCtx, req)
	if err != nil {
		signalStarted(sanitizeProviderError(err))
		return err
	}
	if attempt != nil {
		defer func() {
			attempt.finishStream(executionCtx, resultErr)
		}()
	}
	signalStarted(nil)
	if first != nil {
		if err := p.forwardStreamChunk(executionCtx, forwarded, *first); err != nil {
			return err
		}
		if first.Err != nil {
			return first.Err
		}
	}
	for {
		select {
		case <-executionCtx.Done():
			return p.streamContextError(ctx, executionCtx, forwarded)
		case chunk, open := <-source:
			if !open {
				return nil
			}
			if attempt != nil {
				attempt.captureStreamUsage(chunk.Usage)
			}
			if err := p.forwardStreamChunk(executionCtx, forwarded, chunk); err != nil {
				return p.streamContextError(ctx, executionCtx, forwarded)
			}
			if chunk.Err != nil {
				return chunk.Err
			}
		}
	}
}

// openStreamBeforeFirstOutput owns the only replay-safe stream retry window. A failed HTTP/SSE
// connection or an upstream terminal event before any chunk is forwarded can be retried; once a
// chunk crosses this wrapper boundary, replay could duplicate text or tool-call side effects.
func (p *ProviderWrapper) openStreamBeforeFirstOutput(ctx context.Context, req *llm.ChatRequest) (<-chan llm.ChatChunk, *llm.ChatChunk, *providerNetworkAttempt, error) {
	for attempt := 0; ; attempt++ {
		networkAttempt := p.beginProviderAttempt(req)
		source, err := p.provider.ChatStream(ctx, req)
		if err == nil && source == nil {
			networkAttempt.markInvalidResponse()
			// 业务路径沿用既有 upstream 分类以保持 retry/breaker 语义；attempt fact 则记录
			// 更准确的 invalid_response，避免把 provider 契约违规误算成网络失败。
			networkAttempt.finishStream(ctx, llm.ErrInvalidResponse)
			err = fmt.Errorf("provider returned an empty stream: %w", llm.ErrUpstream)
		}
		if err != nil {
			networkAttempt.finishStream(ctx, err)
			if retry, retryErr := p.retryBeforeFirstOutput(ctx, attempt, err); retry {
				continue
			} else {
				return nil, nil, nil, retryErr
			}
		}

		select {
		case <-ctx.Done():
			networkAttempt.finishStream(ctx, ctx.Err())
			return nil, nil, nil, ctx.Err()
		case first, open := <-source:
			if !open {
				networkAttempt.finishStream(ctx, nil)
				return source, nil, nil, nil
			}
			networkAttempt.captureStreamUsage(first.Usage)
			if first.Err != nil {
				networkAttempt.finishStream(ctx, first.Err)
				if retry, retryErr := p.retryBeforeFirstOutput(ctx, attempt, first.Err); retry {
					continue
				} else {
					return nil, nil, nil, retryErr
				}
			}
			return source, &first, networkAttempt, nil
		}
	}
}

func (p *ProviderWrapper) retryBeforeFirstOutput(ctx context.Context, attempt int, err error) (bool, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if p.executionPolicy == nil || !errors.Is(err, llm.ErrUpstream) || attempt >= p.executionPolicy.RetryMax {
		return false, err
	}
	if err := p.sleep(ctx, providerRetryDelay(attempt, p.executionPolicy.RetryBackoff)); err != nil {
		return false, err
	}
	return true, nil
}

func (p *ProviderWrapper) forwardStreamChunk(ctx context.Context, forwarded chan<- llm.ChatChunk, chunk llm.ChatChunk) error {
	terminalErr := chunk.Err
	if terminalErr != nil {
		chunk.Err = sanitizeProviderError(terminalErr)
	}
	select {
	case forwarded <- chunk:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *ProviderWrapper) streamContextError(parent, execution context.Context, forwarded chan<- llm.ChatChunk) error {
	err := execution.Err()
	if parent.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		// The caller is still alive, so the internal request-budget exhaustion is a required stream
		// terminal event, not an optional best-effort notification. Waiting for queue space keeps a
		// slow consumer from mistaking a truncated response for clean EOF; parent cancellation still
		// releases an abandoned stream.
		select {
		case forwarded <- llm.ChatChunk{Err: context.DeadlineExceeded}:
		case <-parent.Done():
		}
	}
	return err
}

func (p *ProviderWrapper) executionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.executionPolicy == nil {
		return ctx, func() {}
	}
	return p.withTimeout(ctx, p.executionPolicy.Timeout)
}

func (p *ProviderWrapper) executeWithRetry(ctx context.Context, call func(context.Context) error) error {
	if p.executionPolicy == nil {
		return call(ctx)
	}
	return retryProviderCall(ctx, *p.executionPolicy, p.sleep, call)
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func sanitizeProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, llm.ErrInvalidResponse) {
		return llm.ErrInvalidResponse
	}
	if errors.Is(err, llm.ErrRateLimit) {
		return fmt.Errorf("provider request failed: %w", errors.Join(llm.ErrUpstream, llm.ErrRateLimit))
	}
	if errors.Is(err, llm.ErrUpstream) {
		return fmt.Errorf("provider request failed: %w", llm.ErrUpstream)
	}
	if errors.Is(err, ErrCircuitOpen) {
		return err
	}
	return ErrProviderRejected
}

// providerNetworkAttempt 的生命周期覆盖一次 adapter 调用直至其真实终态。
// 流式调用不能在收到 HTTP/SSE channel 时提前成功：只有 terminal chunk、关闭或错误
// 才能结算，以免把“连接建立成功但消费失败”误记为成功。
type providerNetworkAttempt struct {
	wrapper         *ProviderWrapper
	provider        string
	requestedModel  string
	startedAt       time.Time
	usage           llm.ProviderUsage
	invalidResponse bool
	once            sync.Once
}

func (p *ProviderWrapper) beginProviderAttempt(req *llm.ChatRequest) *providerNetworkAttempt {
	return &providerNetworkAttempt{
		wrapper:        p,
		provider:       p.Name(),
		requestedModel: requestedModel(req),
		startedAt:      p.now(),
		usage:          llm.NewUnavailableProviderUsage(),
	}
}

func (a *providerNetworkAttempt) markInvalidResponse() {
	if a != nil {
		a.invalidResponse = true
	}
}

func (a *providerNetworkAttempt) captureStreamUsage(summary *llm.Usage) {
	if a == nil || summary == nil {
		return
	}
	usage, err := llm.NewReportedProviderUsage(*summary)
	if err != nil {
		a.invalidResponse = true
		a.usage = llm.NewUnavailableProviderUsage()
		return
	}
	a.usage = usage
}

func (a *providerNetworkAttempt) finishChat(ctx context.Context, response *llm.ChatResponse, err error) {
	if a == nil {
		return
	}
	outcome := providerChatAttemptOutcome(response, providerAttemptTerminalError(ctx, err))
	actual := ""
	usage := llm.NewUnavailableProviderUsage()
	if outcome == ProviderAttemptSucceeded {
		actual = actualModel(response)
		if response.Usage.Availability() == llm.UsageReported {
			usage = response.Usage
		}
	}
	a.finish(outcome, actual, usage)
}

func (a *providerNetworkAttempt) finishStream(ctx context.Context, err error) {
	if a == nil {
		return
	}
	outcome := providerAttemptOutcomeFromError(providerAttemptTerminalError(ctx, err))
	if outcome == ProviderAttemptSucceeded && a.invalidResponse {
		outcome = ProviderAttemptInvalidResponse
	}
	a.finish(outcome, "", a.usage)
}

func (a *providerNetworkAttempt) finish(outcome ProviderAttemptOutcome, actualModel string, usage llm.ProviderUsage) {
	if a == nil || a.wrapper == nil {
		return
	}
	a.once.Do(func() {
		endedAt := a.wrapper.now()
		duration := endedAt.Sub(a.startedAt)
		if duration < 0 {
			duration = 0
		}
		fact := ProviderAttemptFact{
			provider:         a.provider,
			requestedModel:   a.requestedModel,
			actualModel:      actualModel,
			startedAt:        a.startedAt,
			duration:         duration,
			outcome:          outcome,
			usage:            usage,
			costAvailability: ProviderAttemptCostUnavailable,
		}
		a.wrapper.observeProviderAttempt(fact)
	})
}

func (p *ProviderWrapper) observeProviderAttempt(fact ProviderAttemptFact) {
	if p == nil || p.attemptObserver == nil {
		return
	}
	// metrics/telemetry 是旁路；局部 recover 只隔离 observer，不吞掉业务代码 panic。
	defer func() {
		_ = recover()
	}()
	_ = p.attemptObserver.ObserveProviderAttempt(fact)
}

func (p *ProviderWrapper) call(ctx context.Context, fn func(context.Context) error) providerCallResult {
	result := providerCallResult{}
	breakerErr := p.breaker.Call(ctx, func(ctx context.Context) error {
		err := fn(ctx)
		if err == nil {
			return nil
		}

		result.originalErr = err
		if errors.Is(err, llm.ErrUpstream) {
			return err
		}

		// 400/401/403/404 等调用方错误必须原样返回给上层，但不能让 breaker 记录失败。
		// 否则一个坏请求会把整个 provider 熔断，影响其它正常请求。
		return nil
	})
	result.breakerErr = breakerErr
	return result
}

type providerCallResult struct {
	originalErr error
	breakerErr  error
}

func (r providerCallResult) err() error {
	if r.breakerErr != nil {
		return r.breakerErr
	}
	return r.originalErr
}

type providerOutcomeObservation struct {
	RequestedModel string
	ActualModel    string
	Err            error
	StartedAt      time.Time
	EndedAt        time.Time
}

func (p *ProviderWrapper) recordOutcome(ctx context.Context, observation providerOutcomeObservation) {
	if p == nil || p.tracer == nil {
		return
	}
	if strings.TrimSpace(p.feature) == "" {
		return
	}

	identity, ok := obs.CorrelationIdentityFromContext(ctx)
	if !ok || strings.TrimSpace(identity.AITraceID) == "" {
		return
	}

	outcomeStatus, failureStatus, degraded, rateLimited := classifyProviderOutcome(observation)
	trace := obs.NewTrace(
		identity.AITraceID,
		p.feature,
		observation.EndedAt,
		obs.WithCorrelationIdentity(identity),
		obs.WithObservationType(obs.ObservationTypeGeneration),
		obs.WithModel(observation.ActualModel),
		obs.WithLatency(0, observation.EndedAt.Sub(observation.StartedAt).Milliseconds()),
		obs.WithOutcome(outcomeStatus),
	)
	trace.FailureStatus = failureStatus
	trace.ProviderName = p.Name()
	trace.RequestedModel = observation.RequestedModel
	trace.CircuitState = string(p.breaker.State())
	trace.Degraded = degraded
	trace.RateLimited = rateLimited

	_ = obs.RecordWithExportFailureProtection(ctx, p.tracer, trace)
}

func classifyProviderOutcome(observation providerOutcomeObservation) (outcomeStatus, failureStatus string, degraded, rateLimited bool) {
	if observation.Err != nil {
		rateLimited = errors.Is(observation.Err, llm.ErrRateLimit)
		switch {
		case errors.Is(observation.Err, context.DeadlineExceeded):
			return "failure", string(obs.FailureTimeout), false, false
		case rateLimited:
			return "failure", string(obs.FailureRateLimit), false, true
		case errors.Is(observation.Err, llm.ErrUpstream), errors.Is(observation.Err, ErrCircuitOpen):
			return "failure", string(obs.FailureUpstream), false, false
		default:
			return "failure", string(obs.FailureCallerError), false, false
		}
	}

	degraded = observation.ActualModel != "" &&
		observation.RequestedModel != "" &&
		observation.ActualModel != observation.RequestedModel
	if degraded {
		return "degraded", "", true, false
	}
	return "success", "", false, false
}

func requestedModel(req *llm.ChatRequest) string {
	if req == nil {
		return ""
	}
	return req.Model
}

func actualModel(response *llm.ChatResponse) string {
	if response == nil {
		return ""
	}
	return response.Model
}
