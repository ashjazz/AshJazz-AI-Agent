// Package chat 编排非流式聊天用例。
//
// HTTP 校验属于 controller，provider 协议映射属于 pkg/ai/llm adapter；本包只在
// AI 用例真正开始时创建领域身份，并把模型事实投影为业务结果和低敏观测事实。
package chat

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	appobs "github.com/ashjazz/Longtermism/internal/observability"
	obssmoke "github.com/ashjazz/Longtermism/internal/observability/smoke"
	aieval "github.com/ashjazz/Longtermism/pkg/ai/eval"
	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
	traceapi "go.opentelemetry.io/otel/trace"
)

const (
	chatFeature                  = "chat"
	chatTelemetryComponent       = "generation"
	chatTelemetryFailureClass    = "telemetry_export_failed"
	chatSideEffectFailureClass   = "side_effect_failed"
	chatBridgeComponent          = "chat_bridge"
	chatGenerationComponent      = "generation_span"
	chatEvaluatorComponent       = "evaluator"
	chatEvaluatorSpanComponent   = "evaluator_span"
	chatEvidenceStoreComponent   = "evidence_store"
	chatScoreProjectionComponent = "score_projection"
	chatSuccessOutcome           = "success"
	chatFailedOutcome            = "failed"
	maxModelIdentifierBytes      = 128
	maxFinishReasonBytes         = 64
	maxAITraceIDBytes            = 128
	maxChatResponseBytes         = 1024 * 1024
	maxUsageTokens               = 100_000_000
)

var (
	safeModelIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
	safeAITraceIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

	// ErrChatConfiguration 表示 composition root 没有提供用例运行所需的服务端依赖。
	// 错误不包含配置值，避免模型名、endpoint 或 credential 进入 HTTP 错误链。
	ErrChatConfiguration = errors.New("chat: usecase is not configured")

	// ErrChatProviderFailure 表示 provider 返回了未归类错误。原始错误可能包含响应正文
	// 或凭据，因此 usecase 只返回稳定低敏 sentinel，不包装外部错误。
	ErrChatProviderFailure = errors.New("chat: provider call failed")

	// ErrChatInvalidResponse 表示 provider 违反了 (response, error) 契约。
	ErrChatInvalidResponse = errors.New("chat: provider returned an invalid response")

	// ErrChatInvalidContext 表示调用方违反 Go context 契约。
	ErrChatInvalidContext = errors.New("chat: context is required")
)

// ChatCommand 是 controller 交给应用层的最小命令。provider、模型和 debug 策略均由
// 服务端装配，客户端不能通过该对象改变运行策略。
type ChatCommand struct {
	Message    string
	SmokeRunID string
}

type ChatRunManifestInput = obssmoke.ChatRunManifestInput
type ChatRunManifestWriter = obssmoke.ChatRunManifestWriter

func SmokeRunIDFromContext(ctx context.Context) string {
	return appobs.ChatSmokeRunIDFromContext(ctx)
}

// ChatResult 保留业务响应与关联身份。即使 provider 失败，Identity 也会返回，使
// controller 能在 502/504 响应中保留已经启动的 AI 链路身份。
type ChatResult struct {
	Content      string
	Model        string
	FinishReason llm.FinishReason
	Usage        llm.Usage
	Identity     obs.CorrelationIdentity
	EvalSummary  *DebugEvalSummary
}

// ChatProvider 是 usecase 实际消费的窄端口。流式能力、provider 配置和 protocol
// adapter 不应泄漏到当前非流式应用流程。
type ChatProvider interface {
	Chat(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error)
}

// ChatTelemetry 把低敏领域事实放入有界观测队列。
//
// TryRecord 必须同步且立即返回：实现只能做 non-blocking enqueue，队列满时返回低敏
// 错误并由 worker 异步投递。usecase 不自行启动 goroutine，避免无界并发和 shutdown
// 丢失。返回错误只用于诊断旁路，绝不能改写已经确定的模型业务结果。
type ChatTelemetry interface {
	TryRecord(obs.Trace) error
}

// ChatTelemetryFailure 是可安全写入日志/指标的旁路故障事实。
type ChatTelemetryFailure struct {
	Component  string
	ErrorClass string
}

// ChatTelemetryDiagnostics 记录观测旁路失败，不接收原始 error，防止 provider body、
// endpoint 或 credential 被二次写入日志。实现必须是 non-blocking 本地计数或有界入队。
type ChatTelemetryDiagnostics interface {
	TryRecordTelemetryFailure(ChatTelemetryFailure)
}

