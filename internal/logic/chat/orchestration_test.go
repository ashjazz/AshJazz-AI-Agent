package chat

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	appobs "github.com/ashjazz/Longtermism/internal/observability"
	aieval "github.com/ashjazz/Longtermism/pkg/ai/eval"
	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	llmtestutil "github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
)

const (
	t090ServiceTraceID   = "11111111111111111111111111111111"
	t090BridgeSpanID     = "1111111111111111"
	t090PlatformTraceID  = t090ServiceTraceID
	t090ForeignTraceID   = "22222222222222222222222222222222"
	t090GenerationSpanID = "2222222222222222"
	t090EvaluatorSpanID  = "3333333333333333"
)

func TestChatUsecaseOrchestratesEvidencePipelineInOrderWithoutMutatingFacts(t *testing.T) {
	events := make([]string, 0, 9)
	threshold := 0.8
	evaluator, err := NewCompletionContractEvaluator(CompletionContractEvaluatorConfig{
		Dataset:    aieval.DatasetIdentity{Name: "chat_contract", Version: "v1"},
		SampleID:   "completion_shape",
		MetricName: "completion_contract",
		Threshold:  &threshold,
	})
	if err != nil {
		t.Fatalf("NewCompletionContractEvaluator() error = %v", err)
	}
	trackedEvaluator := &recordingCompletionEvaluator{
		events:     &events,
		delegate:   evaluator,
		wantEvalID: "eval-t090-order",
	}
	store := &mutatingEvidenceStore{events: &events}
	projection := &recordingProjectionQueue{events: &events}
	response := &llm.ChatResponse{
		Content:      "evidence pipeline completed",
		Model:        "provider-actual-model",
		FinishReason: llm.FinishStop,
		Usage:        llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 4, OutputTokens: 3, TotalTokens: 7}),
	}
	responseBefore := *response
	generationObserver := &recordingGenerationObserver{
		events:   &events,
		identity: appobs.PlatformSpanIdentity{TraceID: t090PlatformTraceID, SpanID: t090GenerationSpanID, Projectable: true},
	}
	evaluatorObserver := &recordingEvaluatorObserver{events: &events}

	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider: &scriptedProvider{chat: func(ctx context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
			events = append(events, "provider")
			identity, ok := obs.CorrelationIdentityFromContext(ctx)
			if !ok || identity.AITraceID != "ai-t090-order" || identity.EvalRunID != "" {
				t.Fatalf("provider identity = %#v present=%t", identity, ok)
			}
			return response, nil
		}},
		RequestedModel:          "server-configured-model",
		ProviderName:            "openai-compatible",
		PromptTemplateVersion:   "chat-v1",
		PromptHash:              "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PayloadMode:             obs.PayloadModeMetadataOnly,
		NewAITraceID:            func() string { events = append(events, "identity"); return "ai-t090-order" },
		NewEvalRunID:            func() string { events = append(events, "eval_identity"); return "eval-t090-order" },
		CanonicalizeActualModel: allowActualModels("provider-actual-model"),
		Bridge: &recordingChatBridge{
			events: &events,
			identity: obs.NewCorrelationIdentity(
				"req-t090-order",
				obs.WithServiceSpan(t090ServiceTraceID, t090BridgeSpanID),
				obs.WithAITraceID("ai-t090-order"),
			),
		},
		GenerationObserver: generationObserver,
		Evaluator:          trackedEvaluator,
		EvaluatorObserver:  evaluatorObserver,
		EvidenceStore:      store,
		ProjectionQueue:    projection,
		Now:                monotonicChatClock(),
	})
	inbound := obs.NewCorrelationIdentity(
		"req-t090-order",
		obs.WithServiceSpan("forged-inbound-trace", "forged-inbound-span"),
		obs.WithAITraceID("stale-ai"),
		obs.WithEvalRunID("stale-eval"),
	)
	command := ChatCommand{Message: "raw message must stay outside evaluation facts", SmokeRunID: "run-t177-orchestration"}
	commandBefore := command

	trustedContext := appobs.ContextWithChatSmokeRunID(obs.ContextWithCorrelationIdentity(context.Background(), inbound), command.SmokeRunID)
	result, executeErr := usecase.Execute(
		trustedContext,
		command,
	)
	if executeErr != nil {
		t.Fatalf("Execute() error = %v", executeErr)
	}
	if projection.input.RunID != command.SmokeRunID {
		t.Fatalf("projection run ID = %q, want trusted smoke run %q", projection.input.RunID, command.SmokeRunID)
	}
	wantEvents := []string{
		"identity",
		"bridge",
		"provider",
		"generation_observation",
		"eval_identity",
		"evaluator",
		"evidence_store",
		"evaluator_observation",
		"projection",
		"bridge_end",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("orchestration events = %#v, want %#v", events, wantEvents)
	}
	responseUsage, reported := response.Usage.Summary()
	if !reported {
		t.Fatal("provider fixture must carry explicitly reported usage")
	}
	if result.Content != response.Content || result.Model != response.Model || result.Usage != responseUsage {
		t.Fatalf("business result = %#v, want provider facts", result)
	}
	if result.Identity.EvalRunID != "" ||
		result.Identity.ServiceTraceID != t090ServiceTraceID ||
		result.Identity.SpanID != t090BridgeSpanID {
		t.Fatalf("result identity = %#v, want public bridge identity without internal eval identity", result.Identity)
	}
	if result.EvalSummary == nil ||
		result.EvalSummary.Evaluator != completionContractEvaluatorName ||
		result.EvalSummary.Score == nil ||
		*result.EvalSummary.Score != 1 {
		t.Fatalf("result eval summary = %#v", result.EvalSummary)
	}
	if projection.input.Generation != (appobs.PlatformSpanIdentity{TraceID: t090PlatformTraceID, SpanID: t090GenerationSpanID, Projectable: true}) {
		t.Fatalf("projection target = %#v, want real generation platform identity", projection.input.Generation)
	}
	if generationObserver.input.SmokeRunID != command.SmokeRunID || evaluatorObserver.input.SmokeRunID != command.SmokeRunID {
		t.Fatalf("semantic observer markers = generation:%q evaluator:%q, want %q", generationObserver.input.SmokeRunID, evaluatorObserver.input.SmokeRunID, command.SmokeRunID)
	}
	if projection.input.Evidence.Threshold == nil || *projection.input.Evidence.Threshold != threshold {
		t.Fatalf("projection evidence threshold = %#v, want defensive copy", projection.input.Evidence.Threshold)
	}
	if trackedEvaluator.result.Evidence == nil ||
		trackedEvaluator.result.Evidence.Threshold == nil ||
		*trackedEvaluator.result.Evidence.Threshold != threshold {
		t.Fatal("evidence store mutation escaped back into evaluator result")
	}
	if command != commandBefore || !reflect.DeepEqual(*response, responseBefore) || inbound.EvalRunID != "stale-eval" {
		t.Fatal("orchestration mutated caller-owned command, provider response, or inbound identity")
	}
}

