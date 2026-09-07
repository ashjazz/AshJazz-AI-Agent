package observability

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	"github.com/ashjazz/Longtermism/pkg/ai/resilience"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricsRecordRequiredInstrumentsWithOnlyLowCardinalityAttributes(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	metrics, err := NewMetrics(provider.Meter("github.com/ashjazz/Longtermism/internal/observability"), WithMetricLabelPolicy(testMetricLabelPolicy()))
	if err != nil {
		t.Fatal("NewMetrics() returned an unexpected error")
	}

	ctx := context.Background()
	// 每种输入都带上故意的高基数身份和原始内容；指标端口只可消费合同列出的
	// 低基数维度，防止一次 chat 或 smoke 造成新的 Prometheus time series。
	if err := metrics.RecordHTTP(ctx, HTTPMetric{RouteTemplate: "/api/v1/chat", RawRoute: "/api/v1/chat?message=synthetic-private-prompt", Method: "POST", StatusCode: 502, Duration: 120 * time.Millisecond, RequestID: "req-t017", TraceID: "trace-t017", SpanID: "span-t017", SmokeRunID: "smoke-t017"}); err != nil {
		t.Fatal("RecordHTTP() returned an unexpected error")
	}
	if err := metrics.RecordLLM(ctx, LLMMetric{Provider: "openai-compatible", RequestedModel: "gpt-test", ActualModel: "gpt-test-actual", Outcome: "failed", Duration: 800 * time.Millisecond, UsageAvailability: llm.UsageReported, InputTokens: 10, OutputTokens: 5, CostAvailability: resilience.ProviderAttemptCostEstimated, Cost: 0.01, Currency: "USD", EstimateStatus: "estimated", AITraceID: "ai-t017", SessionID: "session-t017", TraceID: "trace-t017", SpanID: "span-t017", PromptHash: "sha256:synthetic"}); err != nil {
		t.Fatal("RecordLLM() returned an unexpected error")
	}
	if err := metrics.RecordEval(ctx, EvalMetric{Evaluator: "deterministic", Status: "passed", MetricName: "answer_quality", Score: 0.9, RequestID: "req-t017", AITraceID: "ai-t017", TraceID: "trace-t017", SpanID: "span-t017", PromptHash: "sha256:synthetic"}); err != nil {
		t.Fatal("RecordEval() returned an unexpected error")
	}
	if err := metrics.RecordScoreProjection(ctx, ScoreProjectionMetric{Backend: "langfuse", Status: "sent", RequestID: "req-t017", AITraceID: "ai-t017", TraceID: "trace-t017", SpanID: "span-t017", SmokeRunID: "smoke-t017"}); err != nil {
		t.Fatal("RecordScoreProjection() returned an unexpected error")
	}
	if err := metrics.RecordScoreQueue(ctx, ScoreQueueMetric{Backend: "langfuse", Depth: 3, RequestID: "req-t017", TraceID: "trace-t017", SpanID: "span-t017", SessionID: "session-t017"}); err != nil {
		t.Fatal("RecordScoreQueue() returned an unexpected error")
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &collected); err != nil {
		t.Fatal("ManualReader.Collect() returned an unexpected error")
	}

	requestAttributes := metricAttributes("http.route", "/api/v1/chat", "http.request.method", "POST", "http.response.status_class", "5xx")
	llmRequestAttributes := metricAttributes("gen_ai.provider.name", "openai-compatible", "gen_ai.request.model", "gpt-test", "outcome", "failed")
	evalAttributes := metricAttributes("evaluator", "deterministic", "status", "passed", "metric.name", "answer_quality")
	want := map[string]metricExpectation{
		"longtermism.http.server.request.count":    {kind: metricKindInt64Counter, unit: "", expectedAttributeSets: []map[string]string{requestAttributes}},
		"longtermism.http.server.request.duration": {kind: metricKindHistogram, unit: "s", expectedAttributeSets: []map[string]string{requestAttributes}},
		"longtermism.llm.request.count":            {kind: metricKindInt64Counter, unit: "", expectedAttributeSets: []map[string]string{llmRequestAttributes}},
		"longtermism.llm.duration":                 {kind: metricKindHistogram, unit: "s", expectedAttributeSets: []map[string]string{llmRequestAttributes}},
		"longtermism.llm.tokens": {kind: metricKindInt64Counter, unit: "{token}", expectedAttributeSets: []map[string]string{
			metricAttributes("gen_ai.provider.name", "openai-compatible", "gen_ai.response.model", "gpt-test-actual", "gen_ai.token.type", "input"),
			metricAttributes("gen_ai.provider.name", "openai-compatible", "gen_ai.response.model", "gpt-test-actual", "gen_ai.token.type", "output"),
		}},
		"longtermism.llm.cost":           {kind: metricKindFloat64Counter, unit: "", expectedAttributeSets: []map[string]string{metricAttributes("gen_ai.provider.name", "openai-compatible", "gen_ai.response.model", "gpt-test-actual", "currency", "USD", "estimate.status", "estimated")}},
		"longtermism.eval.result":        {kind: metricKindInt64Counter, unit: "", expectedAttributeSets: []map[string]string{evalAttributes}},
		"longtermism.eval.score":         {kind: metricKindHistogram, unit: "", expectedAttributeSets: []map[string]string{evalAttributes}},
		"longtermism.score.projection":   {kind: metricKindInt64Counter, unit: "", expectedAttributeSets: []map[string]string{metricAttributes("backend", "langfuse", "status", "sent")}},
		"longtermism.score.worker.queue": {kind: metricKindGauge, unit: "", expectedAttributeSets: []map[string]string{metricAttributes("backend", "langfuse")}},
	}

	seen := make(map[string]struct{}, len(want))
	for _, scopeMetrics := range collected.ScopeMetrics {
		for _, collectedMetric := range scopeMetrics.Metrics {
			expectation, required := want[collectedMetric.Name]
			if !required {
				t.Fatal("scoped meter emitted an unknown metric instrument")
			}
			seen[collectedMetric.Name] = struct{}{}
			// OTel unit 会参与 Prometheus 名称归一化。这里逐项锁定，防止代码仍能记录、
			// 但 dashboard 因 `_seconds` / `_token` 等后缀漂移而静默查不到数据。
			if collectedMetric.Unit != expectation.unit {
				t.Fatalf("metric %q unit = %q, want %q", collectedMetric.Name, collectedMetric.Unit, expectation.unit)
			}
			assertMetricAggregationKind(t, collectedMetric.Data, expectation.kind)
			assertMetricDataPointAttributes(t, collectedMetric.Data, expectation.expectedAttributeSets)
		}
	}
	if len(seen) != len(want) {
		t.Fatal("required first-wave metric instruments were not all collected")
	}
}