// CanonicalizeActualModel 把 provider 原始 model 字段映射为服务端允许公开和导出的稳定
// 标识。raw 值不在 allowlist 时返回 false，usecase 不把它写入结果、错误或 telemetry。
type CanonicalizeActualModel func(raw string) (canonical string, ok bool)

// ChatUsecaseDependencies 由 composition root 注入。依赖在构造后只读，因此 usecase
// 本身没有跨请求共享的可变状态；并发安全由注入端口各自保证。
type ChatUsecaseDependencies struct {
	Provider                ChatProvider
	RequestedModel          string
	ProviderName            string
	PromptTemplateVersion   string
	PromptHash              string
	PayloadMode             obs.PayloadMode
	PayloadRedacted         bool
	NewAITraceID            func() string
	NewEvalRunID            func() string
	CanonicalizeActualModel CanonicalizeActualModel
	Bridge                  ChatAIExecutionBoundary
	GenerationObserver      ChatGenerationObserver
	Evaluator               Evaluator[CompletionContractEvaluationInput, CompletionContractEvaluationResult]
	EvaluatorObserver       ChatEvaluatorObserver
	EvidenceStore           ChatEvidenceStore
	ProjectionQueue         ChatScoreProjectionQueue
	Telemetry               ChatTelemetry
	Diagnostics             ChatTelemetryDiagnostics
	RunManifestWriter       ChatRunManifestWriter
	AIPlaneFacts            AIPlaneFactRecorder
	Now                     func() time.Time
}

// ChatUsecase 编排一次非流式模型调用。
type ChatUsecase struct {
	provider                ChatProvider
	requestedModel          string
	providerName            string
	promptTemplateVersion   string
	promptHash              string
	payloadMode             obs.PayloadMode
	payloadRedacted         bool
	newAITraceID            func() string
	newEvalRunID            func() string
	canonicalizeActualModel CanonicalizeActualModel
	bridge                  ChatAIExecutionBoundary
	generationObserver      ChatGenerationObserver
	evaluator               Evaluator[CompletionContractEvaluationInput, CompletionContractEvaluationResult]
	isEvaluatorConfigured   bool
	evaluatorObserver       ChatEvaluatorObserver
	evidenceStore           ChatEvidenceStore
	projectionQueue         ChatScoreProjectionQueue
	telemetry               ChatTelemetry
	diagnostics             ChatTelemetryDiagnostics
	runManifestWriter       ChatRunManifestWriter
	aiPlaneFacts            AIPlaneFactRecorder
	now                     func() time.Time
}

type chatProviderExecution struct {
	response           *llm.ChatResponse
	usage              llm.Usage
	canonicalModel     string
	generationIdentity appobs.PlatformSpanIdentity
	failureStatus      obs.FailureStatus
}

type chatEvaluationExecution struct {
	context     context.Context
	identity    obs.CorrelationIdentity
	result      CompletionContractEvaluationResult
	startedAt   time.Time
	completedAt time.Time
}

// NewChatUsecase 创建只读 usecase。依赖校验留在 Execute，使当前构造签名保持简单，
// 同时保证错误路径仍能返回稳定、低敏且不会 panic 的结果。
func NewChatUsecase(dependencies ChatUsecaseDependencies) *ChatUsecase {
	now := dependencies.Now
	if now == nil {
		now = time.Now
	}
	evaluator := dependencies.Evaluator
	isEvaluatorConfigured := evaluator != nil
	if evaluator == nil {
		evaluator = NewCompletionContractNotRunEvaluator()
	}
	return &ChatUsecase{
		provider:                dependencies.Provider,
		requestedModel:          dependencies.RequestedModel,
		providerName:            dependencies.ProviderName,
		promptTemplateVersion:   dependencies.PromptTemplateVersion,
		promptHash:              dependencies.PromptHash,
		payloadMode:             dependencies.PayloadMode,
		payloadRedacted:         dependencies.PayloadRedacted,
		newAITraceID:            dependencies.NewAITraceID,
		newEvalRunID:            dependencies.NewEvalRunID,
		canonicalizeActualModel: dependencies.CanonicalizeActualModel,
		bridge:                  dependencies.Bridge,
		generationObserver:      dependencies.GenerationObserver,
		evaluator:               evaluator,
		isEvaluatorConfigured:   isEvaluatorConfigured,
		evaluatorObserver:       dependencies.EvaluatorObserver,
		evidenceStore:           dependencies.EvidenceStore,
		projectionQueue:         dependencies.ProjectionQueue,
		telemetry:               dependencies.Telemetry,
		diagnostics:             dependencies.Diagnostics,
		runManifestWriter:       dependencies.RunManifestWriter,
		aiPlaneFacts:            dependencies.AIPlaneFacts,
		now:                     now,
	}
}

