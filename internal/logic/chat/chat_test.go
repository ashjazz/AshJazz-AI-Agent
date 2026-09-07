// Package chat fixes the application-layer contract for the first non-streaming chat flow.
//
// The controller owns HTTP validation and envelopes, while this usecase owns the point where
// a chat becomes an AI request. In particular, ai_trace_id must exist before the provider can
// fail, but it remains a domain identity rather than an OTel or platform trace identifier.
package chat

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	appobs "github.com/ashjazz/Longtermism/internal/observability"
	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	llmtestutil "github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	traceapi "go.opentelemetry.io/otel/trace"
)

func TestChatUsecaseReturnsProviderFactsAndCreatesAIIdentityBeforeProviderCall(t *testing.T) {
	provider := &scriptedProvider{
		chat: func(ctx context.Context, request *llm.ChatRequest) (*llm.ChatResponse, error) {
			identity, ok := obs.CorrelationIdentityFromContext(ctx)
			if !ok {
				t.Fatal("provider context must carry the correlation identity")
			}
			if identity.AITraceID != "ai-t070-success" {
				t.Fatalf("AITraceID at provider call = %q, want identity created before the call", identity.AITraceID)
			}
			if identity.ServiceTraceID != "otel-trace-t070" || identity.SpanID != "otel-span-t070" {
				t.Fatalf("provider identity = %#v, want existing service identities preserved", identity)
			}
			if identity.EvalRunID != "" {
				t.Fatalf("provider identity retained stale eval_run_id %q", identity.EvalRunID)
			}
			if request.Model != "server-configured-model" {
				t.Fatalf("provider request model = %q, want server-owned configured model", request.Model)
			}
			if !reflect.DeepEqual(request.Messages, []llm.Message{{Role: llm.RoleUser, Content: "Explain the evidence loop."}}) {
				t.Fatalf("provider request messages = %#v", request.Messages)
			}
			return &llm.ChatResponse{
				Content:      "Observe facts, evaluate evidence, then regress changes.",
				Model:        "provider-actual-model",
				FinishReason: llm.FinishLength,
				Usage: llmtestutil.MustReportedUsage(llm.Usage{
					InputTokens:      11,
					OutputTokens:     17,
					ReasoningTokens:  5,
					CacheReadTokens:  3,
					CacheWriteTokens: 2,
					TotalTokens:      28,
				}),
			}, nil
		},
	}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider:                provider,
		RequestedModel:          "server-configured-model",
		NewAITraceID:            func() string { return "ai-t070-success" },
		CanonicalizeActualModel: allowActualModels("provider-actual-model"),
	})
	ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity(
		"req-t070-success",
		obs.WithServiceSpan("otel-trace-t070", "otel-span-t070"),
		obs.WithAITraceID("untrusted-ai-trace-id"),
		obs.WithEvalRunID("untrusted-stale-eval-run"),
	))

	result, err := usecase.Execute(ctx, ChatCommand{Message: "Explain the evidence loop."})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Content != "Observe facts, evaluate evidence, then regress changes." {
		t.Fatalf("result content = %q", result.Content)
	}
	if result.Model != "provider-actual-model" {
		t.Fatalf("result model = %q, want actual provider model rather than requested model", result.Model)
	}
	if result.FinishReason != llm.FinishLength {
		t.Fatalf("result finish reason = %q, want %q", result.FinishReason, llm.FinishLength)
	}
	wantUsage := llm.Usage{InputTokens: 11, OutputTokens: 17, ReasoningTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 2, TotalTokens: 28}
	if result.Usage != wantUsage {
		t.Fatalf("result usage = %#v, want %#v", result.Usage, wantUsage)
	}
	wantIdentity := obs.NewCorrelationIdentity("req-t070-success", obs.WithServiceSpan("otel-trace-t070", "otel-span-t070"), obs.WithAITraceID("ai-t070-success"))
	if result.Identity != wantIdentity {
		t.Fatalf("result identity = %#v, want %#v", result.Identity, wantIdentity)
	}
}

