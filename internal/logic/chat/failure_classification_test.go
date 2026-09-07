package chat

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	llmtestutil "github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
)

// T116 模型与观测失败域分离契约测试（RED 先行，T126 在 chat.go 落地
// ClassifyChatModelFailure / ClassifyChatObservabilityFailure 使其 GREEN）。
//
// 覆盖的生产风险（FR-007 + US3 验收场景 3）：exporter/评分投递失败被归类成
// 模型 5xx、或模型 429/超时被归类成观测错误——两种方向都会把用户可见的
// 业务语义和旁路诊断语义搅在一起。契约要求：
//
// 1. 模型失败（429/5xx/timeout/canceled/响应契约违约）映射到稳定业务错误类，
//    且错误链只包含 controller 已经认识的低敏 sentinel（llm.ErrRateLimit、
//    llm.ErrUpstream、context.DeadlineExceeded、context.Canceled、
//    ErrChatProviderFailure、ErrChatInvalidResponse），原始外部错误绝不透传；
// 2. 观测失败（telemetry/score/exporter/evidence 旁路）只归类为稳定观测
//    错误类（telemetry_export_failed / side_effect_failed），不携带任何
//    业务 sentinel，因此不可能被投影为 5xx；
// 3. 两个类集合严格不相交：模型分类器永不出观测类，观测分类器永不出
//    业务类；Execute 失败时 ai_trace_id 仍保留在结果身份中。

func TestClassifyChatModelFailureMaps429ToRateLimitedBusinessClass(t *testing.T) {
	class, err := ClassifyChatModelFailure(llm.ErrRateLimit)
	if class != ChatFailureClassRateLimited {
		t.Errorf("class = %q, want %q", class, ChatFailureClassRateLimited)
	}
	if err == nil || !errors.Is(err, llm.ErrRateLimit) || !errors.Is(err, llm.ErrUpstream) {
		t.Errorf("error chain = %v, want errors.Is(llm.ErrRateLimit) && errors.Is(llm.ErrUpstream)（controller 需要 429 投影依据）", err)
	}
}

func TestClassifyChatModelFailureMaps5xxToUnavailableBusinessClass(t *testing.T) {
	class, err := ClassifyChatModelFailure(llm.ErrUpstream)
	if class != ChatFailureClassUpstreamUnavailable {
		t.Errorf("class = %q, want %q", class, ChatFailureClassUpstreamUnavailable)
	}
	if err == nil || !errors.Is(err, llm.ErrUpstream) || errors.Is(err, llm.ErrRateLimit) {
		t.Errorf("error chain = %v, want 仅 llm.ErrUpstream（5xx 不得携带 rate limit 语义）", err)
	}
}

func TestClassifyChatModelFailureMapsTimeoutToTimeoutBusinessClass(t *testing.T) {
	class, err := ClassifyChatModelFailure(context.DeadlineExceeded)
	if class != ChatFailureClassUpstreamTimeout {
		t.Errorf("class = %q, want %q", class, ChatFailureClassUpstreamTimeout)
	}
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error chain = %v, want errors.Is(context.DeadlineExceeded)（controller 需要 504 投影依据）", err)
	}
}

// 取消是调用方行为，不是 provider 5xx：不得归类为上游故障，也不得携带
// llm.ErrUpstream（否则 controller 会返回 502 而不是正确的取消语义）。
func TestClassifyChatModelFailureKeepsCancellationCallerSided(t *testing.T) {
	class, err := ClassifyChatModelFailure(context.Canceled)
	if class != ChatFailureClassCallerCanceled {
		t.Errorf("class = %q, want %q", class, ChatFailureClassCallerCanceled)
	}
	if err == nil || !errors.Is(err, context.Canceled) || errors.Is(err, llm.ErrUpstream) {
		t.Errorf("error chain = %v, want 仅 context.Canceled", err)
	}
}

// 包装过的 sentinel 必须仍被正确分类：adapter 链路上的 %w 包装不得改变分类。
func TestClassifyChatModelFailureClassifiesWrappedSentinels(t *testing.T) {
	wrappedRateLimit := fmt.Errorf("provider gateway: %w", llm.ErrRateLimit)
	wrappedDeadline := fmt.Errorf("round trip: %w", context.DeadlineExceeded)

	class, err := ClassifyChatModelFailure(wrappedRateLimit)
	if class != ChatFailureClassRateLimited || err == nil || !errors.Is(err, llm.ErrRateLimit) {
		t.Errorf("wrapped rate limit: class=%q err=%v, want rate_limited + errors.Is(llm.ErrRateLimit)", class, err)
	}
	class, err = ClassifyChatModelFailure(wrappedDeadline)
	if class != ChatFailureClassUpstreamTimeout || err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wrapped deadline: class=%q err=%v, want upstream_timeout + errors.Is(DeadlineExceeded)", class, err)
	}
}