// Execute 按“AI identity -> root/bridge -> provider -> semantic spans -> evaluator
// -> local evidence -> non-blocking projection”执行。除 provider/响应契约外的所有
// 观测与评估端口都是旁路，失败只能留下低敏诊断，不能覆盖模型业务结果。
func (usecase *ChatUsecase) Execute(ctx context.Context, command ChatCommand) (ChatResult, error) {
	if ctx == nil {
		return ChatResult{}, ErrChatInvalidContext
	}
	trustedSmokeRunID := appobs.ChatSmokeRunIDFromContext(ctx)
	if command.SmokeRunID != trustedSmokeRunID {
		return ChatResult{}, ErrChatConfiguration
	}
	identity, identityContext, err := usecase.startAIExecution(ctx)
	result := ChatResult{Identity: identity}
	if err != nil {
		return result, err
	}
	if usecase.provider == nil ||
		usecase.canonicalizeActualModel == nil ||
		!isSafeModelIdentifier(usecase.requestedModel) {
		return result, ErrChatConfiguration
	}

	executionContext, executionIdentity, endExecution, hasTrustedExecution := usecase.startChatBoundary(identityContext, identity)
	result.Identity = executionIdentity
	usecase.recordAIPlaneFact(command.SmokeRunID, hasTrustedExecution)
	executionOutcome := appobs.ChatAIExecutionOutcome{
		Outcome:       chatFailedOutcome,
		FailureStatus: obs.FailureUpstream,
	}
	defer func() {
		usecase.endChatBoundary(endExecution, executionOutcome)
	}()
	execution, providerErr := usecase.executeProvider(
		executionContext,
		executionIdentity,
		command,
		hasTrustedExecution,
	)
	if providerErr != nil {
		executionOutcome.FailureStatus = execution.failureStatus
		return result, providerErr
	}
	result = ChatResult{
		Content:      execution.response.Content,
		Model:        execution.canonicalModel,
		FinishReason: execution.response.FinishReason,
		Usage:        execution.usage,
		Identity:     executionIdentity,
	}
	usecase.writeRunManifest(executionContext, command.SmokeRunID, executionIdentity)
	result.EvalSummary = usecase.evaluateAndPersist(
		executionContext,
		executionIdentity,
		execution.canonicalModel,
		execution.response,
		execution.usage,
		execution.generationIdentity,
		hasTrustedExecution,
	)
	executionOutcome = appobs.ChatAIExecutionOutcome{Outcome: chatSuccessOutcome}
	return result, nil
}

// recordAIPlaneFact 只在 AI 桥接 span 真实创建后登记一次 AI 平面发射事实，供 infra
// smoke 的 AI-negative marker-count 查询读取。登记是旁路：未注入 recorder、无 marker
// 或桥接失败时静默跳过，绝不改写业务结果。
func (usecase *ChatUsecase) recordAIPlaneFact(marker string, hasTrustedExecution bool) {
	if usecase == nil || usecase.aiPlaneFacts == nil || marker == "" || !hasTrustedExecution {
		return
	}
	usecase.aiPlaneFacts.RecordAIPlaneFact(marker, usecase.now())
}

func (usecase *ChatUsecase) executeProvider(ctx context.Context,
	identity obs.CorrelationIdentity,
	command ChatCommand,
	hasTrustedExecution bool,
) (chatProviderExecution, error) {
	startedAt := usecase.now()
	response, providerErr := usecase.provider.Chat(ctx, &llm.ChatRequest{
		Model: usecase.requestedModel,
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: command.Message,
		}},
	})
	completedAt := usecase.now()
	if providerErr != nil {
		failureStatus, safeErr := classifyProviderFailure(providerErr)
		usecase.recordFailedProvider(ctx, identity, startedAt, completedAt, failureStatus, hasTrustedExecution)
		return chatProviderExecution{failureStatus: failureStatus}, safeErr
	}
	canonicalModel, usage, isValidResponse := usecase.canonicalizeProviderResponse(response)
	if !isValidResponse {
		usecase.recordFailedProvider(ctx, identity, startedAt, completedAt, obs.FailureUpstream, hasTrustedExecution)
		return chatProviderExecution{failureStatus: obs.FailureUpstream}, ErrChatInvalidResponse
	}
	generationInput := usecase.newGenerationInput(ctx, identity, startedAt, completedAt, chatSuccessOutcome, "")
	generationInput.ActualModel = canonicalModel
	generationInput.FinishReason = response.FinishReason
	generationInput.Usage = usage
	generationIdentity := usecase.recordGenerationObservation(ctx, generationInput, hasTrustedExecution)
	usecase.recordTelemetry(successTrace(identity, canonicalModel, usage, usecase.now()))
	return chatProviderExecution{
		response:           response,
		usage:              usage,
		canonicalModel:     canonicalModel,
		generationIdentity: generationIdentity,
	}, nil
}

