// Package observability contains application-facing observability adapters.
package observability

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	"github.com/ashjazz/Longtermism/pkg/ai/resilience"
	"go.opentelemetry.io/otel/attribute"
	metricapi "go.opentelemetry.io/otel/metric"
)

const (
	metricHTTPServerRequestCount    = "longtermism.http.server.request.count"
	metricHTTPServerRequestDuration = "longtermism.http.server.request.duration"
	metricLLMRequestCount           = "longtermism.llm.request.count"
	metricLLMDuration               = "longtermism.llm.duration"
	metricLLMTokens                 = "longtermism.llm.tokens"
	metricLLMCost                   = "longtermism.llm.cost"
	metricEvalResult                = "longtermism.eval.result"
	metricEvalScore                 = "longtermism.eval.score"
	metricScoreProjection           = "longtermism.score.projection"
	metricScoreWorkerQueue          = "longtermism.score.worker.queue"
)

const metricOtherLabelValue = "other"

var (
	allowedHTTPMethods = metricLabelSet("GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS")
	// openai 是当前生产 adapter 的稳定 Name()；openai-compatible 保留给已校验的
	// 通用兼容 provider 配置。二者均是有限枚举，未知供应商仍收敛为 other。
	allowedProviders          = metricLabelSet("openai", "openai-compatible")
	allowedOutcomes           = metricLabelSet("succeeded", "failed", "cancelled", "timeout", "invalid_response")
	allowedCurrencies         = metricLabelSet("USD")
	allowedEstimates          = metricLabelSet("estimated", "actual", "unavailable")
	allowedEvaluators         = metricLabelSet("deterministic", "llm_judge")
	allowedEvalStatus         = metricLabelSet("passed", "failed", "skipped", "error")
	allowedBackends           = metricLabelSet("langfuse")
	allowedProjectionStatuses = metricLabelSet("queued", "sent", "failed", "dropped", "not_configured")
)

var ErrInvalidMetricValue = errors.New("invalid metric value")

// Metrics 是应用业务代码使用的指标语义端口。它不认识 Prometheus、Collector 或任何
// 平台 endpoint；这些边界由 OTel MeterProvider 的装配层负责，避免业务层被后端绑定。
type Metrics struct {
	httpRequests     metricapi.Int64Counter
	httpDuration     metricapi.Float64Histogram
	llmRequests      metricapi.Int64Counter
	llmDuration      metricapi.Float64Histogram
	llmTokens        metricapi.Int64Counter
	llmCost          metricapi.Float64Counter
	evalResults      metricapi.Int64Counter
	evalScore        metricapi.Float64Histogram
	scoreProjections metricapi.Int64Counter
	scoreWorkerQueue metricapi.Int64Gauge
	labels           metricLabelPolicy
}

// MetricLabelPolicy is the explicit allowlist for dimensions whose vocabulary depends on
// application configuration. Routes and model IDs may otherwise contain unbounded user or
// provider values, so they must never become labels implicitly.
type MetricLabelPolicy struct {
	AllowedRoutes      []string
	AllowedModels      []string
	AllowedMetricNames []string
}

type metricLabelPolicy struct {
	routes      map[string]struct{}
	models      map[string]struct{}
	metricNames map[string]struct{}
}

// MetricsOption changes only explicit metric-label policy during wiring.
type MetricsOption func(*metricLabelPolicy)

// WithMetricLabelPolicy defensively copies configuration so a later caller mutation cannot
// silently create new time series in a running process.
func WithMetricLabelPolicy(policy MetricLabelPolicy) MetricsOption {
	return func(target *metricLabelPolicy) {
		target.routes = metricLabelSet(policy.AllowedRoutes...)
		target.models = metricLabelSet(policy.AllowedModels...)
		target.metricNames = metricLabelSet(policy.AllowedMetricNames...)
	}
}

// HTTPMetric keeps request facts at the API boundary. Identity fields are intentionally
// accepted for call-site ergonomics but never become metric attributes: they belong in spans,
// structured logs, and smoke reports where high cardinality is safe and useful.
type HTTPMetric struct {
	RouteTemplate string
	RawRoute      string
	Method        string
	StatusCode    int
	Duration      time.Duration
	RequestID     string
	TraceID       string
	SpanID        string
	SmokeRunID    string
}

// LLMMetric describes one provider attempt. Requested and actual models have distinct
// lifecycles: routing determines the requested model, while provider output records the actual one.
type LLMMetric struct {
	Provider          string
	RequestedModel    string
	ActualModel       string
	Outcome           string
	Duration          time.Duration
	UsageAvailability llm.UsageAvailability
	InputTokens       int64
	OutputTokens      int64
	CostAvailability  resilience.ProviderAttemptCostAvailability
	Cost              float64
	Currency          string
	EstimateStatus    string
	AITraceID         string
	SessionID         string
	TraceID           string
	SpanID            string
	PromptHash        string
}