func TestChatUsecaseKeepsProviderResultWhenEvidenceSideChannelsFail(t *testing.T) {
	sideEffectErr := errors.New("provider body api-key=forbidden-t090-side-effect")
	tests := []struct {
		name           string
		bridge         ChatAIExecutionBoundary
		generation     ChatGenerationObserver
		evaluator      Evaluator[CompletionContractEvaluationInput, CompletionContractEvaluationResult]
		evalObserver   ChatEvaluatorObserver
		store          ChatEvidenceStore
		projection     ChatScoreProjectionQueue
		wantComponents []string
		wantStoreCalls int
		wantQueueCalls int
	}{
		{
			name:           "bridge failure",
			bridge:         &failingChatBridge{err: sideEffectErr},
			evaluator:      newT090Evaluator(t),
			store:          &countingEvidenceStore{},
			projection:     &countingProjectionQueue{},
			wantComponents: []string{chatBridgeComponent},
		},
		{
			name: "bridge rejects structurally invalid native identity",
			bridge: &recordingChatBridge{identity: obs.NewCorrelationIdentity(
				"req-t090-side-effect",
				obs.WithServiceSpan("not-a-trace-id", "not-a-span-id"),
				obs.WithAITraceID("ai-t090-side-effect"),
			)},
			evaluator:      newT090Evaluator(t),
			store:          &countingEvidenceStore{},
			projection:     &countingProjectionQueue{},
			wantComponents: []string{chatBridgeComponent},
		},
		{
			name:           "evaluator failure",
			bridge:         &recordingChatBridge{},
			evaluator:      failingCompletionEvaluator{err: sideEffectErr},
			store:          &countingEvidenceStore{},
			projection:     &countingProjectionQueue{},
			wantComponents: []string{chatEvaluatorComponent},
		},
		{
			name:           "evaluator evidence cannot switch correlation identity",
			bridge:         &recordingChatBridge{},
			evaluator:      mismatchedEvidenceEvaluator{delegate: newT090Evaluator(t)},
			store:          &countingEvidenceStore{},
			projection:     &countingProjectionQueue{},
			wantComponents: []string{chatEvaluatorComponent},
		},
		{
			name:           "generation observation failure preserves local evidence but blocks projection",
			bridge:         &recordingChatBridge{},
			generation:     &recordingGenerationObserver{err: sideEffectErr},
			evaluator:      newT090Evaluator(t),
			store:          &countingEvidenceStore{},
			projection:     &countingProjectionQueue{},
			wantComponents: []string{chatGenerationComponent, chatScoreProjectionComponent},
			wantStoreCalls: 1,
		},
		{
			name:       "generation rejects structurally invalid projection identity",
			bridge:     &recordingChatBridge{},
			generation: &recordingGenerationObserver{identity: appobs.PlatformSpanIdentity{TraceID: "not-a-trace-id", SpanID: "not-a-span-id"}},
			evaluator:  newT090Evaluator(t),
			store:      &countingEvidenceStore{},
			projection: &countingProjectionQueue{},
			wantComponents: []string{
				chatGenerationComponent,
				chatScoreProjectionComponent,
			},
			wantStoreCalls: 1,
		},
		{
			name:       "head-sampled generation keeps evidence but is not projected",
			bridge:     &recordingChatBridge{},
			generation: &recordingGenerationObserver{identity: appobs.PlatformSpanIdentity{TraceID: t090PlatformTraceID, SpanID: t090GenerationSpanID}},
			evaluator:  newT090Evaluator(t),
			store:      &countingEvidenceStore{},
			projection: &countingProjectionQueue{},
			wantComponents: []string{
				chatScoreProjectionComponent,
			},
			wantStoreCalls: 1,
		},
		{
			name:   "foreign generation trace keeps local evidence but is not projected",
			bridge: &recordingChatBridge{},
			generation: &recordingGenerationObserver{identity: appobs.PlatformSpanIdentity{
				TraceID:     t090ForeignTraceID,
				SpanID:      t090GenerationSpanID,
				Projectable: true,
			}},
			evaluator:  newT090Evaluator(t),
			store:      &countingEvidenceStore{},
			projection: &countingProjectionQueue{},
			wantComponents: []string{
				chatScoreProjectionComponent,
			},
			wantStoreCalls: 1,
		},
		{
			name:           "evidence store failure blocks projection",
			bridge:         &recordingChatBridge{},
			evaluator:      newT090Evaluator(t),
			store:          &countingEvidenceStore{err: sideEffectErr},
			projection:     &countingProjectionQueue{},
			wantComponents: []string{chatEvidenceStoreComponent},
			wantStoreCalls: 1,
		},
		{
			name:           "evaluator observation failure keeps evidence and projection",
			bridge:         &recordingChatBridge{},
			evaluator:      newT090Evaluator(t),
			evalObserver:   &recordingEvaluatorObserver{err: sideEffectErr},
			store:          &countingEvidenceStore{},
			projection:     &countingProjectionQueue{},
			wantComponents: []string{chatEvaluatorSpanComponent},
			wantStoreCalls: 1,
			wantQueueCalls: 1,
		},
		{
			name:           "projection failure",
			bridge:         &recordingChatBridge{},
			evaluator:      newT090Evaluator(t),
			store:          &countingEvidenceStore{},
			projection:     &countingProjectionQueue{err: sideEffectErr},
			wantComponents: []string{chatScoreProjectionComponent},
			wantStoreCalls: 1,
			wantQueueCalls: 1,
		},
		{
			name:           "bridge end failure",
			bridge:         &recordingChatBridge{endErr: sideEffectErr},
			evaluator:      newT090Evaluator(t),
			store:          &countingEvidenceStore{},
			projection:     &countingProjectionQueue{},
			wantComponents: []string{chatBridgeComponent},
			wantStoreCalls: 1,
			wantQueueCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := tt.store.(*countingEvidenceStore)
			queue := tt.projection.(*countingProjectionQueue)
			generation := tt.generation
			if generation == nil {
				generation = &recordingGenerationObserver{
					identity: appobs.PlatformSpanIdentity{TraceID: t090PlatformTraceID, SpanID: t090GenerationSpanID, Projectable: true},
				}
			}
			evalObserver := tt.evalObserver
			if evalObserver == nil {
				evalObserver = &recordingEvaluatorObserver{}
			}
			diagnostics := &recordingTelemetryDiagnostics{}
			usecase := NewChatUsecase(ChatUsecaseDependencies{
				Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
					return &llm.ChatResponse{
						Content:      "business result survives side-channel failures",
						Model:        "provider-actual-model",
						FinishReason: llm.FinishStop,
						Usage:        llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 2, OutputTokens: 4, TotalTokens: 6}),
					}, nil
				}},
				RequestedModel:          "server-configured-model",
				NewAITraceID:            func() string { return "ai-t090-side-effect" },
				NewEvalRunID:            func() string { return "eval-t090-side-effect" },
				CanonicalizeActualModel: allowActualModels("provider-actual-model"),
				Bridge:                  tt.bridge,
				GenerationObserver:      generation,
				Evaluator:               tt.evaluator,
				EvaluatorObserver:       evalObserver,
				EvidenceStore:           store,
				ProjectionQueue:         queue,
				Diagnostics:             diagnostics,
				Now:                     monotonicChatClock(),
			})
			ctx := obs.ContextWithCorrelationIdentity(
				context.Background(),
				obs.NewCorrelationIdentity(
					"req-t090-side-effect",
					obs.WithServiceSpan(t090ServiceTraceID, t090BridgeSpanID),
				),
			)

			result, err := usecase.Execute(ctx, ChatCommand{Message: "side-channel isolation"})
			if err != nil {
				t.Fatalf("Execute() error = %v, want side-channel failure isolated", err)
			}
			if result.Content != "business result survives side-channel failures" || result.Usage.TotalTokens != 6 {
				t.Fatalf("business result = %#v, want unchanged provider result", result)
			}
			if store.calls != tt.wantStoreCalls || queue.calls != tt.wantQueueCalls {
				t.Fatalf("side-channel calls = store:%d queue:%d, want store:%d queue:%d", store.calls, queue.calls, tt.wantStoreCalls, tt.wantQueueCalls)
			}
			gotComponents := make([]string, len(diagnostics.failures))
			for index, failure := range diagnostics.failures {
				gotComponents[index] = failure.Component
				if failure.ErrorClass != chatSideEffectFailureClass {
					t.Fatalf("diagnostic = %#v, want stable failure class", failure)
				}
			}
			if !reflect.DeepEqual(gotComponents, tt.wantComponents) {
				t.Fatalf("diagnostic components = %#v, want %#v", gotComponents, tt.wantComponents)
			}
		})
	}
}