func (usecase *ChatUsecase) recordFailedProvider(
	ctx context.Context,
	identity obs.CorrelationIdentity,
	startedAt, completedAt time.Time,
	failureStatus obs.FailureStatus,
	hasTrustedExecution bool,
) {
	generationInput := usecase.newGenerationInput(
		ctx,
		identity,
		startedAt,
		completedAt,
		chatFailedOutcome,
		failureStatus,
	)
	usecase.recordGenerationObservation(ctx, generationInput, hasTrustedExecution)
	usecase.recordTelemetry(failureTrace(identity, usecase.requestedModel, usecase.now(), failureStatus))
}

func (usecase *ChatUsecase) newGenerationInput(
	ctx context.Context,
	identity obs.CorrelationIdentity,
	startedAt, completedAt time.Time,
	outcome string,
	failureStatus obs.FailureStatus,
) appobs.GenerationSpanInput {
	return appobs.GenerationSpanInput{
		SmokeRunID:            appobs.ChatSmokeRunIDFromContext(ctx),
		Feature:               chatFeature,
		StartedAt:             startedAt,
		CompletedAt:           completedAt,
		Identity:              identity,
		Provider:              usecase.providerName,
		RequestedModel:        usecase.requestedModel,
		TotalLatency:          completedAt.Sub(startedAt),
		Outcome:               outcome,
		FailureStatus:         string(failureStatus),
		PromptTemplateVersion: usecase.promptTemplateVersion,
		PromptHash:            usecase.promptHash,
		PayloadMode:           usecase.payloadMode,
		PayloadRedacted:       usecase.payloadRedacted,
	}
}

func (usecase *ChatUsecase) writeRunManifest(ctx context.Context, marker string, identity obs.CorrelationIdentity) {
	if usecase == nil || usecase.runManifestWriter == nil || marker == "" {
		return
	}
	spanContext := traceapi.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return
	}
	if err := usecase.runManifestWriter.Write(ctx, ChatRunManifestInput{
		SmokeRunID: marker, RequestID: identity.RequestID, AITraceID: identity.AITraceID,
		ServiceTraceID: spanContext.TraceID().String(), SpanID: spanContext.SpanID().String(),
	}); err != nil {
		usecase.recordSideEffectFailure("run_manifest")
	}
}

func (usecase *ChatUsecase) startAIExecution(ctx context.Context) (obs.CorrelationIdentity, context.Context, error) {
	identity, _ := obs.CorrelationIdentityFromContext(ctx)
	// ai_trace_id/eval_run_id 只能由各自领域边界生成。开始新 AI 用例时先不可变地清除
	// 入站旧值，避免复用 context 或 baggage 把本次 evidence 串到其它运行。
	identity = obs.ApplyCorrelationOptions(
		identity,
		obs.WithAITraceID(""),
		obs.WithEvalRunID(""),
	)
	if usecase == nil || usecase.newAITraceID == nil {
		return identity, ctx, ErrChatConfiguration
	}
	aiTraceID := strings.TrimSpace(usecase.newAITraceID())
	if !isSafeAITraceID(aiTraceID) {
		return identity, ctx, ErrChatConfiguration
	}
	identity = obs.ApplyCorrelationOptions(identity, obs.WithAITraceID(aiTraceID))
	return identity, obs.ContextWithCorrelationIdentity(ctx, identity), nil
}

func (usecase *ChatUsecase) startChatBoundary(
	ctx context.Context,
	identity obs.CorrelationIdentity,
) (context.Context, obs.CorrelationIdentity, appobs.EndChatAIExecution, bool) {
	if usecase == nil || usecase.bridge == nil {
		if usecase != nil && usecase.isEvaluatorConfigured {
			usecase.recordSideEffectFailure(chatBridgeComponent)
		}
		return ctx, identity, nil, false
	}
	derivedContext, derivedIdentity, end, err := usecase.bridge.Start(ctx, identity)
	if err != nil || derivedContext == nil || end == nil || !isTrustedBoundaryIdentity(identity, derivedIdentity) {
		if end != nil {
			_ = end(appobs.ChatAIExecutionOutcome{
				Outcome:       chatFailedOutcome,
				FailureStatus: obs.FailureTelemetryExportFailed,
			})
		}
		usecase.recordSideEffectFailure(chatBridgeComponent)
		return ctx, identity, nil, false
	}
	return obs.ContextWithCorrelationIdentity(derivedContext, derivedIdentity), derivedIdentity, end, true
}

