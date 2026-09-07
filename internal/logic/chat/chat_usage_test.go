package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	appobs "github.com/ashjazz/Longtermism/internal/observability"
	aieval "github.com/ashjazz/Longtermism/pkg/ai/eval"
	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	llmtestutil "github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
)

// T206：直接注入 provider 端口，不能依靠 OpenAI adapter 替 usecase 守住事实边界。
// 零值/显式 unavailable 已被 T215 防御；adapter 返回 ErrInvalidResponse 的路径
// 仍须归一为同一应用错误。测试保留真实 RED，修复属于 T216。
func TestChatUsecaseUsageRejectsUnreportedOrInvalidProviderFacts(t *testing.T) {
	tests := []struct {
		name     string
		response *llm.ChatResponse
		err      error
	}{
		{name: "zero_value_usage", response: t206Response(llm.ProviderUsage{})},
		{name: "explicit_unavailable_usage", response: t206Response(llm.NewUnavailableProviderUsage())},
		{name: "provider_invalid_response", err: llm.ErrInvalidResponse},
		{name: "wrapped_provider_invalid_response", err: fmt.Errorf("%s: %w", t206RawError, llm.ErrInvalidResponse)},
		{
			// response+error 的返回形态也不能把貌似合法的 token 事实带入成功链路。
			name:     "response_with_invalid_response_error",
			response: t206Response(llmtestutil.MustReportedUsage(t206NonzeroUsage())),
			err:      fmt.Errorf("%s: %w", t206RawError, llm.ErrInvalidResponse),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newT206UsageFixture(t, tt.response, tt.err)
			result, err := fixture.execute(t)
			// 不在分类断言处 Fatal：即使当前分类为 RED，也实际运行全部副作用断言。
			// 返回稳定 sentinel 本身，不允许隐藏文案但经 Unwrap 保留原始错误链。
			if err != ErrChatInvalidResponse {
				t.Error("provider usage rejection must map to ErrChatInvalidResponse without an external error chain")
			}
			class, safeErr := ClassifyChatModelFailure(err)
			if class != ChatFailureClassInvalidResponse || safeErr != ErrChatInvalidResponse {
				t.Errorf("business error class = %q, want invalid_response", class)
			}
			// controller 的成功 DTO 投影不能获得任何 provider/eval 事实；身份必须保留。
			if result != (ChatResult{Identity: fixture.identity}) {
				t.Error("rejected response must return identity only, without success/eval facts")
			}
			wantEvents := []string{"identity", "bridge", "provider", "generation_observation", "bridge_end"}
			if !reflect.DeepEqual(fixture.events, wantEvents) {
				t.Errorf("failure events = %v, want %v", fixture.events, wantEvents)
			}
			if len(fixture.evaluator.inputs) != 0 || len(fixture.store.evidence) != 0 || fixture.projection.input != (ChatScoreProjectionInput{}) {
				t.Error("invalid usage must not reach evaluator, evidence store or score projection")
			}
			generation := fixture.generation.input
			if generation.Outcome != "failed" || generation.FailureStatus != string(obs.FailureUpstream) || generation.Identity != fixture.identity {
				t.Error("generation must record upstream failure with the existing identity")
			}
			if generation.Usage != (llm.Usage{}) || generation.ActualModel != "" || generation.FinishReason != "" || generation.TTFT != nil {
				t.Error("failed generation must not project rejected completion facts")
			}
			trace := fixture.onlyTrace(t)
			// 领域 Trace 的业务失败写在 OutcomeStatus；FailureStatus 专供观测自身失败。
			if trace.OutcomeStatus != string(obs.FailureUpstream) || trace.FailureStatus != "" || trace.Model != "server-model" {
				t.Error("failure trace must retain only upstream failure and the configured model")
			}
			// 检查可序列化事实，防止缺失 usage 被导出为“真实零 token/cost/eval”。
			encoded, marshalErr := json.Marshal(trace)
			if marshalErr != nil {
				t.Fatal("failed to serialize failure trace")
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(encoded, &fields) != nil {
				t.Fatal("failed to inspect failure trace fields")
			}
			for _, key := range []string{"inputTokens", "outputTokens", "reasoningTokens", "cacheReadTokens", "cacheWriteTokens", "costUsd", "autoEvalScore"} {
				if _, present := fields[key]; present {
					t.Errorf("failure trace fabricated %s", key)
				}
			}
			assertT206NoRawFacts(t, err, result, generation, trace, fixture.diagnostics.failures)
		})
	}
}