func TestChatUsecaseDiagnosesMissingRequiredEvaluationPorts(t *testing.T) {
	newDependencies := func(diagnostics ChatTelemetryDiagnostics) ChatUsecaseDependencies {
		return ChatUsecaseDependencies{
			Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
				return &llm.ChatResponse{
					Content:      "missing ports remain diagnosable",
					Model:        "provider-actual-model",
					Usage:        llmtestutil.MustReportedUsage(llm.Usage{}),
					FinishReason: llm.FinishStop,
				}, nil
			}},
			RequestedModel:          "server-configured-model",
			NewAITraceID:            func() string { return "ai-t090-missing-ports" },
			NewEvalRunID:            func() string { return "eval-t090-missing-ports" },
			CanonicalizeActualModel: allowActualModels("provider-actual-model"),
			Bridge:                  &recordingChatBridge{},
			Evaluator:               newT090Evaluator(t),
			Diagnostics:             diagnostics,
			Now:                     monotonicChatClock(),
		}
	}
	ctx := obs.ContextWithCorrelationIdentity(
		context.Background(),
		obs.NewCorrelationIdentity(
			"req-t090-missing-ports",
			obs.WithServiceSpan(t090ServiceTraceID, t090BridgeSpanID),
		),
	)

	t.Run("missing generation evaluator observer and evidence store", func(t *testing.T) {
		diagnostics := &recordingTelemetryDiagnostics{}
		if result, err := NewChatUsecase(newDependencies(diagnostics)).Execute(ctx, ChatCommand{Message: "missing ports"}); err != nil || result.Content == "" {
			t.Fatalf("Execute() = result:%#v error:%v, want isolated port diagnostics", result, err)
		}
		want := []ChatTelemetryFailure{
			{Component: chatGenerationComponent, ErrorClass: chatSideEffectFailureClass},
			{Component: chatEvidenceStoreComponent, ErrorClass: chatSideEffectFailureClass},
		}
		if !reflect.DeepEqual(diagnostics.failures, want) {
			t.Fatalf("diagnostics = %#v, want %#v", diagnostics.failures, want)
		}
	})

	t.Run("missing evaluator observer and projection queue", func(t *testing.T) {
		diagnostics := &recordingTelemetryDiagnostics{}
		store := &countingEvidenceStore{}
		dependencies := newDependencies(diagnostics)
		dependencies.GenerationObserver = &recordingGenerationObserver{
			identity: appobs.PlatformSpanIdentity{
				TraceID:     t090PlatformTraceID,
				SpanID:      t090GenerationSpanID,
				Projectable: true,
			},
		}
		dependencies.EvidenceStore = store

		if result, err := NewChatUsecase(dependencies).Execute(ctx, ChatCommand{Message: "missing ports"}); err != nil || result.Content == "" {
			t.Fatalf("Execute() = result:%#v error:%v, want isolated port diagnostics", result, err)
		}
		want := []ChatTelemetryFailure{
			{Component: chatEvaluatorSpanComponent, ErrorClass: chatSideEffectFailureClass},
			{Component: chatScoreProjectionComponent, ErrorClass: chatSideEffectFailureClass},
		}
		if !reflect.DeepEqual(diagnostics.failures, want) || store.calls != 1 {
			t.Fatalf("diagnostics = %#v store calls = %d, want %#v and one persisted evidence", diagnostics.failures, store.calls, want)
		}
	})
}

