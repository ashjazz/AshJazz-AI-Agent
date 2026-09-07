package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	llmtestutil "github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
)

// 没有 usage 事实时不能继续累计预算或执行工具；fake/未来 adapter 也必须
// 在这个边界稳定失败，而不是把零值当成免费调用或触发 nil pointer panic。
func TestExecutorRejectsUnavailableUsage(t *testing.T) {
	for _, usage := range []llm.ProviderUsage{{}, llm.NewUnavailableProviderUsage()} {
		tool := newLimitTool("search_docs")
		executor := NewExecutor(newScriptedProvider(llm.ChatResponse{
			Usage: usage, Content: "untrusted answer", FinishReason: llm.FinishStop,
		}), registryWithTool(t, tool))
		result, err := executor.Run(context.Background(), Request{Query: "question", Model: "tool-model"})
		if !errors.Is(err, llm.ErrInvalidResponse) || result.Answer != "" || result.TokensUsed != 0 || tool.Invocations() != 0 {
			t.Fatalf("Run() = %#v, %v, want no success facts and invalid response", result, err)
		}
	}
}

func TestExecutorStopsAtMaxSteps(t *testing.T) {
	t.Parallel()

	registry := registryWithTool(t, newLimitTool("search_docs"))
	provider := newScriptedProvider(repeatedToolCallResponse("search_docs", 5)...)
	executor := NewExecutor(provider, registry)

	result, err := executor.Run(context.Background(), Request{
		Query: "keep searching",
		Model: "tool-model",
		Limit: Limit{MaxSteps: 2},
	})

	assertExecutorResult(t, result, err, "max_steps", 2)
	if provider.CallCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.CallCount())
	}
}

func TestExecutorRunsToolCallingLoopUntilFinalAnswer(t *testing.T) {
	t.Parallel()

	tool := newLimitTool("search_docs")
	registry := registryWithTool(t, tool)
	provider := newScriptedProvider(
		toolCallResponse("call-search", "search_docs", map[string]any{"query": "agent harness"}),
		llm.ChatResponse{
			Content:      "Agent harness uses native tool calling.",
			Usage:        llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}),
			FinishReason: llm.FinishStop,
		},
	)
	executor := NewExecutor(provider, registry)

	result, err := executor.Run(context.Background(), Request{
		Query: "what does the harness use?",
		Model: "tool-model",
		Limit: Limit{MaxSteps: 3},
	})

	assertExecutorResult(t, result, err, "finished", 1)
	if result.Answer != "Agent harness uses native tool calling." {
		t.Fatalf("Answer = %q, want final provider content", result.Answer)
	}
	if result.TokensUsed != 4 {
		t.Fatalf("TokensUsed = %d, want accumulated usage 4", result.TokensUsed)
	}
	if len(result.Steps) != 1 || result.Steps[0].Result.Content != "ok" {
		t.Fatalf("Steps = %#v, want one successful tool result", result.Steps)
	}
	if provider.CallCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.CallCount())
	}
}

func TestExecutorStopsOnLoopDetected(t *testing.T) {
	t.Parallel()

	registry := registryWithTool(t, newLimitTool("search_docs"))
	provider := newScriptedProvider(
		toolCallResponse("call-1", "search_docs", map[string]any{"query": "same"}),
		toolCallResponse("call-2", "search_docs", map[string]any{"query": "same"}),
	)
	executor := NewExecutor(provider, registry)

	result, err := executor.Run(context.Background(), Request{
		Query: "loop forever",
		Model: "tool-model",
		Limit: Limit{MaxSteps: 10},
	})

	assertExecutorResult(t, result, err, "loop_detected", 1)
	if provider.CallCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.CallCount())
	}
}

func TestExecutorCancelsToolAtStepTimeout(t *testing.T) {
	t.Parallel()

	toolStarted := make(chan struct{})
	registry := registryWithTool(t, &blockingLimitTool{name: "slow_tool", started: toolStarted})
	provider := newScriptedProvider(toolCallResponse("call-slow", "slow_tool", map[string]any{"query": "wait"}))
	executor := NewExecutor(provider, registry)

	startedAt := time.Now()
	result, err := executor.Run(context.Background(), Request{
		Query: "run slow tool",
		Model: "tool-model",
		Limit: Limit{MaxSteps: 3, StepTimeoutS: 1},
	})

	assertExecutorResult(t, result, err, "step_timeout", 1)
	select {
	case <-toolStarted:
	default:
		t.Fatal("tool was not invoked before step timeout")
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("Run() elapsed = %s, want tool canceled near configured step timeout", elapsed)
	}
}