func TestMetricsCoarsensUnknownLabels(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	metrics, err := NewMetrics(provider.Meter("t029-label-policy"), WithMetricLabelPolicy(testMetricLabelPolicy()))
	if err != nil {
		t.Fatal("NewMetrics() returned an unexpected error")
	}

	// 模拟错误边界把原始路由、模型别名和敏感文本交给指标端口。它们必须被压缩为
	// 有界 other，而不是作为标签值进入 Prometheus 时序。
	for _, record := range []func() error{
		func() error {
			return metrics.RecordHTTP(context.Background(), HTTPMetric{RouteTemplate: "/api/v1/chat?token=synthetic-private", Method: "TRACE", StatusCode: 200})
		},
		func() error {
			return metrics.RecordLLM(context.Background(), LLMMetric{Provider: "Bearer synthetic-private", RequestedModel: "user-input-synthetic-private", ActualModel: "provider-error-synthetic-private", Outcome: "provider-error-synthetic-private", UsageAvailability: llm.UsageUnavailable, CostAvailability: resilience.ProviderAttemptCostUnavailable})
		},
		func() error {
			return metrics.RecordEval(context.Background(), EvalMetric{Evaluator: "user-synthetic-private", Status: "user-synthetic-private", MetricName: "user-synthetic-private"})
		},
		func() error {
			return metrics.RecordScoreProjection(context.Background(), ScoreProjectionMetric{Backend: "user-synthetic-private", Status: "user-synthetic-private"})
		},
		func() error {
			return metrics.RecordScoreQueue(context.Background(), ScoreQueueMetric{Backend: "user-synthetic-private"})
		},
	} {
		if err := record(); err != nil {
			t.Fatalf("recording unknown labels returned error = %v", err)
		}
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal("ManualReader.Collect() returned an unexpected error")
	}

	otherCount := 0
	for _, scopeMetrics := range collected.ScopeMetrics {
		for _, collectedMetric := range scopeMetrics.Metrics {
			for _, attributes := range metricAttributeSets(collectedMetric.Data) {
				for _, value := range attributes {
					if strings.Contains(value, "synthetic-private") || strings.Contains(value, "Bearer") {
						t.Fatalf("metric label leaked an unbounded or sensitive input: %q", value)
					}
					if value == metricOtherLabelValue {
						otherCount++
					}
				}
			}
		}
	}
	if otherCount == 0 {
		t.Fatal("unknown labels were not coarsened to other")
	}
}