func (usecase *ChatUsecase) recordGenerationObservation(
	ctx context.Context,
	input appobs.GenerationSpanInput,
	hasTrustedExecution bool,
) appobs.PlatformSpanIdentity {
	if usecase == nil {
		return appobs.PlatformSpanIdentity{}
	}
	if usecase.generationObserver == nil {
		if usecase.isEvaluatorConfigured {
			usecase.recordSideEffectFailure(chatGenerationComponent)
		}
		return appobs.PlatformSpanIdentity{}
	}
	if !hasTrustedExecution {
		return appobs.PlatformSpanIdentity{}
	}
	identity, err := usecase.generationObserver.RecordGeneration(ctx, input)
	if err != nil || !isValidPlatformSpanIdentity(identity) {
		usecase.recordSideEffectFailure(chatGenerationComponent)
		return appobs.PlatformSpanIdentity{}
	}
	return identity
}

func (usecase *ChatUsecase) evaluateAndPersist(
	ctx context.Context,
	identity obs.CorrelationIdentity,
	canonicalModel string,
	response *llm.ChatResponse,
	usage llm.Usage,
	generationIdentity appobs.PlatformSpanIdentity,
	hasTrustedExecution bool,
) *DebugEvalSummary {
	if usecase == nil || usecase.evaluator == nil {
		return nil
	}
	if !usecase.isEvaluatorConfigured {
		evaluation, err := usecase.evaluator.Evaluate(ctx, CompletionContractEvaluationInput{})
		if err != nil {
			usecase.recordSideEffectFailure(chatEvaluatorComponent)
			return nil
		}
		return cloneDebugEvalSummary(&evaluation.Summary)
	}
	// Evidence 必须关联真实 root/bridge identity；boundary 失败时宁可显式不运行，
	// 也不能把入站可伪造的 service trace/span 字符串固化到本地事实源。
	if !hasTrustedExecution {
		return nil
	}
	execution, ok := usecase.executeEvaluation(ctx, identity, canonicalModel, response, usage)
	if !ok {
		return nil
	}
	if execution.result.Evidence != nil &&
		!evidenceMatchesEvaluationIdentity(*execution.result.Evidence, execution.identity) {
		usecase.recordSideEffectFailure(chatEvaluatorComponent)
		return nil
	}
	summary := cloneDebugEvalSummary(&execution.result.Summary)
	if execution.result.Evidence == nil {
		return summary
	}
	evidence := cloneEvaluationEvidence(*execution.result.Evidence)
	if !usecase.persistEvidence(execution.context, evidence) {
		return summary
	}
	usecase.recordEvaluatorObservation(execution, evidence)
	usecase.projectEvidence(execution.context, evidence, generationIdentity)
	return summary
}

func (usecase *ChatUsecase) executeEvaluation(
	ctx context.Context,
	identity obs.CorrelationIdentity,
	canonicalModel string,
	response *llm.ChatResponse,
	usage llm.Usage,
) (chatEvaluationExecution, bool) {
	if usecase.newEvalRunID == nil {
		usecase.recordSideEffectFailure(chatEvaluatorComponent)
		return chatEvaluationExecution{}, false
	}
	evalRunID := strings.TrimSpace(usecase.newEvalRunID())
	if !isSafeAITraceID(evalRunID) {
		usecase.recordSideEffectFailure(chatEvaluatorComponent)
		return chatEvaluationExecution{}, false
	}
	evaluationIdentity := obs.ApplyCorrelationOptions(identity, obs.WithEvalRunID(evalRunID))
	evaluationContext := obs.ContextWithCorrelationIdentity(ctx, evaluationIdentity)
	startedAt := usecase.now()
	result, err := usecase.evaluator.Evaluate(evaluationContext, CompletionContractEvaluationInput{
		Identity:      evaluationIdentity,
		ActualModel:   canonicalModel,
		FinishReason:  response.FinishReason,
		Usage:         usage,
		OutputPresent: response.Content != "",
	})
	completedAt := usecase.now()
	if err != nil {
		usecase.recordSideEffectFailure(chatEvaluatorComponent)
		return chatEvaluationExecution{}, false
	}
	return chatEvaluationExecution{
		context:     evaluationContext,
		identity:    evaluationIdentity,
		result:      result,
		startedAt:   startedAt,
		completedAt: completedAt,
	}, true
}