// 同一套完整装配是失败断言的正向对照：不允许通过漏装 evaluator/store 取得假阴性。
// reported zero 是明确事实，不是 unavailable 的兜底；非零用量含 reasoning/cache 细项。
func TestChatUsecaseUsagePreservesReportedZeroAndNonzeroFacts(t *testing.T) {
	for _, tt := range []struct {
		name  string
		usage llm.Usage
	}{
		{name: "reported_zero", usage: llm.Usage{}},
		{name: "reported_nonzero", usage: t206NonzeroUsage()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := t206Response(llmtestutil.MustReportedUsage(tt.usage))
			fixture := newT206UsageFixture(t, response, nil)
			result, err := fixture.execute(t)
			if err != nil {
				t.Fatal("explicitly reported valid usage must succeed")
			}
			if result.Identity != fixture.identity || result.Content != response.Content || result.Model != response.Model || result.FinishReason != response.FinishReason || result.Usage != tt.usage {
				t.Error("successful result changed identity or provider-reported completion facts")
			}
			wantEvents := []string{"identity", "bridge", "provider", "generation_observation", "eval_identity", "evaluator", "evidence_store", "evaluator_observation", "projection", "bridge_end"}
			if !reflect.DeepEqual(fixture.events, wantEvents) {
				t.Errorf("success events = %v, want %v", fixture.events, wantEvents)
			}
			generation := fixture.generation.input
			if generation.Outcome != "success" || generation.FailureStatus != "" || generation.Identity != fixture.identity || generation.Usage != tt.usage || generation.ActualModel != response.Model || generation.FinishReason != response.FinishReason {
				t.Error("generation changed reported usage or completion identity")
			}
			if len(fixture.evaluator.inputs) != 1 || len(fixture.store.evidence) != 1 {
				t.Fatal("reported usage must reach the real evaluator and evidence store once")
			}
			wantEvalIdentity := obs.ApplyCorrelationOptions(fixture.identity, obs.WithEvalRunID("eval-t206-usage"))
			if fixture.evaluator.inputs[0] != (CompletionContractEvaluationInput{
				Identity: wantEvalIdentity, ActualModel: response.Model, FinishReason: response.FinishReason,
				Usage: tt.usage, OutputPresent: true,
			}) {
				t.Error("evaluator did not receive the exact validated usage and evaluation identity")
			}
			evidence := fixture.store.evidence[0]
			if evidence.RequestID != wantEvalIdentity.RequestID || evidence.AITraceID != wantEvalIdentity.AITraceID || evidence.ServiceTraceID != wantEvalIdentity.ServiceTraceID || evidence.SpanID != wantEvalIdentity.SpanID || evidence.EvalRunID != wantEvalIdentity.EvalRunID || evidence.Score != 1 {
				t.Error("reported usage did not produce correlated completion-contract evidence")
			}
			if !reflect.DeepEqual(fixture.evalObserver.input.Evidence, evidence) || !reflect.DeepEqual(fixture.projection.input.Evidence, evidence) || fixture.projection.input.Generation != fixture.generation.identity {
				t.Error("persisted evidence or generation identity changed during projection")
			}
			if result.EvalSummary == nil || result.EvalSummary.Status != EvalStatusPassed || result.EvalSummary.Score == nil || *result.EvalSummary.Score != 1 {
				t.Error("reported usage must retain the successful completion-contract summary")
			}
			trace := fixture.onlyTrace(t)
			if trace.OutcomeStatus != "success" || trace.FailureStatus != "" || trace.Model != response.Model || trace.InputTokens != tt.usage.InputTokens || trace.OutputTokens != tt.usage.OutputTokens || trace.ReasoningTokens != tt.usage.ReasoningTokens || trace.CacheReadTokens != tt.usage.CacheReadTokens || trace.CacheWriteTokens != tt.usage.CacheWriteTokens || trace.CostUSD != 0 {
				t.Error("telemetry changed reported usage or fabricated unavailable cost")
			}
			if len(fixture.diagnostics.failures) != 0 {
				t.Error("positive control must have all evidence side channels correctly configured")
			}
			// 业务 content 可交给 controller，但不得进入低敏 generation/evidence/telemetry。
			assertT206NoRawFacts(t, generation, fixture.evaluator.inputs, evidence, fixture.projection.input, trace)
		})
	}
}

const (
	t206RawInput  = "synthetic-user-input-t206"
	t206RawOutput = "synthetic-provider-output-t206"
	t206RawError  = "synthetic-provider-body-t206 https://provider.invalid/private Authorization: Bearer synthetic-t206"
)

func t206NonzeroUsage() llm.Usage {
	return llm.Usage{InputTokens: 11, OutputTokens: 17, ReasoningTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 2, TotalTokens: 28}
}

func t206Response(usage llm.ProviderUsage) *llm.ChatResponse {
	return &llm.ChatResponse{Content: t206RawOutput, Model: "provider-model", FinishReason: llm.FinishStop, Usage: usage}
}

type t206UsageFixture struct {
	usecase      *ChatUsecase
	identity     obs.CorrelationIdentity
	events       []string
	generation   *recordingGenerationObserver
	evaluator    *t206UsageEvaluator
	evalObserver *recordingEvaluatorObserver
	store        *t206EvidenceStore
	projection   *recordingProjectionQueue
	telemetry    *recordingTelemetry
	diagnostics  *recordingTelemetryDiagnostics
}