// EvalMetric captures local evaluation results without promoting evidence identities to labels.
type EvalMetric struct {
	Evaluator  string
	Status     string
	MetricName string
	Score      float64
	RequestID  string
	AITraceID  string
	TraceID    string
	SpanID     string
	PromptHash string
}

// ScoreProjectionMetric records the asynchronous platform-projection outcome.
type ScoreProjectionMetric struct {
	Backend    string
	Status     string
	RequestID  string
	AITraceID  string
	TraceID    string
	SpanID     string
	SmokeRunID string
}

// ScoreQueueMetric records the current worker queue depth for one projection backend.
type ScoreQueueMetric struct {
	Backend   string
	Depth     int64
	RequestID string
	TraceID   string
	SpanID    string
	SessionID string
}

// NewMetrics constructs the complete first-wave instrument set once during application wiring.
// Instrument creation can fail for an invalid provider, so startup fails explicitly instead of
// silently producing partial telemetry that would mislead production investigations.
func NewMetrics(meter metricapi.Meter, options ...MetricsOption) (*Metrics, error) {
	httpRequests, err := meter.Int64Counter(metricHTTPServerRequestCount)
	if err != nil {
		return nil, err
	}
	httpDuration, err := meter.Float64Histogram(metricHTTPServerRequestDuration, metricapi.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	llmRequests, err := meter.Int64Counter(metricLLMRequestCount)
	if err != nil {
		return nil, err
	}
	llmDuration, err := meter.Float64Histogram(metricLLMDuration, metricapi.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	llmTokens, err := meter.Int64Counter(metricLLMTokens, metricapi.WithUnit("{token}"))
	if err != nil {
		return nil, err
	}
	llmCost, err := meter.Float64Counter(metricLLMCost)
	if err != nil {
		return nil, err
	}
	evalResults, err := meter.Int64Counter(metricEvalResult)
	if err != nil {
		return nil, err
	}
	evalScore, err := meter.Float64Histogram(metricEvalScore)
	if err != nil {
		return nil, err
	}
	scoreProjections, err := meter.Int64Counter(metricScoreProjection)
	if err != nil {
		return nil, err
	}
	scoreWorkerQueue, err := meter.Int64Gauge(metricScoreWorkerQueue)
	if err != nil {
		return nil, err
	}

	labels := metricLabelPolicy{}
	for _, option := range options {
		if option != nil {
			option(&labels)
		}
	}

	return &Metrics{httpRequests, httpDuration, llmRequests, llmDuration, llmTokens, llmCost, evalResults, evalScore, scoreProjections, scoreWorkerQueue, labels}, nil
}

func (m *Metrics) RecordHTTP(ctx context.Context, input HTTPMetric) error {
	if input.Duration < 0 {
		return ErrInvalidMetricValue
	}
	attributes := metricapi.WithAttributes(m.httpAttributes(input)...)
	m.httpRequests.Add(ctx, 1, attributes)
	m.httpDuration.Record(ctx, input.Duration.Seconds(), attributes)
	return nil
}

func (m *Metrics) RecordLLM(ctx context.Context, input LLMMetric) error {
	if err := validateLLMMetric(input); err != nil {
		return ErrInvalidMetricValue
	}
	m.llmRequests.Add(ctx, 1, metricapi.WithAttributes(m.llmRequestAttributes(input)...))
	m.llmDuration.Record(ctx, input.Duration.Seconds(), metricapi.WithAttributes(m.llmRequestAttributes(input)...))
	// Availability 是领域事实的一部分：unavailable 不是合法的数值零，不能执行
	// Add(0) 制造一个“provider 明确报告零 token/成本”的时序。
	if input.UsageAvailability == llm.UsageReported {
		m.llmTokens.Add(ctx, input.InputTokens, metricapi.WithAttributes(m.llmTokenAttributes(input, "input")...))
		m.llmTokens.Add(ctx, input.OutputTokens, metricapi.WithAttributes(m.llmTokenAttributes(input, "output")...))
	}
	if input.CostAvailability == resilience.ProviderAttemptCostActual ||
		input.CostAvailability == resilience.ProviderAttemptCostEstimated {
		m.llmCost.Add(ctx, input.Cost, metricapi.WithAttributes(m.llmCostAttributes(input)...))
	}
	return nil
}

func validateLLMMetric(input LLMMetric) error {
	if input.Duration < 0 || input.InputTokens < 0 || input.OutputTokens < 0 || !isFiniteNonNegative(input.Cost) {
		return ErrInvalidMetricValue
	}
	switch input.UsageAvailability {
	case llm.UsageUnavailable, llm.UsageReported:
	default:
		return ErrInvalidMetricValue
	}
	switch input.CostAvailability {
	case resilience.ProviderAttemptCostUnavailable,
		resilience.ProviderAttemptCostActual,
		resilience.ProviderAttemptCostEstimated:
		return nil
	default:
		return ErrInvalidMetricValue
	}
}

func (m *Metrics) RecordEval(ctx context.Context, input EvalMetric) error {
	if !isFiniteNonNegative(input.Score) {
		return ErrInvalidMetricValue
	}
	attributes := metricapi.WithAttributes(m.evalAttributes(input)...)
	m.evalResults.Add(ctx, 1, attributes)
	m.evalScore.Record(ctx, input.Score, attributes)
	return nil
}

func (m *Metrics) RecordScoreProjection(ctx context.Context, input ScoreProjectionMetric) error {
	m.scoreProjections.Add(ctx, 1, metricapi.WithAttributes(scoreProjectionAttributes(input)...))
	return nil
}

func (m *Metrics) RecordScoreQueue(ctx context.Context, input ScoreQueueMetric) error {
	if input.Depth < 0 {
		return ErrInvalidMetricValue
	}
	m.scoreWorkerQueue.Record(ctx, input.Depth, metricapi.WithAttributes(attribute.String("backend", allowedOrOther(allowedBackends, input.Backend))))
	return nil
}

func (m *Metrics) httpAttributes(input HTTPMetric) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("http.route", m.labels.allowedOrOther(m.labels.routes, input.RouteTemplate)),
		attribute.String("http.request.method", allowedOrOther(allowedHTTPMethods, input.Method)),
		attribute.String("http.response.status_class", statusClass(input.StatusCode)),
	}
}

