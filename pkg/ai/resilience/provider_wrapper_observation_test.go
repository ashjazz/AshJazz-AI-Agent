package resilience

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	llmtestutil "github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
	"github.com/ashjazz/Longtermism/pkg/ai/obs/testutil"
)

func TestProviderWrapperRecordsOutcomeObservation(t *testing.T) {
	callerErr := errors.New("openai chat request failed with status 400")
	tests := []struct {
		name              string
		model             string
		setupProvider     func(*countingProvider)
		wantErrIs         error
		wantOutcome       string
		wantFailureStatus string
		wantModel         string
		wantCircuitState  State
		wantDegraded      bool
		wantRateLimited   bool
	}{
		{
			name:  "upstream failure opens circuit and records stable failure status",
			model: "upstream-model",
			setupProvider: func(provider *countingProvider) {
				provider.chatErrors["upstream-model"] = fmt.Errorf("provider unavailable: %w", llm.ErrUpstream)
			},
			wantErrIs:         llm.ErrUpstream,
			wantOutcome:       "failure",
			wantFailureStatus: string(obs.FailureUpstream),
			wantCircuitState:  StateOpen,
		},
		{
			name:  "rate limited upstream keeps rate limit diagnosable",
			model: "rate-limit-model",
			setupProvider: func(provider *countingProvider) {
				provider.chatErrors["rate-limit-model"] = fmt.Errorf("provider 429: %w: %w", llm.ErrRateLimit, llm.ErrUpstream)
			},
			wantErrIs:         llm.ErrRateLimit,
			wantOutcome:       "failure",
			wantFailureStatus: string(obs.FailureRateLimit),
			wantCircuitState:  StateOpen,
			wantRateLimited:   true,
		},
		{
			name:  "model fallback records degraded outcome",
			model: "premium-model",
			setupProvider: func(provider *countingProvider) {
				provider.chatResponses["premium-model"] = llm.ChatResponse{
					Content:      "served by fallback",
					Model:        "fallback-model",
					Usage:        llmtestutil.MustReportedUsage(llm.Usage{}),
					FinishReason: llm.FinishStop,
				}
			},
			wantOutcome:      "degraded",
			wantModel:        "fallback-model",
			wantCircuitState: StateClosed,
			wantDegraded:     true,
		},
		{
			name:  "caller 4xx error records caller failure without opening circuit",
			model: "bad-request-model",
			setupProvider: func(provider *countingProvider) {
				provider.chatErrors["bad-request-model"] = callerErr
			},
			wantErrIs:         ErrProviderRejected,
			wantOutcome:       "failure",
			wantFailureStatus: string(obs.FailureCallerError),
			wantCircuitState:  StateClosed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := obs.NewCorrelationIdentity(
				"req-provider-"+tt.model,
				obs.WithServiceSpan("svc-trace-provider-"+tt.model, "span-provider-"+tt.model),
				obs.WithAITraceID("ai-trace-provider-"+tt.model),
			)
			recorder := testutil.NewRecorder()
			provider := newCountingProvider()
			tt.setupProvider(provider)
			breaker := NewCircuitBreaker(Config{
				FailureThreshold: 1,
				RecoveryTimeout:  time.Minute,
			})
			wrapped := NewProviderWrapper(
				provider,
				breaker,
				WithTracer(recorder),
				WithFeature("llm_generation"),
				WithNow(scriptedProviderObservationClock(
					time.Date(2026, time.July, 7, 10, 0, 0, 0, time.UTC),
					time.Date(2026, time.July, 7, 10, 0, 0, 42*int(time.Millisecond), time.UTC),
				)),
			)
			ctx := obs.ContextWithCorrelationIdentity(context.Background(), identity)

			response, err := wrapped.Chat(ctx, &llm.ChatRequest{Model: tt.model})
			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("Chat() error = %v, want errors.Is %v", err, tt.wantErrIs)
				}
			} else if err != nil {
				t.Fatalf("Chat() error = %v", err)
			}
			if tt.wantModel != "" && response.Model != tt.wantModel {
				t.Fatalf("response Model = %q, want %q", response.Model, tt.wantModel)
			}
			if breaker.State() != tt.wantCircuitState {
				t.Fatalf("breaker State() = %q, want %q", breaker.State(), tt.wantCircuitState)
			}

			recorder.AssertCount(t, 1)
			recorder.AssertTrace(t, 0, func(t *testing.T, trace obs.Trace) {
				t.Helper()

				assertProviderWrapperTraceIdentity(t, trace, identity)
				if trace.ProviderName != provider.Name() {
					t.Fatalf("ProviderName = %q, want %q", trace.ProviderName, provider.Name())
				}
				if trace.RequestedModel != tt.model {
					t.Fatalf("RequestedModel = %q, want %q", trace.RequestedModel, tt.model)
				}
				if trace.Model != tt.wantModel {
					t.Fatalf("Model = %q, want %q", trace.Model, tt.wantModel)
				}
				if trace.OutcomeStatus != tt.wantOutcome {
					t.Fatalf("OutcomeStatus = %q, want %q", trace.OutcomeStatus, tt.wantOutcome)
				}
				if trace.FailureStatus != tt.wantFailureStatus {
					t.Fatalf("FailureStatus = %q, want %q", trace.FailureStatus, tt.wantFailureStatus)
				}
				if trace.CircuitState != string(tt.wantCircuitState) {
					t.Fatalf("CircuitState = %q, want %q", trace.CircuitState, tt.wantCircuitState)
				}
				if trace.Degraded != tt.wantDegraded {
					t.Fatalf("Degraded = %v, want %v", trace.Degraded, tt.wantDegraded)
				}
				if trace.RateLimited != tt.wantRateLimited {
					t.Fatalf("RateLimited = %v, want %v", trace.RateLimited, tt.wantRateLimited)
				}
				if trace.TotalLatencyMs != 42 {
					t.Fatalf("TotalLatencyMs = %d, want 42", trace.TotalLatencyMs)
				}
			})
		})
	}
}

func assertProviderWrapperTraceIdentity(t *testing.T, trace obs.Trace, identity obs.CorrelationIdentity) {
	t.Helper()

	if trace.TraceID != identity.AITraceID {
		t.Fatalf("TraceID = %q, want ai_trace_id %q", trace.TraceID, identity.AITraceID)
	}
	if trace.RequestID != identity.RequestID {
		t.Fatalf("RequestID = %q, want %q", trace.RequestID, identity.RequestID)
	}
	if trace.ServiceTraceID != identity.ServiceTraceID {
		t.Fatalf("ServiceTraceID = %q, want %q", trace.ServiceTraceID, identity.ServiceTraceID)
	}
	if trace.SpanID != identity.SpanID {
		t.Fatalf("SpanID = %q, want %q", trace.SpanID, identity.SpanID)
	}
	if trace.ObservationType != obs.ObservationTypeGeneration {
		t.Fatalf("ObservationType = %q, want %q", trace.ObservationType, obs.ObservationTypeGeneration)
	}
	if trace.Feature != "llm_generation" {
		t.Fatalf("Feature = %q, want llm_generation", trace.Feature)
	}
}

func scriptedProviderObservationClock(times ...time.Time) func() time.Time {
	index := 0
	return func() time.Time {
		if len(times) == 0 {
			return time.Time{}
		}
		if index >= len(times) {
			return times[len(times)-1]
		}
		now := times[index]
		index++
		return now
	}
}
