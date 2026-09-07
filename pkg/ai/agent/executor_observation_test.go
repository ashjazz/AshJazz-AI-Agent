package agent

import (
	"context"
	"testing"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	llmtestutil "github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
	"github.com/ashjazz/Longtermism/pkg/ai/obs/testutil"
)

func TestExecutorRecordsAgentStepObservationForToolCall(t *testing.T) {
	identity := obs.NewCorrelationIdentity(
		"req-agent-observation-001",
		obs.WithServiceSpan("svc-trace-agent-observation-001", "span-agent-observation-001"),
		obs.WithAITraceID("ai-trace-agent-observation-001"),
	)
	recorder := testutil.NewRecorder()
	registry := registryWithTool(t, newLimitTool("search_docs"))
	provider := newScriptedProvider(
		toolCallResponse("call-search-docs", "search_docs", map[string]any{"query": "agent observability"}),
		llm.ChatResponse{
			Content:      "Agent observability records native tool steps.",
			Usage:        llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}),
			FinishReason: llm.FinishStop,
		},
	)
	executor := NewExecutor(
		provider,
		registry,
		WithTracer(recorder),
		WithFeature("agent_loop"),
		WithNow(scriptedAgentObservationClock(
			time.Date(2026, time.July, 7, 9, 0, 0, 0, time.UTC),
			time.Date(2026, time.July, 7, 9, 0, 0, 25*int(time.Millisecond), time.UTC),
		)),
	)
	ctx := obs.ContextWithCorrelationIdentity(context.Background(), identity)

	result, err := executor.Run(ctx, Request{
		Query: "observe a native tool step",
		Model: "tool-model",
		Limit: Limit{MaxSteps: 3},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.TerminatedBy != terminatedFinished {
		t.Fatalf("TerminatedBy = %q, want %q", result.TerminatedBy, terminatedFinished)
	}

	recorder.AssertCount(t, 1)
	recorder.AssertTrace(t, 0, func(t *testing.T, trace obs.Trace) {
		t.Helper()

		assertAgentTraceIdentity(t, trace, identity)
		if trace.AgentStepIndex != 1 {
			t.Fatalf("AgentStepIndex = %d, want 1", trace.AgentStepIndex)
		}
		if trace.ToolCallID != "call-search-docs" {
			t.Fatalf("ToolCallID = %q, want call-search-docs", trace.ToolCallID)
		}
		if trace.ToolName != "search_docs" {
			t.Fatalf("ToolName = %q, want search_docs", trace.ToolName)
		}
		if trace.TerminationReason != terminatedFinished {
			t.Fatalf("TerminationReason = %q, want %q", trace.TerminationReason, terminatedFinished)
		}
		if trace.OutcomeStatus != "success" {
			t.Fatalf("OutcomeStatus = %q, want success", trace.OutcomeStatus)
		}
		if trace.TotalLatencyMs != 25 {
			t.Fatalf("TotalLatencyMs = %d, want 25", trace.TotalLatencyMs)
		}
		if trace.ToolSummary.Count != 1 || trace.ToolSummary.Status != "success" {
			t.Fatalf("ToolSummary = %#v, want count=1 status=success", trace.ToolSummary)
		}
	})
}

func TestExecutorRecordsLoopAndBudgetTerminationObservations(t *testing.T) {
	tests := []struct {
		name                  string
		provider              *scriptedProvider
		limit                 Limit
		wantTerminatedBy      string
		wantTraceCount        int
		wantLastStepIndex     int
		wantLastToolCallID    string
		wantLoopDetected      bool
		wantBudgetExceeded    bool
		wantLastOutcomeStatus string
	}{
		{
			name: "loop detected records attempted repeated tool call",
			provider: newScriptedProvider(
				toolCallResponse("call-loop-1", "search_docs", map[string]any{"query": "same"}),
				toolCallResponse("call-loop-2", "search_docs", map[string]any{"query": "same"}),
			),
			limit:                 Limit{MaxSteps: 10},
			wantTerminatedBy:      terminatedLoopDetected,
			wantTraceCount:        2,
			wantLastStepIndex:     2,
			wantLastToolCallID:    "call-loop-2",
			wantLoopDetected:      true,
			wantLastOutcomeStatus: "failure",
		},
		{
			name: "budget exceeded records limit status before tool invocation",
			provider: newScriptedProvider(llm.ChatResponse{
				Usage:        llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 2, OutputTokens: 4, TotalTokens: 6}),
				FinishReason: llm.FinishToolCall,
				ToolCalls: []llm.ToolCall{
					{ID: "call-budget", Name: "search_docs", Arguments: map[string]any{"query": "budget"}},
				},
			}),
			limit:                 Limit{MaxSteps: 3, TokenBudget: 5},
			wantTerminatedBy:      terminatedBudgetExceeded,
			wantTraceCount:        1,
			wantLastStepIndex:     0,
			wantBudgetExceeded:    true,
			wantLastOutcomeStatus: "failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := obs.NewCorrelationIdentity(
				"req-"+tt.wantTerminatedBy,
				obs.WithServiceSpan("svc-trace-"+tt.wantTerminatedBy, "span-"+tt.wantTerminatedBy),
				obs.WithAITraceID("ai-trace-"+tt.wantTerminatedBy),
			)
			recorder := testutil.NewRecorder()
			registry := registryWithTool(t, newLimitTool("search_docs"))
			executor := NewExecutor(
				tt.provider,
				registry,
				WithTracer(recorder),
				WithFeature("agent_loop"),
				WithNow(scriptedAgentObservationClock(
					time.Date(2026, time.July, 7, 9, 1, 0, 0, time.UTC),
					time.Date(2026, time.July, 7, 9, 1, 0, 10*int(time.Millisecond), time.UTC),
					time.Date(2026, time.July, 7, 9, 1, 0, 20*int(time.Millisecond), time.UTC),
				)),
			)
			ctx := obs.ContextWithCorrelationIdentity(context.Background(), identity)

			result, err := executor.Run(ctx, Request{
				Query: "observe termination",
				Model: "tool-model",
				Limit: tt.limit,
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.TerminatedBy != tt.wantTerminatedBy {
				t.Fatalf("TerminatedBy = %q, want %q", result.TerminatedBy, tt.wantTerminatedBy)
			}

			recorder.AssertCount(t, tt.wantTraceCount)
			recorder.AssertTrace(t, tt.wantTraceCount-1, func(t *testing.T, trace obs.Trace) {
				t.Helper()

				assertAgentTraceIdentity(t, trace, identity)
				if trace.AgentStepIndex != tt.wantLastStepIndex {
					t.Fatalf("AgentStepIndex = %d, want %d", trace.AgentStepIndex, tt.wantLastStepIndex)
				}
				if trace.ToolCallID != tt.wantLastToolCallID {
					t.Fatalf("ToolCallID = %q, want %q", trace.ToolCallID, tt.wantLastToolCallID)
				}
				if trace.TerminationReason != tt.wantTerminatedBy {
					t.Fatalf("TerminationReason = %q, want %q", trace.TerminationReason, tt.wantTerminatedBy)
				}
				if trace.LoopDetected != tt.wantLoopDetected {
					t.Fatalf("LoopDetected = %v, want %v", trace.LoopDetected, tt.wantLoopDetected)
				}
				if trace.BudgetExceeded != tt.wantBudgetExceeded {
					t.Fatalf("BudgetExceeded = %v, want %v", trace.BudgetExceeded, tt.wantBudgetExceeded)
				}
				if trace.OutcomeStatus != tt.wantLastOutcomeStatus {
					t.Fatalf("OutcomeStatus = %q, want %q", trace.OutcomeStatus, tt.wantLastOutcomeStatus)
				}
				if trace.ToolSummary.Status != tt.wantTerminatedBy {
					t.Fatalf("ToolSummary.Status = %q, want %q", trace.ToolSummary.Status, tt.wantTerminatedBy)
				}
			})
		})
	}
}

func assertAgentTraceIdentity(t *testing.T, trace obs.Trace, identity obs.CorrelationIdentity) {
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
	if trace.ObservationType != obs.ObservationTypeAgent {
		t.Fatalf("ObservationType = %q, want %q", trace.ObservationType, obs.ObservationTypeAgent)
	}
	if trace.Feature != "agent_loop" {
		t.Fatalf("Feature = %q, want agent_loop", trace.Feature)
	}
}

func scriptedAgentObservationClock(times ...time.Time) func() time.Time {
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