func (m *Metrics) llmRequestAttributes(input LLMMetric) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("gen_ai.provider.name", allowedOrOther(allowedProviders, input.Provider)),
		attribute.String("gen_ai.request.model", m.labels.allowedOrOther(m.labels.models, input.RequestedModel)),
		attribute.String("outcome", allowedOrOther(allowedOutcomes, input.Outcome)),
	}
}

func (m *Metrics) llmTokenAttributes(input LLMMetric, tokenType string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("gen_ai.provider.name", allowedOrOther(allowedProviders, input.Provider)),
		attribute.String("gen_ai.response.model", m.labels.allowedOrOther(m.labels.models, input.ActualModel)),
		attribute.String("gen_ai.token.type", tokenType),
	}
}

func (m *Metrics) llmCostAttributes(input LLMMetric) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("gen_ai.provider.name", allowedOrOther(allowedProviders, input.Provider)),
		attribute.String("gen_ai.response.model", m.labels.allowedOrOther(m.labels.models, input.ActualModel)),
		attribute.String("currency", allowedOrOther(allowedCurrencies, input.Currency)),
		attribute.String("estimate.status", allowedOrOther(allowedEstimates, input.EstimateStatus)),
	}
}

func (m *Metrics) evalAttributes(input EvalMetric) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("evaluator", allowedOrOther(allowedEvaluators, input.Evaluator)),
		attribute.String("status", allowedOrOther(allowedEvalStatus, input.Status)),
		attribute.String("metric.name", m.labels.allowedOrOther(m.labels.metricNames, input.MetricName)),
	}
}

func scoreProjectionAttributes(input ScoreProjectionMetric) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("backend", allowedOrOther(allowedBackends, input.Backend)),
		attribute.String("status", allowedOrOther(allowedProjectionStatuses, input.Status)),
	}
}

func metricLabelSet(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func (p metricLabelPolicy) allowedOrOther(allowed map[string]struct{}, value string) string {
	return allowedOrOther(allowed, value)
}

func allowedOrOther(allowed map[string]struct{}, value string) string {
	if _, exists := allowed[value]; exists {
		return value
	}
	return metricOtherLabelValue
}

func isFiniteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func statusClass(statusCode int) string {
	switch {
	case statusCode >= 100 && statusCode < 200:
		return "1xx"
	case statusCode >= 200 && statusCode < 300:
		return "2xx"
	case statusCode >= 300 && statusCode < 400:
		return "3xx"
	case statusCode >= 400 && statusCode < 500:
		return "4xx"
	case statusCode >= 500 && statusCode < 600:
		return "5xx"
	default:
		return "unknown"
	}
}
