package observability

import (
	"context"
	"errors"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	"github.com/ashjazz/Longtermism/pkg/ai/resilience"
)

var (
	errProviderAttemptMetricsUnavailable = errors.New("provider attempt metrics are unavailable")
	errProviderAttemptCostIncomplete     = errors.New("provider attempt cost fact is incomplete")
)

// NewProviderAttemptMetricsObserver 把 resilience 的不可变 network-attempt fact 投影到
// 应用 OTel metrics 端口。返回核心层窄接口而不是暴露 adapter 具体类型，避免调用方
// 依赖 app-layer 实现细节；T219 只需在 composition root 创建一次并注入 wrapper。
func NewProviderAttemptMetricsObserver(metrics *Metrics) resilience.ProviderAttemptObserver {
	return &providerAttemptMetricsObserver{metrics: metrics}
}

type providerAttemptMetricsObserver struct {
	metrics *Metrics
}

func (observer *providerAttemptMetricsObserver) ObserveProviderAttempt(fact resilience.ProviderAttemptFact) error {
	if observer == nil || observer.metrics == nil {
		return errProviderAttemptMetricsUnavailable
	}

	usage := fact.Usage()
	metric := LLMMetric{
		Provider:          fact.Provider(),
		RequestedModel:    fact.RequestedModel(),
		ActualModel:       fact.ActualModel(),
		Outcome:           string(fact.Outcome()),
		Duration:          fact.Duration(),
		UsageAvailability: usage.Availability(),
		CostAvailability:  fact.CostAvailability(),
	}
	if usage.Availability() == llm.UsageReported {
		summary, reported := usage.Summary()
		if !reported {
			return ErrInvalidMetricValue
		}
		metric.InputTokens = int64(summary.InputTokens)
		metric.OutputTokens = int64(summary.OutputTokens)
	}

	// T217 当前只拥有 cost availability，没有金额、币种和估算状态。若未来核心 fact
	// 声明成本可用却仍缺这些不可分割字段，必须拒绝该旁路记录，不能猜测零成本。
	if metric.CostAvailability != resilience.ProviderAttemptCostUnavailable {
		return errProviderAttemptCostIncomplete
	}

	// Attempt fact 故意不携带请求 context 或 identity。使用独立 background context
	// 可避免已取消的业务请求抑制最终指标，也不会引入高基数标签。
	return observer.metrics.RecordLLM(context.Background(), metric)
}
