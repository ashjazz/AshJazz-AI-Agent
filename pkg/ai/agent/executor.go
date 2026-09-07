package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
)

const (
	defaultMaxSteps     = 10
	defaultStepTimeoutS = 30

	terminatedFinished       = "finished"
	terminatedMaxSteps       = "max_steps"
	terminatedLoopDetected   = "loop_detected"
	terminatedStepTimeout    = "step_timeout"
	terminatedBudgetExceeded = "budget_exceeded"
)

// NativeExecutor 是最小 native tool calling 执行器。
//
// 它只消费 provider 返回的结构化 llm.ToolCall，不解析 ReAct 时代的 Thought/Action 文本。
// 这样做牺牲了一些“看起来什么文本都能跑”的便利，但换来工具参数、trace、评估和权限控制都能
// 依赖稳定结构，而不是依赖脆弱的自然语言解析。
type NativeExecutor struct {
	provider llm.Provider
	registry *Registry
	tracer   obs.Tracer
	feature  string
	now      func() time.Time
}

// ExecutorOption 描述 Agent executor 的可选装配项。
type ExecutorOption func(*NativeExecutor)

// NewExecutor 创建 Agent executor。
func NewExecutor(provider llm.Provider, registry *Registry, options ...ExecutorOption) *NativeExecutor {
	executor := &NativeExecutor{
		provider: provider,
		registry: registry,
		now:      time.Now,
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		option(executor)
	}
	if executor.now == nil {
		executor.now = time.Now
	}
	return executor
}

// WithTracer 配置 Agent 语义观测出口。
func WithTracer(tracer obs.Tracer) ExecutorOption {
	return func(executor *NativeExecutor) {
		executor.tracer = tracer
	}
}

// WithFeature 配置 Agent trace 的功能维度。
func WithFeature(feature string) ExecutorOption {
	return func(executor *NativeExecutor) {
		executor.feature = feature
	}
}

// WithNow 注入时钟，便于契约测试稳定断言延迟。
func WithNow(now func() time.Time) ExecutorOption {
	return func(executor *NativeExecutor) {
		executor.now = now
	}
}

// Run 执行 native tool calling 循环，直到拿到最终答案或命中安全边界。
func (e *NativeExecutor) Run(ctx context.Context, req Request) (Result, error) {
	if err := validateExecutor(e); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	limit := normalizeLimit(req.Limit)
	runStartedAt := e.now()
	messages := []llm.Message{{Role: llm.RoleUser, Content: req.Query}}
	result := Result{}
	var lastCall *llm.ToolCall
	var pendingStep *StepResult

	for {
		response, err := e.provider.Chat(ctx, &llm.ChatRequest{
			Model:    req.Model,
			Messages: messages,
			Tools:    e.registry.LLMTools(),
		})
		if err != nil {
			return result, fmt.Errorf("agent provider chat: %w", err)
		}

		if response == nil {
			return result, llm.ErrInvalidResponse
		}
		usage, reported := response.Usage.Summary()
		if !reported {
			return result, llm.ErrInvalidResponse
		}
		result.TokensUsed += usage.TotalTokens
		if exceededTokenBudget(result.TokensUsed, limit.TokenBudget) {
			result.TerminatedBy = terminatedBudgetExceeded
			e.recordAgentObservation(ctx, agentObservation{
				StepIndex:         result.StepsTaken,
				TerminationReason: terminatedBudgetExceeded,
				OutcomeStatus:     "failure",
				FailureStatus:     string(obs.FailureBudgetExceeded),
				BudgetExceeded:    true,
				StartedAt:         runStartedAt,
				EndedAt:           e.now(),
			})
			return result, nil
		}

		if response.FinishReason != llm.FinishToolCall {
			result.Answer = response.Content
			result.TerminatedBy = terminatedFinished
			if pendingStep != nil {
				e.recordAgentStepObservation(ctx, *pendingStep, result.StepsTaken, terminatedFinished, "success", "success", "", false, false, runStartedAt, e.now())
			}
			return result, nil
		}
		if len(response.ToolCalls) == 0 {
			return result, fmt.Errorf("agent provider returned finish_reason=%q without tool calls", response.FinishReason)
		}

		for _, toolCall := range response.ToolCalls {
			if lastCall != nil && sameToolCall(*lastCall, toolCall) {
				result.TerminatedBy = terminatedLoopDetected
				if pendingStep != nil {
					e.recordAgentStepObservation(ctx, *pendingStep, result.StepsTaken, "success", "success", "success", "", false, false, runStartedAt, e.now())
					pendingStep = nil
				}
				e.recordAgentToolCallTermination(ctx, toolCall, result.StepsTaken+1, terminatedLoopDetected, terminatedLoopDetected, "failure", string(obs.FailureLoopDetected), true, false, runStartedAt, e.now())
				return result, nil
			}
			if pendingStep != nil {
				e.recordAgentStepObservation(ctx, *pendingStep, result.StepsTaken, "success", "success", "success", "", false, false, runStartedAt, e.now())
				pendingStep = nil
			}

			step, terminated, err := e.invokeTool(ctx, toolCall, limit)
			if err != nil {
				return result, err
			}
			result.Steps = append(result.Steps, step)
			result.StepsTaken = len(result.Steps)
			if terminated != "" {
				result.TerminatedBy = terminated
				e.recordAgentStepObservation(ctx, step, result.StepsTaken, terminated, terminated, "failure", failureStatusForTermination(terminated), false, terminated == terminatedBudgetExceeded, runStartedAt, e.now())
				return result, nil
			}
			if result.StepsTaken >= limit.MaxSteps {
				result.TerminatedBy = terminatedMaxSteps
				e.recordAgentStepObservation(ctx, step, result.StepsTaken, terminatedMaxSteps, terminatedMaxSteps, "failure", "", false, false, runStartedAt, e.now())
				return result, nil
			}

			callCopy := cloneToolCall(toolCall)
			lastCall = &callCopy
			stepCopy := cloneStepResult(step)
			pendingStep = &stepCopy
			messages = append(messages, toolResultMessage(step.Result))
		}
	}
}