func TestMetricsPreservesExplicitNotConfiguredScoreStatus(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	metrics, err := NewMetrics(provider.Meter("t101-not-configured"))
	if err != nil {
		t.Fatalf("NewMetrics() error = %v", err)
	}
	if err := metrics.RecordScoreProjection(context.Background(), ScoreProjectionMetric{
		Backend: "langfuse", Status: "not_configured",
	}); err != nil {
		t.Fatalf("RecordScoreProjection() error = %v", err)
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	want := metricAttributes("backend", "langfuse", "status", "not_configured")
	for _, scopeMetrics := range collected.ScopeMetrics {
		for _, collectedMetric := range scopeMetrics.Metrics {
			if collectedMetric.Name == metricScoreProjection {
				assertMetricDataPointAttributes(t, collectedMetric.Data, []map[string]string{want})
				return
			}
		}
	}
	t.Fatal("score projection metric was not collected")
}

func TestMetricsRejectsInvalidMeasurements(t *testing.T) {
	tests := []struct {
		name   string
		record func(*Metrics) error
	}{
		{name: "negative HTTP duration", record: func(metrics *Metrics) error {
			return metrics.RecordHTTP(context.Background(), HTTPMetric{Duration: -time.Second})
		}},
		{name: "negative LLM tokens", record: func(metrics *Metrics) error {
			return metrics.RecordLLM(context.Background(), LLMMetric{UsageAvailability: llm.UsageReported, InputTokens: -1, CostAvailability: resilience.ProviderAttemptCostUnavailable})
		}},
		{name: "negative LLM cost", record: func(metrics *Metrics) error {
			return metrics.RecordLLM(context.Background(), LLMMetric{UsageAvailability: llm.UsageUnavailable, CostAvailability: resilience.ProviderAttemptCostActual, Cost: -0.1})
		}},
		{name: "NaN LLM cost", record: func(metrics *Metrics) error {
			return metrics.RecordLLM(context.Background(), LLMMetric{UsageAvailability: llm.UsageUnavailable, CostAvailability: resilience.ProviderAttemptCostActual, Cost: math.NaN()})
		}},
		{name: "infinite LLM cost", record: func(metrics *Metrics) error {
			return metrics.RecordLLM(context.Background(), LLMMetric{UsageAvailability: llm.UsageUnavailable, CostAvailability: resilience.ProviderAttemptCostEstimated, Cost: math.Inf(1)})
		}},
		{name: "negative eval score", record: func(metrics *Metrics) error { return metrics.RecordEval(context.Background(), EvalMetric{Score: -0.1}) }},
		{name: "infinite eval score", record: func(metrics *Metrics) error {
			return metrics.RecordEval(context.Background(), EvalMetric{Score: math.Inf(1)})
		}},
		{name: "negative queue depth", record: func(metrics *Metrics) error {
			return metrics.RecordScoreQueue(context.Background(), ScoreQueueMetric{Depth: -1})
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := metric.NewManualReader()
			provider := metric.NewMeterProvider(metric.WithReader(reader))
			metrics, err := NewMetrics(provider.Meter("t029-invalid-measurement"), WithMetricLabelPolicy(testMetricLabelPolicy()))
			if err != nil {
				t.Fatal("NewMetrics() returned an unexpected error")
			}

			if err := test.record(metrics); !errors.Is(err, ErrInvalidMetricValue) {
				t.Fatalf("record() error = %v, want ErrInvalidMetricValue", err)
			}

			var collected metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &collected); err != nil {
				t.Fatal("ManualReader.Collect() returned an unexpected error")
			}
			for _, scopeMetrics := range collected.ScopeMetrics {
				for _, collectedMetric := range scopeMetrics.Metrics {
					if got := len(metricAttributeSets(collectedMetric.Data)); got != 0 {
						t.Fatalf("invalid input emitted %d partial metric data points", got)
					}
				}
			}
		})
	}
}