// The trusted marker is an execution fact shared by semantic observers, while native service
// identity remains sourced from the active bridge SpanContext and is handed only to the local
// manifest writer. Neither identity may be guessed from ai_trace_id.
func TestChatUsecaseHandsTrustedSmokeIdentityToTelemetryAndManifest(t *testing.T) {
	const marker = "run-t177-usecase"
	manifestWriter := &recordingChatRunManifestWriter{}
	var providerSpanContext traceapi.SpanContext
	providerRuntime := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = providerRuntime.Shutdown(context.Background()) })
	tracer := providerRuntime.Tracer("t177-chat-manifest")
	rootContext, root := tracer.Start(context.Background(), "HTTP POST /api/v1/chat")
	rootSpanContext := root.SpanContext()
	defer root.End()
	provider := &scriptedProvider{chat: func(ctx context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
		providerSpanContext = traceapi.SpanContextFromContext(ctx)
		if got := SmokeRunIDFromContext(ctx); got != marker {
			t.Fatalf("provider smoke marker = %q, want %q", got, marker)
		}
		return &llm.ChatResponse{Content: "ok", Model: "provider-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{}), FinishReason: llm.FinishStop}, nil
	}}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider:                provider,
		RequestedModel:          "server-model",
		NewAITraceID:            func() string { return "ai-t177-domain" },
		CanonicalizeActualModel: allowActualModels("provider-model"),
		Bridge:                  appobs.NewChatAIExecutionBoundary(tracer),
		RunManifestWriter:       manifestWriter,
	})
	ctx := obs.ContextWithCorrelationIdentity(rootContext, obs.NewCorrelationIdentity(
		"req-t177-usecase",
		obs.WithServiceSpan("forged-domain-trace", "forged-domain-span"),
	))

	ctx = appobs.ContextWithChatSmokeRunID(ctx, marker)
	result, err := usecase.Execute(ctx, ChatCommand{Message: "controlled smoke", SmokeRunID: marker})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Identity.AITraceID != "ai-t177-domain" || manifestWriter.calls != 1 {
		t.Fatalf("result/manifest = identity:%#v calls:%d", result.Identity, manifestWriter.calls)
	}
	if manifestWriter.input.ServiceTraceID != rootSpanContext.TraceID().String() {
		t.Fatalf("manifest trace = %q, want active native trace %s", manifestWriter.input.ServiceTraceID, rootSpanContext.TraceID())
	}
	if !providerSpanContext.IsValid() || manifestWriter.input.SpanID != providerSpanContext.SpanID().String() {
		t.Fatalf("manifest span = %q, want active bridge span %s", manifestWriter.input.SpanID, providerSpanContext.SpanID())
	}
	want := ChatRunManifestInput{
		SmokeRunID:     marker,
		RequestID:      "req-t177-usecase",
		AITraceID:      "ai-t177-domain",
		ServiceTraceID: manifestWriter.input.ServiceTraceID,
		SpanID:         manifestWriter.input.SpanID,
	}
	if manifestWriter.input != want {
		t.Fatalf("manifest input = %#v, want native identities %#v", manifestWriter.input, want)
	}
	if manifestWriter.input.ServiceTraceID == result.Identity.AITraceID || manifestWriter.input.ServiceTraceID == "forged-domain-trace" || manifestWriter.input.SpanID == "forged-domain-span" {
		t.Fatal("manifest native identity must not be copied or derived from domain identity")
	}
}