func (e *NativeExecutor) invokeTool(ctx context.Context, toolCall llm.ToolCall, limit Limit) (StepResult, string, error) {
	tool, err := e.registry.Get(toolCall.Name)
	if err != nil {
		return StepResult{}, "", err
	}

	stepCtx, cancel := context.WithTimeout(ctx, time.Duration(limit.StepTimeoutS)*time.Second)
	defer cancel()

	startedAt := time.Now()
	content, err := tool.Invoke(stepCtx, cloneMap(toolCall.Arguments))
	step := StepResult{
		ToolCall: cloneToolCall(toolCall),
		Result: llm.ToolResult{
			ToolCallID: toolCall.ID,
			Name:       toolCall.Name,
			Content:    content,
			IsError:    err != nil,
		},
		LatencyMs: time.Since(startedAt).Milliseconds(),
	}
	if err == nil {
		return step, "", nil
	}
	if stepCtx.Err() != nil {
		return step, terminatedStepTimeout, nil
	}
	return step, "", fmt.Errorf("invoke tool %q: %w", toolCall.Name, err)
}

func validateExecutor(executor *NativeExecutor) error {
	if executor == nil {
		return fmt.Errorf("agent executor is required")
	}
	if executor.provider == nil {
		return fmt.Errorf("agent executor provider is required")
	}
	if executor.registry == nil {
		return fmt.Errorf("agent executor registry is required")
	}
	return nil
}

func normalizeLimit(limit Limit) Limit {
	if limit.MaxSteps <= 0 {
		limit.MaxSteps = defaultMaxSteps
	}
	if limit.StepTimeoutS <= 0 {
		limit.StepTimeoutS = defaultStepTimeoutS
	}
	return limit
}

func exceededTokenBudget(tokensUsed int, tokenBudget int) bool {
	return tokenBudget > 0 && tokensUsed > tokenBudget
}

func sameToolCall(previous llm.ToolCall, current llm.ToolCall) bool {
	return previous.Name == current.Name && reflect.DeepEqual(previous.Arguments, current.Arguments)
}

func toolResultMessage(result llm.ToolResult) llm.Message {
	return llm.Message{
		Role:       llm.RoleTool,
		Content:    result.Content,
		Name:       result.Name,
		ToolCallID: result.ToolCallID,
	}
}

func cloneToolCall(source llm.ToolCall) llm.ToolCall {
	return llm.ToolCall{
		ID:        source.ID,
		Name:      source.Name,
		Arguments: cloneMap(source.Arguments),
	}
}

type agentObservation struct {
	StepIndex         int
	ToolCallID        string
	ToolName          string
	TerminationReason string
	ToolSummaryStatus string
	OutcomeStatus     string
	FailureStatus     string
	LoopDetected      bool
	BudgetExceeded    bool
	StartedAt         time.Time
	EndedAt           time.Time
}