func newT090Evaluator(t *testing.T) Evaluator[CompletionContractEvaluationInput, CompletionContractEvaluationResult] {
	t.Helper()
	threshold := 0.8
	evaluator, err := NewCompletionContractEvaluator(CompletionContractEvaluatorConfig{
		Dataset:    aieval.DatasetIdentity{Name: "chat_contract", Version: "v1"},
		SampleID:   "completion_shape",
		MetricName: "completion_contract",
		Threshold:  &threshold,
	})
	if err != nil {
		t.Fatalf("NewCompletionContractEvaluator() error = %v", err)
	}
	return evaluator
}

func monotonicChatClock() func() time.Time {
	current := time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC)
	return func() time.Time {
		result := current
		current = current.Add(time.Millisecond)
		return result
	}
}

type recordingChatBridge struct {
	events   *[]string
	identity obs.CorrelationIdentity
	endErr   error
}

func (bridge *recordingChatBridge) Start(ctx context.Context, identity obs.CorrelationIdentity) (context.Context, obs.CorrelationIdentity, appobs.EndChatAIExecution, error) {
	if bridge.events != nil {
		*bridge.events = append(*bridge.events, "bridge")
	}
	derived := identity
	if bridge.identity != (obs.CorrelationIdentity{}) {
		derived = bridge.identity
	}
	return obs.ContextWithCorrelationIdentity(ctx, derived), derived, func(appobs.ChatAIExecutionOutcome) error {
		if bridge.events != nil {
			*bridge.events = append(*bridge.events, "bridge_end")
		}
		return bridge.endErr
	}, nil
}