func (usecase *ChatUsecase) recordEvaluatorObservation(
	execution chatEvaluationExecution,
	evidence aieval.EvaluationEvidence,
) {
	if usecase.evaluatorObserver == nil {
		usecase.recordSideEffectFailure(chatEvaluatorSpanComponent)
		return
	}
	if _, observerErr := usecase.evaluatorObserver.RecordEvaluator(execution.context, appobs.EvaluatorSpanInput{
		SmokeRunID:  appobs.ChatSmokeRunIDFromContext(execution.context),
		Feature:     chatFeature,
		StartedAt:   execution.startedAt,
		CompletedAt: execution.completedAt,
		Evidence:    cloneEvaluationEvidence(evidence),
	}); observerErr != nil {
		usecase.recordSideEffectFailure(chatEvaluatorSpanComponent)
	}
}

func (usecase *ChatUsecase) persistEvidence(
	ctx context.Context,
	evidence aieval.EvaluationEvidence,
) bool {
	if usecase.evidenceStore == nil {
		usecase.recordSideEffectFailure(chatEvidenceStoreComponent)
		return false
	}
	if storeErr := usecase.evidenceStore.Append(ctx, cloneEvaluationEvidence(evidence)); storeErr != nil {
		usecase.recordSideEffectFailure(chatEvidenceStoreComponent)
		return false
	}
	return true
}

func (usecase *ChatUsecase) projectEvidence(
	ctx context.Context,
	evidence aieval.EvaluationEvidence,
	generationIdentity appobs.PlatformSpanIdentity,
) {
	if usecase.projectionQueue == nil {
		usecase.recordSideEffectFailure(chatScoreProjectionComponent)
		return
	}
	if !generationIdentity.CanProject() || generationIdentity.TraceID != evidence.ServiceTraceID {
		usecase.recordSideEffectFailure(chatScoreProjectionComponent)
		return
	}
	if projectionErr := usecase.projectionQueue.TryEnqueue(ctx, ChatScoreProjectionInput{
		RunID:      appobs.ChatSmokeRunIDFromContext(ctx),
		Evidence:   cloneEvaluationEvidence(evidence),
		Generation: generationIdentity,
	}); projectionErr != nil {
		usecase.recordSideEffectFailure(chatScoreProjectionComponent)
	}
}

func evidenceMatchesEvaluationIdentity(evidence aieval.EvaluationEvidence, identity obs.CorrelationIdentity) bool {
	return evidence.EvalRunID == identity.EvalRunID &&
		evidence.RequestID == identity.RequestID &&
		evidence.AITraceID == identity.AITraceID &&
		evidence.ServiceTraceID == identity.ServiceTraceID &&
		evidence.SpanID == identity.SpanID
}

func (usecase *ChatUsecase) endChatBoundary(end appobs.EndChatAIExecution, outcome appobs.ChatAIExecutionOutcome) {
	if end == nil {
		return
	}
	if err := end(outcome); err != nil {
		usecase.recordSideEffectFailure(chatBridgeComponent)
	}
}

func (usecase *ChatUsecase) recordSideEffectFailure(component string) {
	if usecase == nil || usecase.diagnostics == nil {
		return
	}
	usecase.diagnostics.TryRecordTelemetryFailure(ChatTelemetryFailure{
		Component:  component,
		ErrorClass: string(ClassifyChatObservabilityFailure(component)),
	})
}

func isTrustedBoundaryIdentity(base, derived obs.CorrelationIdentity) bool {
	if derived.RequestID != base.RequestID ||
		derived.AITraceID != base.AITraceID ||
		derived.SessionID != base.SessionID ||
		derived.EvalRunID != "" ||
		derived.ServiceTraceID == "" ||
		derived.SpanID == "" {
		return false
	}
	if !((appobs.PlatformSpanIdentity{
		TraceID: derived.ServiceTraceID,
		SpanID:  derived.SpanID,
	}).IsValid()) {
		return false
	}
	return len(obs.ScanForbiddenPayloadFields(map[string]string{
		"service_trace_id": derived.ServiceTraceID,
		"span_id":          derived.SpanID,
	})) == 0
}

func isValidPlatformSpanIdentity(identity appobs.PlatformSpanIdentity) bool {
	if !identity.IsValid() {
		return false
	}
	return len(obs.ScanForbiddenPayloadFields(map[string]string{
		"platform_trace_id": identity.TraceID,
		"platform_span_id":  identity.SpanID,
	})) == 0
}

func cloneEvaluationEvidence(evidence aieval.EvaluationEvidence) aieval.EvaluationEvidence {
	cloned := evidence
	if evidence.Threshold != nil {
		threshold := *evidence.Threshold
		cloned.Threshold = &threshold
	}
	return cloned
}