// AIPlaneFactRecorder 是 infra smoke 的 AI-negative 事实源写入端口：只在受信任的
// smoke 执行真实创建 AI 桥接 span 后登记一次，普通 chat 与失败边界绝不登记。
func TestChatUsecaseRecordsAIPlaneFactOnlyForTrustedSmokeExecution(t *testing.T) {
	const marker = "run-t200-ai-plane"
	fixedNow := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)

	t.Run("trusted smoke execution records exactly one fact", func(t *testing.T) {
		recorder := &recordingAIPlaneFactRecorder{}
		providerRuntime := sdktrace.NewTracerProvider()
		t.Cleanup(func() { _ = providerRuntime.Shutdown(context.Background()) })
		tracer := providerRuntime.Tracer("t200-ai-plane-recording")
		rootContext, root := tracer.Start(context.Background(), "HTTP POST /api/v1/chat")
		defer root.End()
		usecase := NewChatUsecase(ChatUsecaseDependencies{
			Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
				return &llm.ChatResponse{Model: "provider-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{}), FinishReason: llm.FinishStop}, nil
			}},
			RequestedModel:          "server-model",
			NewAITraceID:            func() string { return "ai-t200-recording" },
			CanonicalizeActualModel: allowActualModels("provider-model"),
			Bridge:                  appobs.NewChatAIExecutionBoundary(tracer),
			AIPlaneFacts:            recorder,
			Now:                     func() time.Time { return fixedNow },
		})
		ctx := obs.ContextWithCorrelationIdentity(rootContext, obs.NewCorrelationIdentity("req-t200-recording"))
		ctx = appobs.ContextWithChatSmokeRunID(ctx, marker)
		if _, err := usecase.Execute(ctx, ChatCommand{Message: "controlled smoke", SmokeRunID: marker}); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if recorder.calls != 1 || recorder.marker != marker || !recorder.at.Equal(fixedNow) {
			t.Fatalf("ai plane fact = calls:%d marker:%q at:%s, want one fact for the trusted marker at the injected clock", recorder.calls, recorder.marker, recorder.at)
		}
	})

	t.Run("ordinary chat never records an ai plane fact", func(t *testing.T) {
		recorder := &recordingAIPlaneFactRecorder{}
		usecase := NewChatUsecase(ChatUsecaseDependencies{
			Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
				return &llm.ChatResponse{Model: "provider-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{}), FinishReason: llm.FinishStop}, nil
			}},
			RequestedModel:          "server-model",
			NewAITraceID:            func() string { return "ai-t200-ordinary" },
			CanonicalizeActualModel: allowActualModels("provider-model"),
			AIPlaneFacts:            recorder,
		})
		ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity("req-t200-ordinary"))
		if _, err := usecase.Execute(ctx, ChatCommand{Message: "ordinary chat"}); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if recorder.calls != 0 {
			t.Fatalf("ordinary chat ai plane facts = %d, want zero", recorder.calls)
		}
	})

	t.Run("untrusted execution without a bridge span records nothing", func(t *testing.T) {
		recorder := &recordingAIPlaneFactRecorder{}
		usecase := NewChatUsecase(ChatUsecaseDependencies{
			Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
				return &llm.ChatResponse{Model: "provider-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{}), FinishReason: llm.FinishStop}, nil
			}},
			RequestedModel:          "server-model",
			NewAITraceID:            func() string { return "ai-t200-untrusted" },
			CanonicalizeActualModel: allowActualModels("provider-model"),
			AIPlaneFacts:            recorder,
		})
		ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity("req-t200-untrusted"))
		ctx = appobs.ContextWithChatSmokeRunID(ctx, "run-t200-untrusted")
		if _, err := usecase.Execute(ctx, ChatCommand{Message: "smoke without bridge", SmokeRunID: "run-t200-untrusted"}); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if recorder.calls != 0 {
			t.Fatalf("untrusted execution ai plane facts = %d, want zero without a real bridge span", recorder.calls)
		}
	})

	t.Run("missing recorder keeps the business result unchanged", func(t *testing.T) {
		usecase := NewChatUsecase(ChatUsecaseDependencies{
			Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
				return &llm.ChatResponse{Model: "provider-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{}), FinishReason: llm.FinishStop}, nil
			}},
			RequestedModel:          "server-model",
			NewAITraceID:            func() string { return "ai-t200-no-recorder" },
			CanonicalizeActualModel: allowActualModels("provider-model"),
		})
		ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity("req-t200-no-recorder"))
		ctx = appobs.ContextWithChatSmokeRunID(ctx, "run-t200-no-recorder")
		result, err := usecase.Execute(ctx, ChatCommand{Message: "smoke without recorder", SmokeRunID: "run-t200-no-recorder"})
		if err != nil || result.Model != "provider-model" {
			t.Fatalf("Execute() = (%#v, %v), want unchanged business result without a recorder", result, err)
		}
	})
}

type recordingAIPlaneFactRecorder struct {
	calls  int
	marker string
	at     time.Time
}

func (r *recordingAIPlaneFactRecorder) RecordAIPlaneFact(marker string, at time.Time) {
	r.calls++
	r.marker, r.at = marker, at
}

func TestOrdinaryChatDoesNotWriteSmokeRunManifest(t *testing.T) {
	writer := &recordingChatRunManifestWriter{}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Model: "provider-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{}), FinishReason: llm.FinishStop}, nil
		}},
		RequestedModel:          "server-model",
		NewAITraceID:            func() string { return "ai-t177-ordinary" },
		CanonicalizeActualModel: allowActualModels("provider-model"),
		RunManifestWriter:       writer,
	})
	ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity("req-t177-ordinary"))
	if _, err := usecase.Execute(ctx, ChatCommand{Message: "ordinary chat"}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if writer.calls != 0 {
		t.Fatalf("ordinary chat manifest writes = %d, want zero", writer.calls)
	}
}

func TestChatCommandCannotElevateAnUntrustedSmokeMarker(t *testing.T) {
	writer := &recordingChatRunManifestWriter{}
	usecase := NewChatUsecase(ChatUsecaseDependencies{RunManifestWriter: writer})
	_, err := usecase.Execute(context.Background(), ChatCommand{Message: "untrusted", SmokeRunID: "run-t182-command-only"})
	if !errors.Is(err, ErrChatConfiguration) || writer.calls != 0 {
		t.Fatalf("command-only marker = error:%v writes:%d, want fail-closed", err, writer.calls)
	}
}