type failingChatBridge struct{ err error }

func (bridge *failingChatBridge) Start(ctx context.Context, identity obs.CorrelationIdentity) (context.Context, obs.CorrelationIdentity, appobs.EndChatAIExecution, error) {
	return ctx, identity, nil, bridge.err
}

type recordingGenerationObserver struct {
	events   *[]string
	identity appobs.PlatformSpanIdentity
	input    appobs.GenerationSpanInput
	err      error
}

func (observer *recordingGenerationObserver) RecordGeneration(_ context.Context, input appobs.GenerationSpanInput) (appobs.PlatformSpanIdentity, error) {
	observer.input = input
	if observer.events != nil {
		*observer.events = append(*observer.events, "generation_observation")
	}
	return observer.identity, observer.err
}

type recordingCompletionEvaluator struct {
	events     *[]string
	delegate   Evaluator[CompletionContractEvaluationInput, CompletionContractEvaluationResult]
	wantEvalID string
	result     CompletionContractEvaluationResult
}

func (evaluator *recordingCompletionEvaluator) Evaluate(ctx context.Context, input CompletionContractEvaluationInput) (CompletionContractEvaluationResult, error) {
	*evaluator.events = append(*evaluator.events, "evaluator")
	if input.Identity.EvalRunID != evaluator.wantEvalID {
		return CompletionContractEvaluationResult{}, errors.New("unexpected eval identity")
	}
	result, err := evaluator.delegate.Evaluate(ctx, input)
	evaluator.result = result
	return result, err
}