// 未知外部错误：归类为 provider_failure 且原始错误绝不透传——provider body、
// endpoint、credential 可能藏在原始 error 里，错误链只能含低敏 sentinel。
func TestClassifyChatModelFailureSentinelsUnknownErrorsWithoutLeaking(t *testing.T) {
	raw := errors.New("provider returned forbidden body authorization=Bearer-forbidden-t116")
	class, err := ClassifyChatModelFailure(raw)
	if class != ChatFailureClassProviderFailure {
		t.Errorf("class = %q, want %q", class, ChatFailureClassProviderFailure)
	}
	if err == nil || !errors.Is(err, ErrChatProviderFailure) {
		t.Errorf("error chain = %v, want ErrChatProviderFailure", err)
	}
	if errors.Is(err, raw) {
		t.Error("原始外部错误进入了 sentinel 链：敏感 provider 细节可能泄露")
	}
}

func TestClassifyChatModelFailureNilErrorIsNoFailure(t *testing.T) {
	class, err := ClassifyChatModelFailure(nil)
	if class != ChatFailureClassNone || err != nil {
		t.Errorf("nil error: class=%q err=%v, want none/nil", class, err)
	}
}

// 观测旁路失败分类：generation 遥测入队失败是投递类错误，其余旁路
// （score projection、evaluator、evidence store、bridge、manifest、
// exporter）是副作用类错误；未知组件安全落到 side_effect_failed。
func TestClassifyChatObservabilityFailureSeparatesDeliveryFromSideEffects(t *testing.T) {
	tests := []struct {
		component string
		want      ChatObservabilityFailureClass
	}{
		{chatTelemetryComponent, ChatObservabilityClassTelemetryExport},
		{chatScoreProjectionComponent, ChatObservabilityClassSideEffect},
		{chatEvaluatorComponent, ChatObservabilityClassSideEffect},
		{chatEvaluatorSpanComponent, ChatObservabilityClassSideEffect},
		{chatEvidenceStoreComponent, ChatObservabilityClassSideEffect},
		{chatBridgeComponent, ChatObservabilityClassSideEffect},
		{"run_manifest", ChatObservabilityClassSideEffect},
		{"collector_exporter", ChatObservabilityClassSideEffect},
		{"unknown_component", ChatObservabilityClassSideEffect},
	}
	for _, tc := range tests {
		t.Run(tc.component, func(t *testing.T) {
			if got := ClassifyChatObservabilityFailure(tc.component); got != tc.want {
				t.Errorf("component %q: class = %q, want %q", tc.component, got, tc.want)
			}
		})
	}
}

// 领域隔离不变量：业务错误类与观测错误类严格不相交，且两个分类器都不会
// 产出对方的类——观测故障不可能被投影为模型业务失败（T126 门控：
// 禁止把 exporter failure 返回为 5xx）。
func TestFailureDomainsNeverCrossClassify(t *testing.T) {
	businessClasses := []ChatFailureClass{
		ChatFailureClassNone,
		ChatFailureClassRateLimited,
		ChatFailureClassUpstreamUnavailable,
		ChatFailureClassUpstreamTimeout,
		ChatFailureClassCallerCanceled,
		ChatFailureClassInvalidResponse,
		ChatFailureClassProviderFailure,
	}
	observabilityClasses := []ChatObservabilityFailureClass{
		ChatObservabilityClassTelemetryExport,
		ChatObservabilityClassSideEffect,
	}
	for _, business := range businessClasses {
		if slices.Contains(observabilityClasses, ChatObservabilityFailureClass(business)) {
			t.Errorf("业务类 %q 与观测类集合重叠", business)
		}
	}

	modelFailures := []error{
		llm.ErrRateLimit,
		llm.ErrUpstream,
		context.DeadlineExceeded,
		context.Canceled,
		errors.New("unknown"),
	}
	for _, failure := range modelFailures {
		class, _ := ClassifyChatModelFailure(failure)
		if slices.Contains(observabilityClasses, ChatObservabilityFailureClass(class)) {
			t.Errorf("模型失败 %v 被归类为观测错误类 %q：两个域不得互相归类", failure, class)
		}
	}

	for _, component := range []string{chatTelemetryComponent, chatScoreProjectionComponent, "collector_exporter", "unknown"} {
		class := ClassifyChatObservabilityFailure(component)
		if slices.Contains(businessClasses, ChatFailureClass(class)) {
			t.Errorf("观测失败组件 %q 被归类为业务错误类 %q", component, class)
		}
	}
}