func TestProviderAttemptMetricsObserverRecordsEachTerminalAttemptExactlyOnce(t *testing.T) {
	const attemptDuration = 250 * time.Millisecond
	tests := []struct {
		name     string
		response *llm.ChatResponse
		err      error
		wantErr  error
		outcome  string
	}{
		{name: "succeeded", response: &llm.ChatResponse{Model: "gpt-test-actual", Usage: llm.NewUnavailableProviderUsage()}, outcome: "succeeded"},
		{name: "failed", err: errors.New("synthetic provider failure"), wantErr: resilience.ErrProviderRejected, outcome: "failed"},
		{name: "timeout", err: context.DeadlineExceeded, wantErr: context.DeadlineExceeded, outcome: "timeout"},
		{name: "cancelled", err: context.Canceled, wantErr: context.Canceled, outcome: "cancelled"},
		{name: "invalid response", err: llm.ErrInvalidResponse, wantErr: llm.ErrInvalidResponse, outcome: "invalid_response"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metrics, reader := newLLMMetricTestRecorder(t, "t208-attempt-outcome")
			observer := NewProviderAttemptMetricsObserver(metrics)
			// 生产 OpenAI-compatible adapter 的稳定 Name() 是 openai；这里使用真实名称，
			// 防止测试 fake 掩盖 allowlist 将生产指标错误压缩为 other。
			provider := &llmMetricAttemptProvider{name: "openai", response: test.response, err: test.err}
			wrapper := resilience.NewProviderWrapper(
				provider,
				resilience.NewCircuitBreaker(resilience.Config{FailureThreshold: 10, RecoveryTimeout: time.Minute}),
				resilience.WithProviderAttemptObserver(observer),
				resilience.WithNow(steppingLLMMetricClock(attemptDuration)),
			)

			// T217 负责冻结 attempt；本测试从公开 wrapper 入口验证 adapter 不会把一次
			// 网络尝试重复投影为 request/duration 时序。
			response, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "gpt-test"})
			if test.wantErr == nil {
				if err != nil || response != test.response {
					t.Fatalf("Chat() response/error = %p/%v, want %p/nil", response, err, test.response)
				}
			} else if !errors.Is(err, test.wantErr) {
				t.Fatalf("Chat() error = %v, want errors.Is(%v)", err, test.wantErr)
			}
			collected := collectMetrics(t, reader)

			wantAttributes := metricAttributes(
				"gen_ai.provider.name", "openai",
				"gen_ai.request.model", "gpt-test",
				"outcome", test.outcome,
			)
			requestMetric := mustCollectedMetric(t, collected, metricLLMRequestCount)
			assertSingleInt64SumValue(t, requestMetric.Data, 1)
			assertMetricDataPointAttributes(t, requestMetric.Data, []map[string]string{wantAttributes})

			durationMetric := mustCollectedMetric(t, collected, metricLLMDuration)
			assertSingleHistogramMeasurement(t, durationMetric.Data, attemptDuration.Seconds())
			assertMetricDataPointAttributes(t, durationMetric.Data, []map[string]string{wantAttributes})

			assertMetricAbsentOrEmpty(t, collected, metricLLMTokens)
			assertMetricAbsentOrEmpty(t, collected, metricLLMCost)
		})
	}
}