func newT206UsageFixture(t *testing.T, response *llm.ChatResponse, providerErr error) *t206UsageFixture {
	t.Helper()
	f := &t206UsageFixture{
		identity:  obs.NewCorrelationIdentity("req-t206-usage", obs.WithServiceSpan(t090ServiceTraceID, t090BridgeSpanID), obs.WithAITraceID("ai-t206-usage")),
		telemetry: &recordingTelemetry{}, diagnostics: &recordingTelemetryDiagnostics{},
	}
	f.generation = &recordingGenerationObserver{events: &f.events, identity: appobs.PlatformSpanIdentity{TraceID: t090PlatformTraceID, SpanID: t090GenerationSpanID, Projectable: true}}
	f.evaluator = &t206UsageEvaluator{events: &f.events, delegate: newT090Evaluator(t)}
	f.evalObserver = &recordingEvaluatorObserver{events: &f.events}
	f.store = &t206EvidenceStore{events: &f.events}
	f.projection = &recordingProjectionQueue{events: &f.events}
	f.usecase = NewChatUsecase(ChatUsecaseDependencies{
		Provider: &scriptedProvider{chat: func(ctx context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
			f.events = append(f.events, "provider")
			if identity, ok := obs.CorrelationIdentityFromContext(ctx); !ok || identity != f.identity {
				t.Fatal("provider must observe the current request/AI identity before it can fail")
			}
			return response, providerErr
		}},
		RequestedModel: "server-model", ProviderName: "scripted", PromptTemplateVersion: "chat-v1",
		PromptHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PayloadMode: obs.PayloadModeMetadataOnly,
		NewAITraceID:            func() string { f.events = append(f.events, "identity"); return f.identity.AITraceID },
		NewEvalRunID:            func() string { f.events = append(f.events, "eval_identity"); return "eval-t206-usage" },
		CanonicalizeActualModel: allowActualModels("provider-model"),
		Bridge:                  &recordingChatBridge{events: &f.events}, GenerationObserver: f.generation,
		Evaluator: f.evaluator, EvaluatorObserver: f.evalObserver, EvidenceStore: f.store,
		ProjectionQueue: f.projection, Telemetry: f.telemetry, Diagnostics: f.diagnostics, Now: monotonicChatClock(),
	})
	return f
}

func (f *t206UsageFixture) execute(t *testing.T) (ChatResult, error) {
	t.Helper()
	// 入站旧 AI/eval 身份必须清除；已生成的本次 AI 身份随后不得被失败或投影覆盖。
	inbound := obs.ApplyCorrelationOptions(f.identity, obs.WithAITraceID("stale-ai"), obs.WithEvalRunID("stale-eval"))
	ctx := obs.ContextWithCorrelationIdentity(context.Background(), inbound)
	result, err := f.usecase.Execute(ctx, ChatCommand{Message: t206RawInput})
	if after, _ := obs.CorrelationIdentityFromContext(ctx); after != inbound {
		t.Error("execution mutated the caller-owned identity")
	}
	return result, err
}

func (f *t206UsageFixture) onlyTrace(t *testing.T) obs.Trace {
	t.Helper()
	if len(f.telemetry.traces) != 1 {
		t.Fatal("one provider execution must record exactly one generation trace")
	}
	trace := f.telemetry.traces[0]
	if trace.RequestID != f.identity.RequestID || trace.TraceID != f.identity.AITraceID || trace.ServiceTraceID != f.identity.ServiceTraceID || trace.SpanID != f.identity.SpanID || trace.ObservationType != obs.ObservationTypeGeneration {
		t.Error("telemetry changed request, AI or service identity")
	}
	return trace
}

type t206UsageEvaluator struct {
	events   *[]string
	inputs   []CompletionContractEvaluationInput
	delegate Evaluator[CompletionContractEvaluationInput, CompletionContractEvaluationResult]
}

func (e *t206UsageEvaluator) Evaluate(ctx context.Context, input CompletionContractEvaluationInput) (CompletionContractEvaluationResult, error) {
	*e.events = append(*e.events, "evaluator")
	e.inputs = append(e.inputs, input)
	return e.delegate.Evaluate(ctx, input)
}

type t206EvidenceStore struct {
	events   *[]string
	evidence []aieval.EvaluationEvidence
}

func (s *t206EvidenceStore) Append(_ context.Context, evidence aieval.EvaluationEvidence) error {
	*s.events = append(*s.events, "evidence_store")
	s.evidence = append(s.evidence, cloneEvaluationEvidence(evidence))
	return nil
}

func assertT206NoRawFacts(t *testing.T, values ...any) {
	t.Helper()
	text := fmt.Sprintf("%+v", values)
	for _, forbidden := range []string{t206RawInput, t206RawOutput, "synthetic-provider-body-t206", "provider.invalid", "Authorization", "synthetic-t206"} {
		if strings.Contains(text, forbidden) {
			t.Error("raw input/output, endpoint or credential canary escaped into low-sensitivity facts")
		}
	}
}
