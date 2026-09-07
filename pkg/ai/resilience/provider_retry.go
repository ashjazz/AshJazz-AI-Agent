package resilience

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
)

const (
	defaultProviderTimeout      = 60 * time.Second
	defaultProviderRetryMax     = 2
	defaultProviderRetryBackoff = time.Second
	maxProviderRetryMax         = 2
)

var providerRetryDelayMultipliers = [...]time.Duration{1, 3}

// ProviderAttemptOutcome 是每次真实进入底层 provider adapter 后的闭合终态。
//
// 这里不暴露 HTTP status、错误正文或供应商自定义类别：这些值会制造高基数指标，
// 也可能把响应正文带入观测旁路。429/5xx 统一归为 failed，更细的业务错误分类仍由
// 原调用链通过 errors.Is 保留。
type ProviderAttemptOutcome string

const (
	ProviderAttemptSucceeded       ProviderAttemptOutcome = "succeeded"
	ProviderAttemptFailed          ProviderAttemptOutcome = "failed"
	ProviderAttemptTimeout         ProviderAttemptOutcome = "timeout"
	ProviderAttemptCancelled       ProviderAttemptOutcome = "cancelled"
	ProviderAttemptInvalidResponse ProviderAttemptOutcome = "invalid_response"
)

// ProviderAttemptCostAvailability 区分“没有可信成本事实”和合法的数值零成本。
// 当前 wrapper 没有价格来源，因此 T217 只产生 unavailable；actual/estimated 为后续
// 价格 adapter 保留闭合集合，但不会在本层猜测金额或币种。
type ProviderAttemptCostAvailability string

const (
	ProviderAttemptCostUnavailable ProviderAttemptCostAvailability = "unavailable"
	ProviderAttemptCostActual      ProviderAttemptCostAvailability = "actual"
	ProviderAttemptCostEstimated   ProviderAttemptCostAvailability = "estimated"
)

func providerAttemptOutcomeFromError(err error) ProviderAttemptOutcome {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ProviderAttemptTimeout
	case errors.Is(err, context.Canceled):
		return ProviderAttemptCancelled
	case errors.Is(err, llm.ErrInvalidResponse):
		return ProviderAttemptInvalidResponse
	case err != nil:
		return ProviderAttemptFailed
	default:
		return ProviderAttemptSucceeded
	}
}

// providerAttemptTerminalError 校准第三方 adapter 遗漏 context cause 的情况。
// retry 层本来就以已结束的 context 作为业务终态；attempt fact 必须使用同一事实，
// 但这里只影响观测分类，不改写 adapter 原始返回或既有 retry/breaker 决策。
func providerAttemptTerminalError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	return err
}

func providerChatAttemptOutcome(response *llm.ChatResponse, err error) ProviderAttemptOutcome {
	if outcome := providerAttemptOutcomeFromError(err); outcome != ProviderAttemptSucceeded {
		return outcome
	}
	if response == nil {
		return ProviderAttemptInvalidResponse
	}
	return ProviderAttemptSucceeded
}

// ProviderExecutionPolicy is provider-neutral execution policy. It intentionally contains no
// OpenAI/Anthropic fields: adapters own protocol-specific configuration while resilience owns
// timeout, retry, cancellation, and terminal stream semantics.
type ProviderExecutionPolicy struct {
	Timeout      time.Duration
	RetryMax     int
	RetryBackoff time.Duration
}

func DefaultProviderExecutionPolicy() ProviderExecutionPolicy {
	return ProviderExecutionPolicy{
		Timeout:      defaultProviderTimeout,
		RetryMax:     defaultProviderRetryMax,
		RetryBackoff: defaultProviderRetryBackoff,
	}
}

func (p ProviderExecutionPolicy) Validate() error {
	if p.Timeout <= 0 {
		return fmt.Errorf("provider execution timeout must be positive")
	}
	if p.RetryMax < 0 || p.RetryMax > maxProviderRetryMax {
		return fmt.Errorf("provider execution retry limit is invalid")
	}
	if p.RetryBackoff <= 0 {
		return fmt.Errorf("provider execution retry backoff must be positive")
	}
	return nil
}

func retryProviderCall(ctx context.Context, policy ProviderExecutionPolicy, sleep func(context.Context, time.Duration) error, call func(context.Context) error) error {
	for attempt := 0; ; attempt++ {
		err := call(ctx)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !errors.Is(err, llm.ErrUpstream) || attempt >= policy.RetryMax {
			return err
		}
		if err := sleep(ctx, providerRetryDelay(attempt, policy.RetryBackoff)); err != nil {
			return err
		}
	}
}

func providerRetryDelay(attempt int, base time.Duration) time.Duration {
	if attempt >= len(providerRetryDelayMultipliers) {
		return base * providerRetryDelayMultipliers[len(providerRetryDelayMultipliers)-1]
	}
	return base * providerRetryDelayMultipliers[attempt]
}

func providerSleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