func TestProviderAttemptMetricsObserverRecordsTokensOnlyWhenUsageReported(t *testing.T) {
	tests := []struct {
		name       string
		usage      llm.ProviderUsage
		wantTokens map[string]int64
	}{
		{name: "reported nonzero", usage: mustReportedProviderUsage(t, 7, 3), wantTokens: map[string]int64{"input": 7, "output": 3}},
		{name: "reported zero", usage: mustReportedProviderUsage(t, 0, 0), wantTokens: map[string]int64{"input": 0, "output": 0}},
		{name: "unavailable", usage: llm.NewUnavailableProviderUsage()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metrics, reader := newLLMMetricTestRecorder(t, "t208-attempt-usage")
			observer := NewProviderAttemptMetricsObserver(metrics)
			provider := &llmMetricAttemptProvider{
				name: "openai-compatible",
				response: &llm.ChatResponse{
					Model: "gpt-test-actual",
					Usage: test.usage,
				},
			}
			wrapper := resilience.NewProviderWrapper(
				provider,
				resilience.NewCircuitBreaker(resilience.Config{FailureThreshold: 10, RecoveryTimeout: time.Minute}),
				resilience.WithProviderAttemptObserver(observer),
			)

			response, err := wrapper.Chat(context.Background(), &llm.ChatRequest{Model: "gpt-test"})
			if err != nil || response != provider.response {
				t.Fatalf("Chat() response/error = %p/%v, want %p/nil", response, err, provider.response)
			}
			collected := collectMetrics(t, reader)
			if test.wantTokens == nil {
				// “没有 usage”与“provider 明确报告零 token”是不同事实。前者不能通过
				// Add(0) 制造一个看似可信的零值 datapoint。
				assertMetricAbsentOrEmpty(t, collected, metricLLMTokens)
			} else {
				tokenMetric := mustCollectedMetric(t, collected, metricLLMTokens)
				if got := int64SumValuesByAttribute(t, tokenMetric.Data, "gen_ai.token.type"); !reflect.DeepEqual(got, test.wantTokens) {
					t.Fatalf("token values = %#v, want %#v", got, test.wantTokens)
				}
				assertMetricDataPointAttributes(t, tokenMetric.Data, []map[string]string{
					metricAttributes("gen_ai.provider.name", "openai-compatible", "gen_ai.response.model", "gpt-test-actual", "gen_ai.token.type", "input"),
					metricAttributes("gen_ai.provider.name", "openai-compatible", "gen_ai.response.model", "gpt-test-actual", "gen_ai.token.type", "output"),
				})
			}
			assertMetricAbsentOrEmpty(t, collected, metricLLMCost)
		})
	}
}

func TestMetricsRecordLLMRecordsCostOnlyWhenAvailable(t *testing.T) {
	tests := []struct {
		name           string
		availability   resilience.ProviderAttemptCostAvailability
		amount         float64
		currency       string
		estimateStatus string
		wantDataPoint  bool
		wantCurrency   string
		wantEstimate   string
	}{
		{name: "actual zero is a reported fact", availability: resilience.ProviderAttemptCostActual, amount: 0, currency: "USD", estimateStatus: "actual", wantDataPoint: true, wantCurrency: "USD", wantEstimate: "actual"},
		{name: "estimated nonzero", availability: resilience.ProviderAttemptCostEstimated, amount: 0.125, currency: "USD", estimateStatus: "estimated", wantDataPoint: true, wantCurrency: "USD", wantEstimate: "estimated"},
		{name: "unavailable omits datapoint", availability: resilience.ProviderAttemptCostUnavailable, amount: 0, currency: "USD", estimateStatus: "unavailable"},
		{name: "available unknown labels coarsen", availability: resilience.ProviderAttemptCostActual, amount: 0.25, currency: "synthetic-private-currency", estimateStatus: "synthetic-private-estimate", wantDataPoint: true, wantCurrency: metricOtherLabelValue, wantEstimate: metricOtherLabelValue},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metrics, reader := newLLMMetricTestRecorder(t, "t208-cost-availability")
			err := metrics.RecordLLM(context.Background(), LLMMetric{
				Provider:          "openai-compatible",
				RequestedModel:    "gpt-test",
				ActualModel:       "gpt-test-actual",
				Outcome:           "succeeded",
				UsageAvailability: llm.UsageUnavailable,
				CostAvailability:  test.availability,
				Cost:              test.amount,
				Currency:          test.currency,
				EstimateStatus:    test.estimateStatus,
			})
			if err != nil {
				t.Fatalf("RecordLLM() error = %v", err)
			}
			collected := collectMetrics(t, reader)
			assertMetricAbsentOrEmpty(t, collected, metricLLMTokens)
			if !test.wantDataPoint {
				assertMetricAbsentOrEmpty(t, collected, metricLLMCost)
				return
			}

			costMetric := mustCollectedMetric(t, collected, metricLLMCost)
			assertSingleFloat64SumValue(t, costMetric.Data, test.amount)
			assertMetricDataPointAttributes(t, costMetric.Data, []map[string]string{
				metricAttributes(
					"gen_ai.provider.name", "openai-compatible",
					"gen_ai.response.model", "gpt-test-actual",
					"currency", test.wantCurrency,
					"estimate.status", test.wantEstimate,
				),
			})
		})
	}
}