func (e *NativeExecutor) recordAgentStepObservation(ctx context.Context, step StepResult, stepIndex int, terminationReason, toolSummaryStatus, outcomeStatus, failureStatus string, loopDetected, budgetExceeded bool, startedAt, endedAt time.Time) {
	e.recordAgentObservation(ctx, agentObservation{
		StepIndex:         stepIndex,
		ToolCallID:        step.ToolCall.ID,
		ToolName:          step.ToolCall.Name,
		TerminationReason: terminationReason,
		ToolSummaryStatus: toolSummaryStatus,
		OutcomeStatus:     outcomeStatus,
		FailureStatus:     failureStatus,
		LoopDetected:      loopDetected,
		BudgetExceeded:    budgetExceeded,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
	})
}

func (e *NativeExecutor) recordAgentToolCallTermination(ctx context.Context, toolCall llm.ToolCall, stepIndex int, terminationReason, toolSummaryStatus, outcomeStatus, failureStatus string, loopDetected, budgetExceeded bool, startedAt, endedAt time.Time) {
	e.recordAgentObservation(ctx, agentObservation{
		StepIndex:         stepIndex,
		ToolCallID:        toolCall.ID,
		ToolName:          toolCall.Name,
		TerminationReason: terminationReason,
		ToolSummaryStatus: toolSummaryStatus,
		OutcomeStatus:     outcomeStatus,
		FailureStatus:     failureStatus,
		LoopDetected:      loopDetected,
		BudgetExceeded:    budgetExceeded,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
	})
}

func (e *NativeExecutor) recordAgentObservation(ctx context.Context, observation agentObservation) {
	if e == nil || e.tracer == nil {
		return
	}
	if strings.TrimSpace(e.feature) == "" {
		return
	}

	identity, ok := obs.CorrelationIdentityFromContext(ctx)
	if !ok || strings.TrimSpace(identity.AITraceID) == "" {
		return
	}

	trace := obs.NewTrace(
		identity.AITraceID,
		e.feature,
		observation.EndedAt,
		obs.WithCorrelationIdentity(identity),
		obs.WithObservationType(obs.ObservationTypeAgent),
		obs.WithLatency(0, observation.EndedAt.Sub(observation.StartedAt).Milliseconds()),
		obs.WithSafeSummaries(
			obs.SafeSummary{},
			obs.SafeSummary{},
			obs.SafeSummary{},
			obs.NewSafeSummary(
				obs.WithSummaryCategory(observation.ToolName),
				obs.WithSummaryCount(toolSummaryCount(observation.ToolName)),
				obs.WithSummaryStatus(toolSummaryStatus(observation)),
				obs.WithSummaryErrorClass(observation.FailureStatus),
			),
		),
		obs.WithOutcome(observation.OutcomeStatus),
	)
	trace.FailureStatus = observation.FailureStatus
	trace.AgentStepIndex = observation.StepIndex
	trace.ToolCallID = observation.ToolCallID
	trace.ToolName = observation.ToolName
	trace.TerminationReason = observation.TerminationReason
	trace.LoopDetected = observation.LoopDetected
	trace.BudgetExceeded = observation.BudgetExceeded

	_ = obs.RecordWithExportFailureProtection(ctx, e.tracer, trace)
}

func toolSummaryStatus(observation agentObservation) string {
	if observation.ToolSummaryStatus != "" {
		return observation.ToolSummaryStatus
	}
	return observation.TerminationReason
}

func toolSummaryCount(toolName string) int {
	if strings.TrimSpace(toolName) == "" {
		return 0
	}
	return 1
}

func failureStatusForTermination(termination string) string {
	switch termination {
	case terminatedLoopDetected:
		return string(obs.FailureLoopDetected)
	case terminatedBudgetExceeded:
		return string(obs.FailureBudgetExceeded)
	case terminatedStepTimeout:
		return string(obs.FailureTimeout)
	default:
		return ""
	}
}

func cloneStepResult(source StepResult) StepResult {
	return StepResult{
		ToolCall: cloneToolCall(source.ToolCall),
		Result: llm.ToolResult{
			ToolCallID: source.Result.ToolCallID,
			Name:       source.Result.Name,
			Content:    source.Result.Content,
			IsError:    source.Result.IsError,
		},
		LatencyMs: source.LatencyMs,
	}
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}

	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneMapValue(value)
	}
	return cloned
}

func cloneMapValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneMapValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return typed
	}
}