func TestChatSmokeManifestRejectsBridgeIdentityWithoutActiveSpanContext(t *testing.T) {
	writer := &recordingChatRunManifestWriter{}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Model: "provider-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{}), FinishReason: llm.FinishStop}, nil
		}},
		RequestedModel:          "server-model",
		NewAITraceID:            func() string { return "ai-t177-no-native-span" },
		CanonicalizeActualModel: allowActualModels("provider-model"),
		Bridge: &recordingChatBridge{identity: obs.NewCorrelationIdentity(
			"req-t177-no-native-span",
			obs.WithAITraceID("ai-t177-no-native-span"),
			obs.WithServiceSpan("0123456789abcdef0123456789abcdef", "0123456789abcdef"),
		)},
		RunManifestWriter: writer,
	})
	ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity("req-t177-no-native-span"))
	ctx = appobs.ContextWithChatSmokeRunID(ctx, "run-t177-no-native-span")
	if _, err := usecase.Execute(ctx, ChatCommand{Message: "controlled smoke", SmokeRunID: "run-t177-no-native-span"}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if writer.calls != 0 {
		t.Fatalf("manifest writes without active native SpanContext = %d, want zero", writer.calls)
	}
}

type recordingChatRunManifestWriter struct {
	calls int
	input ChatRunManifestInput
}

func (writer *recordingChatRunManifestWriter) Write(_ context.Context, input ChatRunManifestInput) error {
	writer.calls++
	writer.input = input
	return nil
}

// Provider failures happen after the usecase has created its domain identity. Keeping both the
// stable cause and the identity lets the controller select 502/429/504 without manufacturing a
// new correlation value after a failure has already occurred.
func TestChatUsecasePreservesFailureClassAndAIIdentity(t *testing.T) {
	tests := []struct {
		name   string
		cause  error
		wantIs []error
	}{
		{name: "upstream unavailable", cause: fmt.Errorf("provider unavailable: %w", llm.ErrUpstream), wantIs: []error{llm.ErrUpstream}},
		{name: "provider rate limited", cause: errors.Join(llm.ErrUpstream, llm.ErrRateLimit), wantIs: []error{llm.ErrUpstream, llm.ErrRateLimit}},
		{name: "provider timeout", cause: errors.Join(llm.ErrUpstream, context.DeadlineExceeded), wantIs: []error{llm.ErrUpstream, context.DeadlineExceeded}},
		{name: "sanitizes provider detail", cause: errors.Join(llm.ErrUpstream, errors.New("provider body forbidden-provider-detail-t070 api-key=forbidden-key-t070")), wantIs: []error{llm.ErrUpstream}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &scriptedProvider{
				chat: func(ctx context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
					identity, ok := obs.CorrelationIdentityFromContext(ctx)
					if !ok || identity.AITraceID != "ai-t070-failure" {
						t.Fatalf("provider must observe a pre-created AI identity, got %#v present=%t", identity, ok)
					}
					return nil, tt.cause
				},
			}
			usecase := NewChatUsecase(ChatUsecaseDependencies{
				Provider:                provider,
				RequestedModel:          "server-configured-model",
				NewAITraceID:            func() string { return "ai-t070-failure" },
				CanonicalizeActualModel: allowActualModels("provider-actual-model"),
			})
			ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity("req-t070-failure"))

			result, err := usecase.Execute(ctx, ChatCommand{Message: "Will this retain correlation on failure?"})
			if err == nil {
				t.Fatal("Execute() error = nil, want preserved provider failure classification")
			}
			for _, want := range tt.wantIs {
				if !errors.Is(err, want) {
					t.Fatalf("Execute() error must preserve errors.Is(_, %v)", want)
				}
			}
			if strings.Contains(err.Error(), "forbidden-provider-detail-t070") || strings.Contains(err.Error(), "forbidden-key-t070") {
				t.Fatal("usecase error must not include raw provider details or credentials")
			}
			if result.Identity.RequestID != "req-t070-failure" || result.Identity.AITraceID != "ai-t070-failure" {
				t.Fatalf("failure result identity = %#v, want request and pre-created AI identity", result.Identity)
			}
		})
	}
}