func TestMetricsRecordLLMCoarsensEveryUnboundedProviderAttemptLabel(t *testing.T) {
	metrics, reader := newLLMMetricTestRecorder(t, "t208-bounded-labels")
	err := metrics.RecordLLM(context.Background(), LLMMetric{
		Provider:          "synthetic-private-provider",
		RequestedModel:    "synthetic-private-requested-model",
		ActualModel:       "synthetic-private-actual-model",
		Outcome:           "synthetic-private-outcome",
		UsageAvailability: llm.UsageReported,
		InputTokens:       2,
		OutputTokens:      1,
		CostAvailability:  resilience.ProviderAttemptCostActual,
		Cost:              0.25,
		Currency:          "synthetic-private-currency",
		EstimateStatus:    "synthetic-private-estimate",
		AITraceID:         "ai-t208",
		SessionID:         "session-t208",
		TraceID:           "trace-t208",
		SpanID:            "span-t208",
		PromptHash:        "prompt-t208",
	})
	if err != nil {
		t.Fatalf("RecordLLM() error = %v", err)
	}
	collected := collectMetrics(t, reader)

	requestAttributes := metricAttributes("gen_ai.provider.name", metricOtherLabelValue, "gen_ai.request.model", metricOtherLabelValue, "outcome", metricOtherLabelValue)
	assertMetricDataPointAttributes(t, mustCollectedMetric(t, collected, metricLLMRequestCount).Data, []map[string]string{requestAttributes})
	assertMetricDataPointAttributes(t, mustCollectedMetric(t, collected, metricLLMDuration).Data, []map[string]string{requestAttributes})
	assertMetricDataPointAttributes(t, mustCollectedMetric(t, collected, metricLLMTokens).Data, []map[string]string{
		metricAttributes("gen_ai.provider.name", metricOtherLabelValue, "gen_ai.response.model", metricOtherLabelValue, "gen_ai.token.type", "input"),
		metricAttributes("gen_ai.provider.name", metricOtherLabelValue, "gen_ai.response.model", metricOtherLabelValue, "gen_ai.token.type", "output"),
	})
	assertMetricDataPointAttributes(t, mustCollectedMetric(t, collected, metricLLMCost).Data, []map[string]string{
		metricAttributes("gen_ai.provider.name", metricOtherLabelValue, "gen_ai.response.model", metricOtherLabelValue, "currency", metricOtherLabelValue, "estimate.status", metricOtherLabelValue),
	})
}

func testMetricLabelPolicy() MetricLabelPolicy {
	return MetricLabelPolicy{
		AllowedRoutes:      []string{"/api/v1/chat"},
		AllowedModels:      []string{"gpt-test", "gpt-test-actual"},
		AllowedMetricNames: []string{"answer_quality"},
	}
}

func TestStatusClass(t *testing.T) {
	// 状态码只被投影为有限集合，避免把完整状态或任意输入变成新的指标序列。
	tests := []struct {
		name       string
		statusCode int
		want       string
	}{
		{name: "informational", statusCode: 100, want: "1xx"},
		{name: "success", statusCode: 200, want: "2xx"},
		{name: "redirect", statusCode: 302, want: "3xx"},
		{name: "client error", statusCode: 404, want: "4xx"},
		{name: "server error", statusCode: 502, want: "5xx"},
		{name: "out of HTTP range", statusCode: 0, want: "unknown"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := statusClass(test.statusCode); got != test.want {
				t.Fatalf("statusClass(%d) = %q, want %q", test.statusCode, got, test.want)
			}
		})
	}
}

type metricKind string

const (
	metricKindInt64Counter   metricKind = "int64_counter"
	metricKindFloat64Counter metricKind = "float64_counter"
	metricKindHistogram      metricKind = "float64_histogram"
	metricKindGauge          metricKind = "int64_gauge"
)

type metricExpectation struct {
	kind                  metricKind
	unit                  string
	expectedAttributeSets []map[string]string
}