type failingCompletionEvaluator struct{ err error }

func (evaluator failingCompletionEvaluator) Evaluate(context.Context, CompletionContractEvaluationInput) (CompletionContractEvaluationResult, error) {
	return CompletionContractEvaluationResult{}, evaluator.err
}

type mismatchedEvidenceEvaluator struct {
	delegate Evaluator[CompletionContractEvaluationInput, CompletionContractEvaluationResult]
}

func (evaluator mismatchedEvidenceEvaluator) Evaluate(
	ctx context.Context,
	input CompletionContractEvaluationInput,
) (CompletionContractEvaluationResult, error) {
	result, err := evaluator.delegate.Evaluate(ctx, input)
	if result.Evidence != nil {
		cloned := cloneEvaluationEvidence(*result.Evidence)
		cloned.EvalRunID = "eval-from-another-execution"
		result.Evidence = &cloned
	}
	return result, err
}

type recordingEvaluatorObserver struct {
	events *[]string
	input  appobs.EvaluatorSpanInput
	err    error
}

func (observer *recordingEvaluatorObserver) RecordEvaluator(_ context.Context, input appobs.EvaluatorSpanInput) (appobs.PlatformSpanIdentity, error) {
	observer.input = input
	if observer.events != nil {
		*observer.events = append(*observer.events, "evaluator_observation")
	}
	return appobs.PlatformSpanIdentity{TraceID: t090PlatformTraceID, SpanID: t090EvaluatorSpanID, Projectable: true}, observer.err
}

type mutatingEvidenceStore struct{ events *[]string }

func (store *mutatingEvidenceStore) Append(_ context.Context, evidence aieval.EvaluationEvidence) error {
	*store.events = append(*store.events, "evidence_store")
	if evidence.Threshold != nil {
		*evidence.Threshold = 0.1
	}
	return nil
}

type recordingProjectionQueue struct {
	events *[]string
	input  ChatScoreProjectionInput
}

func (queue *recordingProjectionQueue) TryEnqueue(_ context.Context, input ChatScoreProjectionInput) error {
	*queue.events = append(*queue.events, "projection")
	queue.input = input
	return nil
}

type countingEvidenceStore struct {
	calls int
	err   error
}

func (store *countingEvidenceStore) Append(context.Context, aieval.EvaluationEvidence) error {
	store.calls++
	return store.err
}

type countingProjectionQueue struct {
	calls int
	err   error
}

func (queue *countingProjectionQueue) TryEnqueue(context.Context, ChatScoreProjectionInput) error {
	queue.calls++
	return queue.err
}