// The usecase is a telemetry producer. It must export only correlation and model facts, never
// the raw user message or provider output that passes through its business result.
func TestChatUsecaseRecordsOnlyLowSensitivityTelemetryFacts(t *testing.T) {
	const messageMarker = "forbidden-user-message-t070"
	const outputMarker = "forbidden-provider-output-t070"
	telemetry := &recordingTelemetry{}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: outputMarker, Model: "provider-actual-model", FinishReason: llm.FinishStop, Usage: llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 13, OutputTokens: 7, TotalTokens: 20})}, nil
		}},
		RequestedModel:          "server-configured-model",
		NewAITraceID:            func() string { return "ai-t070-private" },
		CanonicalizeActualModel: allowActualModels("provider-actual-model"),
		Telemetry:               telemetry,
	})
	ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity("req-t070-private", obs.WithServiceSpan("otel-trace-private", "otel-span-private")))

	if _, err := usecase.Execute(ctx, ChatCommand{Message: messageMarker}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(telemetry.traces) != 1 {
		t.Fatalf("telemetry trace count = %d, want one", len(telemetry.traces))
	}
	trace := telemetry.traces[0]
	if trace.TraceID != "ai-t070-private" || trace.RequestID != "req-t070-private" || trace.ServiceTraceID != "otel-trace-private" || trace.SpanID != "otel-span-private" {
		t.Fatal("telemetry trace must retain explicit correlation identities")
	}
	if trace.Model != "provider-actual-model" || trace.InputTokens != 13 || trace.OutputTokens != 7 || trace.OutcomeStatus != "success" {
		t.Fatal("telemetry trace must preserve low-sensitivity provider outcome facts")
	}
	serialized := fmt.Sprintf("%#v", trace)
	if strings.Contains(serialized, messageMarker) || strings.Contains(serialized, outputMarker) {
		t.Fatal("telemetry trace must not contain raw user input or provider output")
	}
}

// IDs belong to an execution, not a usecase instance. Reusing one cached value would merge
// independent chat trajectories and their later evaluation evidence into the wrong trace.
func TestChatUsecaseGeneratesFreshAIIdentityForEachExecution(t *testing.T) {
	var providerAITraceIDs []string
	provider := &scriptedProvider{chat: func(ctx context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
		identity, ok := obs.CorrelationIdentityFromContext(ctx)
		if !ok {
			t.Fatal("provider context must contain a generated identity")
		}
		providerAITraceIDs = append(providerAITraceIDs, identity.AITraceID)
		return &llm.ChatResponse{Model: "provider-actual-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{}), FinishReason: llm.FinishStop}, nil
	}}
	ids := []string{"ai-t070-first", "ai-t070-second"}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider:                provider,
		RequestedModel:          "server-configured-model",
		CanonicalizeActualModel: allowActualModels("provider-actual-model"),
		NewAITraceID: func() string {
			id := ids[0]
			ids = ids[1:]
			return id
		},
	})
	ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity("req-t070-repeat"))

	first, firstErr := usecase.Execute(ctx, ChatCommand{Message: "first"})
	second, secondErr := usecase.Execute(ctx, ChatCommand{Message: "second"})
	if firstErr != nil || secondErr != nil {
		t.Fatalf("Execute() errors = first:%v second:%v", firstErr, secondErr)
	}
	if !reflect.DeepEqual(providerAITraceIDs, []string{"ai-t070-first", "ai-t070-second"}) {
		t.Fatalf("provider AI identities = %#v, want a fresh ID per execution", providerAITraceIDs)
	}
	if first.Identity.AITraceID != "ai-t070-first" || second.Identity.AITraceID != "ai-t070-second" {
		t.Fatal("results must retain their own execution-specific AI identities")
	}
}

func TestChatUsecaseRecordsSanitizedFailureTelemetry(t *testing.T) {
	const messageMarker = "forbidden-failure-message-t070"
	const providerMarker = "forbidden-failure-provider-detail-t070"
	telemetry := &recordingTelemetry{}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
			return nil, errors.Join(llm.ErrUpstream, errors.New(providerMarker+" api-key=forbidden-failure-key-t070"))
		}},
		RequestedModel:          "server-configured-model",
		NewAITraceID:            func() string { return "ai-t070-failure-private" },
		CanonicalizeActualModel: allowActualModels("provider-actual-model"),
		Telemetry:               telemetry,
	})
	ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity("req-t070-failure-private"))

	result, err := usecase.Execute(ctx, ChatCommand{Message: messageMarker})
	if err == nil {
		t.Fatal("Execute() error = nil, want provider failure")
	}
	if !errors.Is(err, llm.ErrUpstream) {
		t.Fatal("failure must retain the upstream sentinel for resilience handling")
	}
	if strings.Contains(err.Error(), messageMarker) || strings.Contains(err.Error(), providerMarker) || strings.Contains(err.Error(), "forbidden-failure-key-t070") {
		t.Fatal("business error must not expose raw user input, provider failure detail, or credentials")
	}
	if result.Identity.AITraceID != "ai-t070-failure-private" || len(telemetry.traces) != 1 {
		t.Fatal("provider failure must retain its generated identity and record one telemetry fact")
	}
	trace := telemetry.traces[0]
	if trace.TraceID != "ai-t070-failure-private" || trace.OutcomeStatus != string(obs.FailureUpstream) {
		t.Fatal("failure telemetry must retain the identity and stable upstream failure class")
	}
	serialized := fmt.Sprintf("%#v", trace)
	if strings.Contains(serialized, messageMarker) || strings.Contains(serialized, providerMarker) || strings.Contains(serialized, "forbidden-failure-key-t070") {
		t.Fatal("failure telemetry must not contain raw user input, provider detail, or credentials")
	}
}