func assertMetricAggregationKind(t *testing.T, aggregation metricdata.Aggregation, want metricKind) {
	t.Helper()
	switch want {
	case metricKindInt64Counter:
		counter, ok := aggregation.(metricdata.Sum[int64])
		if !ok || !counter.IsMonotonic {
			t.Fatal("metric did not use the expected int64 monotonic counter aggregation")
		}
	case metricKindFloat64Counter:
		// 成本必须保留小数金额，若误换成 Int64Counter 会截断生产事实。
		counter, ok := aggregation.(metricdata.Sum[float64])
		if !ok || !counter.IsMonotonic {
			t.Fatal("metric did not use the expected float64 monotonic counter aggregation")
		}
	case metricKindHistogram:
		if _, ok := aggregation.(metricdata.Histogram[float64]); !ok {
			t.Fatal("metric did not use the expected histogram aggregation")
		}
	case metricKindGauge:
		if _, ok := aggregation.(metricdata.Gauge[int64]); !ok {
			t.Fatal("metric did not use the expected gauge aggregation")
		}
	}
}

func assertMetricDataPointAttributes(t *testing.T, aggregation metricdata.Aggregation, expectedSets []map[string]string) {
	t.Helper()
	sets := metricAttributeSets(aggregation)
	if len(sets) == 0 {
		t.Fatal("metric contained no data points")
	}
	for _, attributes := range sets {
		if !matchesExpectedMetricAttributeSet(attributes, expectedSets) {
			t.Fatal("metric attribute values did not match the low-cardinality contract")
		}
		for key := range attributes {
			normalizedKey := strings.NewReplacer(".", "_", "-", "_").Replace(strings.ToLower(key))
			for _, forbiddenKey := range []string{"request_id", "trace_id", "span_id", "ai_trace_id", "service_trace_id", "session_id", "user_id", "raw_route", "eval_run_id", "run_id", "smoke_run_id", "prompt_id", "prompt_hash"} {
				if normalizedKey == forbiddenKey {
					t.Fatalf("metric attributes included forbidden high-cardinality key %q", key)
				}
			}
		}
		for _, forbiddenFragment := range []string{"req-t017", "trace-t017", "span-t017", "ai-t017", "session-t017", "smoke-t017", "sha256:synthetic", "synthetic-private-prompt", "ai-t208", "session-t208", "trace-t208", "span-t208", "prompt-t208"} {
			for _, value := range attributes {
				if strings.Contains(value, forbiddenFragment) {
					t.Fatal("metric attribute value contained a forbidden high-cardinality or sensitive marker")
				}
			}
		}
	}
	for _, expected := range expectedSets {
		if !matchesExpectedMetricAttributeSet(expected, sets) {
			t.Fatal("metric omitted an expected low-cardinality attribute set")
		}
	}
}

func matchesExpectedMetricAttributeSet(got map[string]string, wantSets []map[string]string) bool {
	for _, want := range wantSets {
		if reflect.DeepEqual(got, want) {
			return true
		}
	}
	return false
}

func metricAttributes(keyValuePairs ...string) map[string]string {
	attributes := make(map[string]string, len(keyValuePairs)/2)
	for index := 0; index < len(keyValuePairs); index += 2 {
		attributes[keyValuePairs[index]] = keyValuePairs[index+1]
	}
	return attributes
}

func metricAttributeSets(aggregation metricdata.Aggregation) []map[string]string {
	var sets []map[string]string
	appendSet := func(slice []metricdata.DataPoint[int64]) {
		for _, point := range slice {
			sets = append(sets, metricAttributeSet(point.Attributes.ToSlice()))
		}
	}
	switch data := aggregation.(type) {
	case metricdata.Sum[int64]:
		appendSet(data.DataPoints)
	case metricdata.Sum[float64]:
		for _, point := range data.DataPoints {
			sets = append(sets, metricAttributeSet(point.Attributes.ToSlice()))
		}
	case metricdata.Gauge[int64]:
		appendSet(data.DataPoints)
	case metricdata.Histogram[float64]:
		for _, point := range data.DataPoints {
			sets = append(sets, metricAttributeSet(point.Attributes.ToSlice()))
		}
	}
	return sets
}

func metricAttributeSet(attributes []attribute.KeyValue) map[string]string {
	result := make(map[string]string, len(attributes))
	for _, attribute := range attributes {
		result[string(attribute.Key)] = attribute.Value.AsString()
	}
	return result
}