func cloneDebugEvalSummary(summary *DebugEvalSummary) *DebugEvalSummary {
	if summary == nil {
		return nil
	}
	cloned := *summary
	cloned.Score = cloneEvalScore(summary.Score)
	return &cloned
}

func (usecase *ChatUsecase) recordTelemetry(trace obs.Trace) {
	if usecase == nil || usecase.telemetry == nil {
		return
	}
	if err := usecase.telemetry.TryRecord(trace); err != nil && usecase.diagnostics != nil {
		usecase.diagnostics.TryRecordTelemetryFailure(ChatTelemetryFailure{
			Component:  chatTelemetryComponent,
			ErrorClass: string(ClassifyChatObservabilityFailure(chatTelemetryComponent)),
		})
	}
}

func successTrace(identity obs.CorrelationIdentity, canonicalModel string, usage llm.Usage, timestamp time.Time) obs.Trace {
	return obs.NewTrace(
		identity.AITraceID,
		chatFeature,
		timestamp,
		obs.WithCorrelationIdentity(identity),
		obs.WithObservationType(obs.ObservationTypeGeneration),
		obs.WithModel(canonicalModel),
		obs.WithUsage(usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens),
		obs.WithCacheUsage(usage.CacheReadTokens, usage.CacheWriteTokens),
		obs.WithOutcome(chatSuccessOutcome),
	)
}

func failureTrace(identity obs.CorrelationIdentity, requestedModel string, timestamp time.Time, failureStatus obs.FailureStatus, options ...obs.TraceOption) obs.Trace {
	baseOptions := []obs.TraceOption{
		obs.WithCorrelationIdentity(identity),
		obs.WithObservationType(obs.ObservationTypeGeneration),
		obs.WithModel(requestedModel),
	}
	baseOptions = append(baseOptions, options...)
	return obs.NewFailureTrace(identity.AITraceID, chatFeature, timestamp, failureStatus, baseOptions...)
}

// ChatFailureClass 是模型业务失败的稳定错误类（T116 契约）。controller
// 依据 sentinel 错误链投影 HTTP 状态，而类名本身是日志/指标聚合的稳定维度：
// 同一类故障不会因为 provider 或 adapter 的表达差异被写成多个维度。
type ChatFailureClass string

const (
	ChatFailureClassNone                ChatFailureClass = "none"
	ChatFailureClassRateLimited         ChatFailureClass = "rate_limited"
	ChatFailureClassUpstreamUnavailable ChatFailureClass = "upstream_unavailable"
	ChatFailureClassUpstreamTimeout     ChatFailureClass = "upstream_timeout"
	ChatFailureClassCallerCanceled      ChatFailureClass = "caller_canceled"
	ChatFailureClassInvalidResponse     ChatFailureClass = "invalid_response"
	ChatFailureClassProviderFailure     ChatFailureClass = "provider_failure"
)

// ChatObservabilityFailureClass 是观测旁路失败的稳定错误类。与
// ChatFailureClass 严格不相交：观测故障（telemetry/score/exporter 投递）
// 只记录诊断事实，绝不进入业务错误链（FR-007 双向隔离）。
type ChatObservabilityFailureClass string

const (
	ChatObservabilityClassTelemetryExport ChatObservabilityFailureClass = "telemetry_export_failed"
	ChatObservabilityClassSideEffect      ChatObservabilityFailureClass = "side_effect_failed"
)