// A telemetry exporter is a side channel. Its explicit delivery failure must remain diagnosable
// without converting a completed model response into an upstream failure or dropping model facts.
func TestChatUsecaseKeepsBusinessResultWhenTelemetryFails(t *testing.T) {
	provider := &scriptedProvider{
		chat: func(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{
				Content:      "business result survives telemetry failure",
				Model:        "provider-actual-model",
				FinishReason: llm.FinishStop,
				Usage:        llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 2, OutputTokens: 5, TotalTokens: 7}),
			}, nil
		},
	}
	telemetry := &failingTelemetry{err: errors.New("synthetic exporter unavailable")}
	diagnostics := &recordingTelemetryDiagnostics{}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider:                provider,
		RequestedModel:          "server-configured-model",
		NewAITraceID:            func() string { return "ai-t070-telemetry" },
		CanonicalizeActualModel: allowActualModels("provider-actual-model"),
		Telemetry:               telemetry,
		Diagnostics:             diagnostics,
	})
	ctx := obs.ContextWithCorrelationIdentity(context.Background(), obs.NewCorrelationIdentity("req-t070-telemetry"))

	result, err := usecase.Execute(ctx, ChatCommand{Message: "Do telemetry failures change this?"})
	if err != nil {
		t.Fatalf("Execute() error = %v, want telemetry failure isolated", err)
	}
	if telemetry.calls != 1 {
		t.Fatalf("telemetry record calls = %d, want one attempted record", telemetry.calls)
	}
	if !reflect.DeepEqual(diagnostics.failures, []ChatTelemetryFailure{{Component: "generation", ErrorClass: "telemetry_export_failed"}}) {
		t.Fatalf("telemetry diagnostics = %#v, want one low-sensitivity failure fact", diagnostics.failures)
	}
	if result.Content != "business result survives telemetry failure" || result.Model != "provider-actual-model" || result.Usage.TotalTokens != 7 {
		t.Fatalf("business result = %#v, want unchanged provider facts", result)
	}
	if result.Identity.AITraceID != "ai-t070-telemetry" {
		t.Fatalf("business result identity = %#v, want the pre-created AI identity", result.Identity)
	}
}

func TestChatUsecaseFailsSafelyAtConfigurationBoundaries(t *testing.T) {
	tests := []struct {
		name         string
		dependencies ChatUsecaseDependencies
		wantAITrace  string
	}{
		{
			name:         "missing identity generator does not trust inbound AI identity",
			dependencies: ChatUsecaseDependencies{Provider: &scriptedProvider{}, RequestedModel: "server-model"},
		},
		{
			name: "empty generated identity does not trust inbound AI identity",
			dependencies: ChatUsecaseDependencies{
				Provider:       &scriptedProvider{},
				RequestedModel: "server-model",
				NewAITraceID:   func() string { return " \t" },
			},
		},
		{
			name: "missing provider retains newly started AI identity",
			dependencies: ChatUsecaseDependencies{
				RequestedModel: "server-model",
				NewAITraceID:   func() string { return "ai-t090-missing-provider" },
			},
			wantAITrace: "ai-t090-missing-provider",
		},
		{
			name: "missing requested model retains newly started AI identity",
			dependencies: ChatUsecaseDependencies{
				Provider:     &scriptedProvider{},
				NewAITraceID: func() string { return "ai-t090-missing-model" },
			},
			wantAITrace: "ai-t090-missing-model",
		},
		{
			name: "missing canonical model mapper fails before provider call",
			dependencies: ChatUsecaseDependencies{
				Provider:       &scriptedProvider{},
				RequestedModel: "server-model",
				NewAITraceID:   func() string { return "ai-t090-missing-canonicalizer" },
			},
			wantAITrace: "ai-t090-missing-canonicalizer",
		},
		{
			name: "sensitive requested model is rejected before telemetry",
			dependencies: ChatUsecaseDependencies{
				Provider:                &scriptedProvider{},
				RequestedModel:          "sk-proj-forbidden-model-secret",
				NewAITraceID:            func() string { return "ai-t090-sensitive-model" },
				CanonicalizeActualModel: allowActualModels("provider-actual-model"),
			},
			wantAITrace: "ai-t090-sensitive-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usecase := NewChatUsecase(tt.dependencies)
			ctx := obs.ContextWithCorrelationIdentity(
				context.Background(),
				obs.NewCorrelationIdentity("req-t090-config", obs.WithAITraceID("untrusted-inbound-ai-id")),
			)

			result, err := usecase.Execute(ctx, ChatCommand{Message: "configuration boundary"})
			if !errors.Is(err, ErrChatConfiguration) {
				t.Fatalf("Execute() error = %v, want ErrChatConfiguration", err)
			}
			if result.Identity.RequestID != "req-t090-config" || result.Identity.AITraceID != tt.wantAITrace {
				t.Fatalf("configuration failure identity = %#v, want request identity and AI trace %q", result.Identity, tt.wantAITrace)
			}
		})
	}
}