func newLLMMetricTestRecorder(t *testing.T, scope string) (*Metrics, *metric.ManualReader) {
	t.Helper()
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("MeterProvider.Shutdown() error = %v", err)
		}
	})
	metrics, err := NewMetrics(provider.Meter(scope), WithMetricLabelPolicy(testMetricLabelPolicy()))
	if err != nil {
		t.Fatalf("NewMetrics() error = %v", err)
	}
	return metrics, reader
}

func collectMetrics(t *testing.T, reader *metric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("ManualReader.Collect() error = %v", err)
	}
	return collected
}

func collectedMetric(collected metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, scopeMetrics := range collected.ScopeMetrics {
		for _, candidate := range scopeMetrics.Metrics {
			if candidate.Name == name {
				return candidate, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func mustCollectedMetric(t *testing.T, collected metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	result, ok := collectedMetric(collected, name)
	if !ok {
		t.Fatalf("metric %q was not collected", name)
	}
	return result
}

func assertMetricAbsentOrEmpty(t *testing.T, collected metricdata.ResourceMetrics, name string) {
	t.Helper()
	got, ok := collectedMetric(collected, name)
	if ok && len(metricAttributeSets(got.Data)) != 0 {
		t.Fatalf("unavailable fact emitted metric %q datapoints", name)
	}
}

func assertSingleInt64SumValue(t *testing.T, aggregation metricdata.Aggregation, want int64) {
	t.Helper()
	sum, ok := aggregation.(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatal("metric did not contain exactly one int64 sum datapoint")
	}
	if got := sum.DataPoints[0].Value; got != want {
		t.Fatalf("int64 sum value = %d, want %d", got, want)
	}
}

func assertSingleFloat64SumValue(t *testing.T, aggregation metricdata.Aggregation, want float64) {
	t.Helper()
	sum, ok := aggregation.(metricdata.Sum[float64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatal("metric did not contain exactly one float64 sum datapoint")
	}
	if got := sum.DataPoints[0].Value; got != want {
		t.Fatalf("float64 sum value = %v, want %v", got, want)
	}
}

func assertSingleHistogramMeasurement(t *testing.T, aggregation metricdata.Aggregation, wantSum float64) {
	t.Helper()
	histogram, ok := aggregation.(metricdata.Histogram[float64])
	if !ok || len(histogram.DataPoints) != 1 {
		t.Fatal("metric did not contain exactly one histogram datapoint")
	}
	if got := histogram.DataPoints[0].Count; got != 1 {
		t.Fatalf("histogram count = %d, want 1", got)
	}
	if got := histogram.DataPoints[0].Sum; got != wantSum {
		t.Fatalf("histogram sum = %v, want attempt duration %v seconds", got, wantSum)
	}
}

func int64SumValuesByAttribute(t *testing.T, aggregation metricdata.Aggregation, key string) map[string]int64 {
	t.Helper()
	sum, ok := aggregation.(metricdata.Sum[int64])
	if !ok {
		t.Fatal("metric did not use int64 sum aggregation")
	}
	result := make(map[string]int64, len(sum.DataPoints))
	for _, point := range sum.DataPoints {
		value, exists := point.Attributes.Value(attribute.Key(key))
		if !exists {
			t.Fatalf("metric datapoint omitted attribute %q", key)
		}
		result[value.AsString()] = point.Value
	}
	return result
}

func mustReportedProviderUsage(t *testing.T, inputTokens, outputTokens int) llm.ProviderUsage {
	t.Helper()
	usage, err := llm.NewReportedProviderUsage(llm.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	})
	if err != nil {
		t.Fatalf("NewReportedProviderUsage() error = %v", err)
	}
	return usage
}

func steppingLLMMetricClock(step time.Duration) func() time.Time {
	var mu sync.Mutex
	next := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		current := next
		next = next.Add(step)
		return current
	}
}

type llmMetricAttemptProvider struct {
	name     string
	response *llm.ChatResponse
	err      error
}

func (p *llmMetricAttemptProvider) Name() string { return p.name }

func (*llmMetricAttemptProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{}
}

func (p *llmMetricAttemptProvider) Chat(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
	return p.response, p.err
}

func (*llmMetricAttemptProvider) ChatStream(context.Context, *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	return nil, errors.New("llm metric attempt provider does not support streaming")
}