func TestExecutorStopsWhenTokenBudgetExceeded(t *testing.T) {
	t.Parallel()

	tool := newLimitTool("search_docs")
	registry := registryWithTool(t, tool)
	provider := newScriptedProvider(llm.ChatResponse{
		Usage:        llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 2, OutputTokens: 4, TotalTokens: 6}),
		FinishReason: llm.FinishToolCall,
		ToolCalls: []llm.ToolCall{
			{ID: "call-budget", Name: "search_docs", Arguments: map[string]any{"query": "budget"}},
		},
	})
	executor := NewExecutor(provider, registry)

	result, err := executor.Run(context.Background(), Request{
		Query: "use too many tokens",
		Model: "tool-model",
		Limit: Limit{MaxSteps: 3, TokenBudget: 5},
	})

	assertExecutorResult(t, result, err, "budget_exceeded", 0)
	if tool.Invocations() != 0 {
		t.Fatalf("tool invocations = %d, want 0 after budget exceeded", tool.Invocations())
	}
	if result.TokensUsed != 6 {
		t.Fatalf("TokensUsed = %d, want 6", result.TokensUsed)
	}
}

func registryWithTool(t *testing.T, tool Tool) *Registry {
	t.Helper()

	registry := NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return registry
}

func assertExecutorResult(t *testing.T, result Result, err error, wantTerminatedBy string, wantSteps int) {
	t.Helper()

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.TerminatedBy != wantTerminatedBy {
		t.Fatalf("TerminatedBy = %q, want %q", result.TerminatedBy, wantTerminatedBy)
	}
	if result.StepsTaken != wantSteps {
		t.Fatalf("StepsTaken = %d, want %d", result.StepsTaken, wantSteps)
	}
}

type scriptedProvider struct {
	mu        sync.Mutex
	responses []llm.ChatResponse
	calls     int
}

func newScriptedProvider(responses ...llm.ChatResponse) *scriptedProvider {
	return &scriptedProvider{responses: responses}
}

func (p *scriptedProvider) Name() string {
	return "scripted"
}

func (p *scriptedProvider) Capabilities(model string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{ToolCalling: true}
}

func (p *scriptedProvider) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls >= len(p.responses) {
		return nil, fmt.Errorf("scripted provider response exhausted at call %d", p.calls+1)
	}

	response := p.responses[p.calls]
	p.calls++
	return &response, nil
}

func (p *scriptedProvider) ChatStream(ctx context.Context, req *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	return nil, fmt.Errorf("scripted provider stream is not used by executor limit tests")
}

func (p *scriptedProvider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func repeatedToolCallResponse(toolName string, count int) []llm.ChatResponse {
	responses := make([]llm.ChatResponse, count)
	for index := range responses {
		responses[index] = toolCallResponse(
			fmt.Sprintf("call-%d", index+1),
			toolName,
			map[string]any{"query": fmt.Sprintf("query-%d", index+1)},
		)
	}
	return responses
}

func toolCallResponse(id string, name string, args map[string]any) llm.ChatResponse {
	return llm.ChatResponse{
		Usage:        llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 1, TotalTokens: 1}),
		FinishReason: llm.FinishToolCall,
		ToolCalls: []llm.ToolCall{
			{ID: id, Name: name, Arguments: args},
		},
	}
}

type limitTool struct {
	mu          sync.Mutex
	name        string
	invocations int
}

func newLimitTool(name string) *limitTool {
	return &limitTool{name: name}
}

func (t *limitTool) Name() string {
	return t.name
}

func (*limitTool) Description() string {
	return "A deterministic test tool for executor limit tests."
}

func (*limitTool) Parameters() map[string]any {
	return limitToolSchema()
}

func (t *limitTool) Invoke(ctx context.Context, args map[string]any) (string, error) {
	t.mu.Lock()
	t.invocations++
	t.mu.Unlock()
	return "ok", nil
}

func (t *limitTool) Invocations() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.invocations
}

type blockingLimitTool struct {
	name    string
	started chan<- struct{}
	once    sync.Once
}

func (t *blockingLimitTool) Name() string {
	return t.name
}

func (*blockingLimitTool) Description() string {
	return "A test tool that blocks until its step context is canceled."
}

func (*blockingLimitTool) Parameters() map[string]any {
	return limitToolSchema()
}

func (t *blockingLimitTool) Invoke(ctx context.Context, args map[string]any) (string, error) {
	t.once.Do(func() {
		close(t.started)
	})
	<-ctx.Done()
	return "", ctx.Err()
}

func limitToolSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
}