// ClassifyChatModelFailure 把 provider 失败映射为稳定业务错误类 + 低敏
// sentinel 错误链。原始外部错误绝不透传（可能携带响应正文或凭据），
// sentinel 链保持 controller 现有 errors.Is 语义：
//
//	429      -> rate_limited         (llm.ErrRateLimit + llm.ErrUpstream)
//	5xx             -> upstream_unavailable (llm.ErrUpstream)
//	timeout         -> upstream_timeout     (llm.ErrUpstream + context.DeadlineExceeded)
//	canceled        -> caller_canceled      (context.Canceled，不是上游故障)
//	invalid response -> invalid_response    (ErrChatInvalidResponse)
//	其它            -> provider_failure     (ErrChatProviderFailure)
//
// nil error 是 none，不产生任何错误链。
func ClassifyChatModelFailure(err error) (ChatFailureClass, error) {
	if err == nil {
		return ChatFailureClassNone, nil
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ChatFailureClassUpstreamTimeout, errors.Join(llm.ErrUpstream, context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return ChatFailureClassCallerCanceled, context.Canceled
	case errors.Is(err, llm.ErrRateLimit):
		return ChatFailureClassRateLimited, errors.Join(llm.ErrUpstream, llm.ErrRateLimit)
	case errors.Is(err, llm.ErrInvalidResponse), errors.Is(err, ErrChatInvalidResponse):
		// Provider 原始错误链可能携带响应正文或 endpoint。这里只返回应用层稳定
		// sentinel 本身，使 controller 可以安全投影 502，同时保留既有 AI 身份。
		return ChatFailureClassInvalidResponse, ErrChatInvalidResponse
	case errors.Is(err, llm.ErrUpstream):
		return ChatFailureClassUpstreamUnavailable, llm.ErrUpstream
	default:
		return ChatFailureClassProviderFailure, ErrChatProviderFailure
	}
}

// ClassifyChatObservabilityFailure 把观测旁路失败的组件归类为稳定观测错误类。
// generation 遥测入队失败是投递类错误，其余旁路（score projection、evaluator、
// evidence store、bridge、manifest、exporter）是副作用类错误；未知组件安全
// 落到 side_effect_failed。返回的类绝不与 ChatFailureClass 重叠——观测故障
// 不可能被投影为模型业务失败（禁止 exporter failure 返回为 5xx）。
func ClassifyChatObservabilityFailure(component string) ChatObservabilityFailureClass {
	if component == chatTelemetryComponent {
		return ChatObservabilityClassTelemetryExport
	}
	return ChatObservabilityClassSideEffect
}

// classifyProviderFailure 保留 Execute 内部所需的 obs.FailureStatus 投影，
// 但分类事实委托给 ClassifyChatModelFailure：业务错误类与观测错误类共用
// 同一份分类逻辑，避免两个词表漂移。
func classifyProviderFailure(err error) (obs.FailureStatus, error) {
	class, safeErr := ClassifyChatModelFailure(err)
	var failureStatus obs.FailureStatus
	switch class {
	case ChatFailureClassRateLimited:
		failureStatus = obs.FailureRateLimit
	case ChatFailureClassUpstreamTimeout:
		failureStatus = obs.FailureTimeout
	case ChatFailureClassCallerCanceled:
		failureStatus = obs.FailureCallerError
	default:
		failureStatus = obs.FailureUpstream
	}
	return failureStatus, safeErr
}

func (usecase *ChatUsecase) canonicalizeProviderResponse(response *llm.ChatResponse) (string, llm.Usage, bool) {
	if response == nil ||
		usecase == nil ||
		usecase.canonicalizeActualModel == nil ||
		len(response.Content) > maxChatResponseBytes {
		return "", llm.Usage{}, false
	}
	canonicalModel, ok := usecase.canonicalizeActualModel(response.Model)
	if !ok || !isSafeModelIdentifier(canonicalModel) || !isSafeFinishReason(response.FinishReason) {
		return "", llm.Usage{}, false
	}
	if response.FinishReason == llm.FinishToolCall || len(response.ToolCalls) > 0 {
		return "", llm.Usage{}, false
	}

	// 类型迁移时必须显式提取已报告事实；只把这份摘要传给后续投影，不能
	// 解引用可选指针，或把缺失摘要的数值零值当作成功 usage。
	usage, reported := response.Usage.Summary()
	if !reported {
		return "", llm.Usage{}, false
	}
	if usage.InputTokens < 0 ||
		usage.OutputTokens < 0 ||
		usage.ReasoningTokens < 0 ||
		usage.CacheReadTokens < 0 ||
		usage.CacheWriteTokens < 0 ||
		usage.InputTokens > maxUsageTokens ||
		usage.OutputTokens > maxUsageTokens ||
		usage.ReasoningTokens > maxUsageTokens ||
		usage.CacheReadTokens > maxUsageTokens ||
		usage.CacheWriteTokens > maxUsageTokens ||
		usage.TotalTokens > maxUsageTokens ||
		usage.TotalTokens < usage.InputTokens+usage.OutputTokens {
		return "", llm.Usage{}, false
	}
	return canonicalModel, usage, true
}

func isSafeModelIdentifier(model string) bool {
	trimmed := strings.TrimSpace(model)
	return trimmed == model &&
		len(model) > 0 &&
		len(model) <= maxModelIdentifierBytes &&
		safeModelIdentifierPattern.MatchString(model) &&
		len(obs.ScanForbiddenPayloadFields(map[string]string{"model": model})) == 0
}

func isSafeFinishReason(reason llm.FinishReason) bool {
	switch reason {
	case llm.FinishStop, llm.FinishLength, llm.FinishContentFilter:
		return len(reason) <= maxFinishReasonBytes
	default:
		return false
	}
}

func isSafeAITraceID(value string) bool {
	return len(value) > 0 &&
		len(value) <= maxAITraceIDBytes &&
		safeAITraceIDPattern.MatchString(value)
}