func TestChatUsecaseRejectsNilContextWithoutCallingProvider(t *testing.T) {
	provider := &scriptedProvider{}
	usecase := NewChatUsecase(ChatUsecaseDependencies{
		Provider:       provider,
		RequestedModel: "server-model",
		NewAITraceID:   func() string { return "ai-t090-nil-context" },
	})

	result, err := usecase.Execute(nil, ChatCommand{Message: "nil context"})
	if !errors.Is(err, ErrChatInvalidContext) || result != (ChatResult{}) {
		t.Fatalf("Execute(nil) = (%#v, %v), want zero result and invalid-context error", result, err)
	}
}

func TestChatUsecaseClassifiesAdditionalProviderContractFailures(t *testing.T) {
	tests := []struct {
		name        string
		providerErr error
		nilResponse bool
		wantError   error
		wantOutcome string
	}{
		{
			name:        "caller cancellation stays a caller failure",
			providerErr: context.Canceled,
			wantError:   context.Canceled,
			wantOutcome: string(obs.FailureCallerError),
		},
		{
			name:        "unknown provider detail becomes a safe sentinel",
			providerErr: errors.New("forbidden-provider-body-t090 authorization=Bearer-forbidden-t090"),
			wantError:   ErrChatProviderFailure,
			wantOutcome: string(obs.FailureUpstream),
		},
		{
			name:        "nil response is rejected without panic",
			nilResponse: true,
			wantError:   ErrChatInvalidResponse,
			wantOutcome: string(obs.FailureUpstream),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			telemetry := &recordingTelemetry{}
			usecase := NewChatUsecase(ChatUsecaseDependencies{
				Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
					if tt.nilResponse {
						return nil, nil
					}
					return nil, tt.providerErr
				}},
				RequestedModel:          "server-model",
				NewAITraceID:            func() string { return "ai-t090-contract" },
				CanonicalizeActualModel: allowActualModels("provider-model-v1"),
				Telemetry:               telemetry,
			})

			result, err := usecase.Execute(context.Background(), ChatCommand{Message: "provider contract boundary"})
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("Execute() error = %v, want errors.Is(_, %v)", err, tt.wantError)
			}
			if strings.Contains(err.Error(), "forbidden-provider-body-t090") || strings.Contains(err.Error(), "Bearer-forbidden-t090") {
				t.Fatal("provider contract error leaked raw external detail")
			}
			if result.Identity.AITraceID != "ai-t090-contract" || len(telemetry.traces) != 1 {
				t.Fatalf("provider contract failure result=%#v traces=%d, want identity and one failure fact", result, len(telemetry.traces))
			}
			if telemetry.traces[0].OutcomeStatus != tt.wantOutcome {
				t.Fatalf("failure outcome = %q, want %q", telemetry.traces[0].OutcomeStatus, tt.wantOutcome)
			}
		})
	}
}