// 观测失败事实（ChatTelemetryFailure）只携带组件与稳定类，不携带 error——
// 因此它永远不可能携带 llm.ErrUpstream 之类的业务 sentinel。
func TestChatTelemetryFailureCarriesNoBusinessSentinel(t *testing.T) {
	failure := ChatTelemetryFailure{
		Component:  chatScoreProjectionComponent,
		ErrorClass: string(ClassifyChatObservabilityFailure(chatScoreProjectionComponent)),
	}
	if failure.ErrorClass != string(ChatObservabilityClassSideEffect) {
		t.Errorf("ErrorClass = %q, want side_effect_failed", failure.ErrorClass)
	}
}

// Execute 层事实：provider 429 失败时业务错误携带 rate_limited 类，
// ai_trace_id 仍保留在结果身份中（US3 验收场景 3：失败按业务语义呈现）。
func TestExecuteRateLimitFailureKeepsAITraceIdentityAndBusinessClass(t *testing.T) {
	telemetry := &recordingTelemetry{}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
			return nil, llm.ErrRateLimit
		}},
		RequestedModel:          "server-model",
		NewAITraceID:            func() string { return "ai-t116-rate-limit" },
		CanonicalizeActualModel: allowActualModels("provider-model-v1"),
		Telemetry:               telemetry,
	})

	result, err := usecase.Execute(context.Background(), ChatCommand{Message: "rate limit boundary"})
	if err == nil {
		t.Fatal("Execute() error = nil, want 业务失败")
	}
	class, _ := ClassifyChatModelFailure(err)
	if class != ChatFailureClassRateLimited {
		t.Errorf("class = %q, want %q", class, ChatFailureClassRateLimited)
	}
	if !errors.Is(err, llm.ErrRateLimit) {
		t.Errorf("error = %v, want errors.Is(llm.ErrRateLimit)", err)
	}
	if result.Identity.AITraceID != "ai-t116-rate-limit" {
		t.Errorf("AITraceID = %q, want 失败时保留 ai_trace_id", result.Identity.AITraceID)
	}
	if len(telemetry.traces) != 1 {
		t.Fatalf("failure traces = %d, want 1", len(telemetry.traces))
	}
	if telemetry.traces[0].OutcomeStatus != "rate_limit" {
		t.Errorf("outcome = %q, want rate_limit（业务失败仍写入观测事实）", telemetry.traces[0].OutcomeStatus)
	}
}

// Execute 层事实：观测旁路失败（telemetry 入队失败）绝不改变业务结果——
// Execute 仍返回 nil error，失败只落入诊断端口。
func TestExecuteTelemetryBypassFailureNeverBecomesBusinessError(t *testing.T) {
	telemetry := &failingTelemetry{err: errors.New("bounded queue full")}
	diagnostics := &recordingTelemetryDiagnostics{}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{
				Content:      "business result",
				Model:        "provider-model-v1",
				FinishReason: llm.FinishStop,
				Usage:        llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}),
			}, nil
		}},
		RequestedModel:          "server-model",
		NewAITraceID:            func() string { return "ai-t116-bypass" },
		CanonicalizeActualModel: allowActualModels("provider-model-v1"),
		Telemetry:               telemetry,
		Diagnostics:             diagnostics,
	})

	result, err := usecase.Execute(context.Background(), ChatCommand{Message: "telemetry bypass boundary"})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil：观测旁路失败不得改写业务结果", err)
	}
	if result.Content != "business result" {
		t.Errorf("content = %q, want 业务结果保持不变", result.Content)
	}
	if len(diagnostics.failures) != 1 {
		t.Fatalf("diagnostic failures = %d, want 1", len(diagnostics.failures))
	}
	recorded := diagnostics.failures[0]
	if recorded.Component != chatTelemetryComponent || recorded.ErrorClass != chatTelemetryFailureClass {
		t.Errorf("recorded = %q/%q, want generation/telemetry_export_failed", recorded.Component, recorded.ErrorClass)
	}
	class := ClassifyChatObservabilityFailure(recorded.Component)
	if slices.Contains([]ChatFailureClass{
		ChatFailureClassRateLimited, ChatFailureClassUpstreamUnavailable,
		ChatFailureClassUpstreamTimeout, ChatFailureClassProviderFailure,
	}, ChatFailureClass(class)) {
		t.Errorf("观测失败被归类为业务类 %q：禁止把 exporter/telemetry failure 返回为 5xx", class)
	}
}