func TestChatUsecaseRejectsUnsafeProviderResponseFacts(t *testing.T) {
	valid := llm.ChatResponse{
		Content:      "safe response",
		Model:        "provider-model-v1",
		FinishReason: llm.FinishStop,
		Usage:        llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}),
	}
	cloneValid := func() llm.ChatResponse {
		response := valid
		summary, reported := valid.Usage.Summary()
		if !reported {
			t.Fatal("valid fixture must carry explicitly reported usage")
		}
		response.Usage = llmtestutil.MustReportedUsage(summary)
		return response
	}
	tests := []struct {
		name     string
		response llm.ChatResponse
	}{
		{
			name: "credential-shaped model outside canonical allowlist",
			response: func() llm.ChatResponse {
				response := cloneValid()
				response.Model = "sk-proj-forbidden-model-t090"
				return response
			}(),
		},
		{
			name: "overlong model",
			response: func() llm.ChatResponse {
				response := cloneValid()
				response.Model = strings.Repeat("m", maxModelIdentifierBytes+1)
				return response
			}(),
		},
		{
			name: "tool call cannot be silently dropped by non-tool chat",
			response: func() llm.ChatResponse {
				response := cloneValid()
				response.FinishReason = llm.FinishToolCall
				response.ToolCalls = []llm.ToolCall{{ID: "call-t090", Name: "unsafe-unconfigured-tool"}}
				return response
			}(),
		},
		{
			name: "unknown finish reason is outside the public contract",
			response: func() llm.ChatResponse {
				response := cloneValid()
				response.FinishReason = "safety_blocked"
				return response
			}(),
		},
		{
			name: "unavailable usage",
			response: func() llm.ChatResponse {
				response := cloneValid()
				response.Usage = llm.NewUnavailableProviderUsage()
				return response
			}(),
		},
		{
			name: "negative usage",
			response: func() llm.ChatResponse {
				response := cloneValid()
				invalid, err := llm.NewReportedProviderUsage(llm.Usage{OutputTokens: -1})
				if !errors.Is(err, llm.ErrInvalidResponse) {
					t.Fatal("constructor must reject negative usage")
				}
				response.Usage = invalid
				return response
			}(),
		},
		{
			name: "inconsistent total usage",
			response: func() llm.ChatResponse {
				response := cloneValid()
				invalid, err := llm.NewReportedProviderUsage(llm.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 4})
				if !errors.Is(err, llm.ErrInvalidResponse) {
					t.Fatal("constructor must reject inconsistent usage")
				}
				response.Usage = invalid
				return response
			}(),
		},
		{
			name: "oversized content",
			response: func() llm.ChatResponse {
				response := cloneValid()
				response.Content = strings.Repeat("x", maxChatResponseBytes+1)
				return response
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			telemetry := &recordingTelemetry{}
			usecase := NewChatUsecase(ChatUsecaseDependencies{
				Provider: &scriptedProvider{chat: func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
					response := tt.response
					return &response, nil
				}},
				RequestedModel:          "server-model",
				NewAITraceID:            func() string { return "ai-t090-invalid-response" },
				CanonicalizeActualModel: allowActualModels("provider-model-v1"),
				Telemetry:               telemetry,
			})

			result, err := usecase.Execute(context.Background(), ChatCommand{Message: "validate response"})
			if !errors.Is(err, ErrChatInvalidResponse) || result.Content != "" || result.Model != "" {
				t.Fatalf("Execute() = (%#v, %v), want rejected response without provider facts", result, err)
			}
			if len(telemetry.traces) != 1 || telemetry.traces[0].Model != "server-model" {
				t.Fatalf("invalid response trace = %#v, want only safe requested model", telemetry.traces)
			}
			serialized := fmt.Sprintf("%#v", telemetry.traces[0])
			if strings.Contains(serialized, "sk-proj-forbidden-model-t090") {
				t.Fatal("invalid provider model leaked into telemetry")
			}
		})
	}
}

type scriptedProvider struct {
	chat func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error)
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{}
}

func (p *scriptedProvider) Chat(ctx context.Context, request *llm.ChatRequest) (*llm.ChatResponse, error) {
	if p.chat == nil {
		return nil, errors.New("scripted provider must not be called")
	}
	return p.chat(ctx, request)
}

func (p *scriptedProvider) ChatStream(context.Context, *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	return nil, errors.New("streaming is outside the non-streaming chat usecase contract")
}

type recordingTelemetry struct {
	traces []obs.Trace
}

func (t *recordingTelemetry) TryRecord(trace obs.Trace) error {
	t.traces = append(t.traces, trace)
	return nil
}

type failingTelemetry struct {
	calls int
	err   error
}

func (t *failingTelemetry) TryRecord(obs.Trace) error {
	t.calls++
	return t.err
}

type recordingTelemetryDiagnostics struct {
	failures []ChatTelemetryFailure
}

func (d *recordingTelemetryDiagnostics) TryRecordTelemetryFailure(failure ChatTelemetryFailure) {
	d.failures = append(d.failures, failure)
}

func allowActualModels(models ...string) CanonicalizeActualModel {
	allowed := make(map[string]string, len(models))
	for _, model := range models {
		allowed[model] = model
	}
	return func(raw string) (string, bool) {
		canonical, ok := allowed[raw]
		return canonical, ok
	}
}
