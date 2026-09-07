// Package smoke 组合本地可重复的 AI 能力冒烟验证。
//
// 这里属于 internal/eval，而不是 pkg/ai/eval：它不是通用评估框架能力，
// 而是把本项目 P0 已完成的 prompt、llm、obs、eval 模块串起来的本地门禁。
package smoke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	aieval "github.com/ashjazz/Longtermism/pkg/ai/eval"
	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
	"github.com/ashjazz/Longtermism/pkg/ai/prompt"
)

const (
	DefaultDatasetPath    = "internal/eval/golden/p0_smoke.json"
	DefaultDatasetName    = "p0-smoke"
	DefaultDatasetVersion = "p0-smoke-local"
	DefaultPromptRoot     = "resource/prompt"
	DefaultPromptName     = "p0_smoke"
	DefaultPromptVersion  = "v1"
	DefaultModel          = "p0-smoke-fake"
	DefaultEvalRunID      = "eval-run-p0-smoke-local"

	featureName   = "p0_eval_smoke"
	successStatus = "success"
	failedStatus  = "failed"
)

// Config 是 P0 smoke 的装配参数。
//
// 默认值会构造本地 JSON dataset、文件系统 prompt registry、fake LLM 和内存 trace recorder。
// 这保证 `make eval-smoke` 不需要真实 API key，同时测试仍可注入自定义 Dataset/Tracer。
type Config struct {
	Dataset         aieval.Dataset
	DatasetPath     string
	DatasetIdentity aieval.DatasetIdentity
	PromptRegistry  prompt.Registry
	PromptRoot      string
	PromptName      string
	PromptVersion   string
	Provider        llm.Provider
	Model           string
	EvalRunID       string
	Tracer          obs.Tracer
	Now             func() time.Time
}

// Result 是一次 P0 smoke 运行的证据包。
//
// Report 用于门禁判断，Traces 用于证明每条样例确实走过 prompt -> llm -> obs -> eval。
type Result struct {
	Report aieval.Report `json:"report"`
	Traces []obs.Trace   `json:"traces,omitempty"`
}

// RunP0 执行 P0 最小 AI 工程闭环。
//
// 执行路径：
//  1. 加载 golden dataset；
//  2. 渲染 Prompt as Code 模板；
//  3. 调用本地 fake LLM provider；
//  4. 记录不含原始 query/prompt 的 trace；
//  5. 用确定性 eval runner 产出报告。
func RunP0(ctx context.Context, config Config) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cfg := applyDefaults(config)
	samples, err := cfg.Dataset.Load(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("load p0 smoke dataset: %w", err)
	}
	if len(samples) == 0 {
		return Result{}, fmt.Errorf("p0 smoke dataset samples are required")
	}

	if cfg.Provider == nil {
		cfg.Provider, err = newGoldenFakeProvider(cfg.Model, samples)
		if err != nil {
			return Result{}, err
		}
	}
	template, err := cfg.PromptRegistry.Get(ctx, cfg.PromptName, cfg.PromptVersion)
	if err != nil {
		return Result{}, fmt.Errorf("load p0 smoke prompt: %w", err)
	}

	traceRecorder := newTraceRecorder()
	cfg.Tracer = newTeeTracer(cfg.Tracer, traceRecorder)

	runnerOptions := make([]aieval.RunnerOption, 0, 1)
	if strings.TrimSpace(cfg.EvalRunID) != "" {
		runnerOptions = append(runnerOptions, aieval.WithEvalRunID(cfg.EvalRunID))
	}
	runner := aieval.NewRunner(cfg.DatasetIdentity, runnerOptions...)
	report, err := runner.Run(ctx, newStaticDataset(samples), func(ctx context.Context, sample aieval.Sample) (aieval.Prediction, error) {
		return predictSample(ctx, cfg, template, sample)
	}, smokeMetrics())
	if err != nil {
		return Result{}, err
	}

	return Result{
		Report: report,
		Traces: traceRecorder.Traces(),
	}, nil
}

func applyDefaults(config Config) Config {
	cfg := config
	if cfg.Dataset == nil {
		path := cfg.DatasetPath
		if strings.TrimSpace(path) == "" {
			path = DefaultDatasetPath
		}
		cfg.Dataset = aieval.NewJSONDataset(resolveLocalPath(path))
	}
	cfg.DatasetIdentity = normalizeP0DatasetIdentity(cfg.DatasetIdentity)
	if strings.TrimSpace(cfg.DatasetIdentity.Name) == "" {
		cfg.DatasetIdentity.Name = DefaultDatasetName
	}
	if strings.TrimSpace(cfg.DatasetIdentity.Version) == "" {
		cfg.DatasetIdentity.Version = DefaultDatasetVersion
	}
	if cfg.PromptRegistry == nil {
		root := cfg.PromptRoot
		if strings.TrimSpace(root) == "" {
			root = DefaultPromptRoot
		}
		cfg.PromptRegistry = prompt.NewFilesystemRegistry(resolveLocalPath(root))
	}
	if strings.TrimSpace(cfg.PromptName) == "" {
		cfg.PromptName = DefaultPromptName
	}
	if strings.TrimSpace(cfg.PromptVersion) == "" {
		cfg.PromptVersion = DefaultPromptVersion
	}
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time {
			return time.Now().UTC()
		}
	}
	return cfg
}

func normalizeP0DatasetIdentity(identity aieval.DatasetIdentity) aieval.DatasetIdentity {
	return aieval.DatasetIdentity{
		Name:    strings.TrimSpace(identity.Name),
		Version: strings.TrimSpace(identity.Version),
	}
}

func predictSample(ctx context.Context, cfg Config, template prompt.Template, sample aieval.Sample) (aieval.Prediction, error) {
	startedAt := cfg.Now()
	identity := correlationIdentityForP0Sample(sample)
	rendered, err := renderPrompt(ctx, template, sample)
	if err != nil {
		return aieval.Prediction{}, err
	}

	request := &llm.ChatRequest{
		Model: sampleModel(cfg.Model, sample.ID),
		Messages: []llm.Message{
			{
				Role:    llm.RoleUser,
				Content: rendered.Content,
			},
		},
		Temperature: ptrFloat64(0),
	}
	response, err := cfg.Provider.Chat(ctx, request)
	if err != nil {
		recordTrace(ctx, cfg, identity, sample, rendered, request.Model, llm.Usage{}, startedAt, failedStatus)
		return aieval.Prediction{}, fmt.Errorf("call p0 smoke fake llm for sample %q: %w", sample.ID, err)
	}

	if response == nil {
		recordTrace(ctx, cfg, identity, sample, rendered, request.Model, llm.Usage{}, startedAt, failedStatus)
		return aieval.Prediction{}, llm.ErrInvalidResponse
	}
	usage, reported := response.Usage.Summary()
	if !reported {
		recordTrace(ctx, cfg, identity, sample, rendered, request.Model, llm.Usage{}, startedAt, failedStatus)
		return aieval.Prediction{}, llm.ErrInvalidResponse
	}
	recordTrace(ctx, cfg, identity, sample, rendered, response.Model, usage, startedAt, successStatus)
	return aieval.Prediction{
		Answer:        response.Content,
		Context:       append([]string(nil), sample.RelevantCtx...),
		TokensUsed:    usage.TotalTokens,
		TraceIdentity: identity,
	}, nil
}

func renderPrompt(ctx context.Context, template prompt.Template, sample aieval.Sample) (prompt.Rendered, error) {
	rendered, err := template.Render(ctx, map[string]any{
		"Question": sample.Query,
		"Context":  strings.Join(sample.RelevantCtx, "\n"),
	})
	if err != nil {
		return prompt.Rendered{}, fmt.Errorf("render p0 smoke prompt for sample %q: %w", sample.ID, err)
	}
	return rendered, nil
}

func recordTrace(ctx context.Context, cfg Config, identity obs.CorrelationIdentity, sample aieval.Sample, rendered prompt.Rendered, model string, usage llm.Usage, startedAt time.Time, status string) {
	trace := obs.NewTrace(
		identity.AITraceID,
		featureName,
		startedAt,
		obs.WithCorrelationIdentity(identity),
		obs.WithObservationType(obs.ObservationTypeGeneration),
		obs.WithQuery(shortHash(sample.Query), "", len([]rune(sample.Query))),
		obs.WithModel(model),
		obs.WithPrompt(rendered.Version, rendered.Hash),
		obs.WithUsage(usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens),
		obs.WithLatency(0, cfg.Now().Sub(startedAt).Milliseconds()),
		obs.WithRetrieval(len(sample.RelevantCtx), "", nil, 0),
		obs.WithOutcome(status),
	)
	cfg.Tracer.Record(ctx, trace)
}

func correlationIdentityForP0Sample(sample aieval.Sample) obs.CorrelationIdentity {
	idPart := sanitizeTraceIDPart(sample.ID)
	return obs.NewCorrelationIdentity(
		"req-p0-smoke-"+idPart,
		obs.WithServiceSpan("svc-trace-p0-smoke-"+idPart, "span-p0-smoke-"+idPart),
		obs.WithAITraceID("ai-trace-p0-smoke-"+idPart),
	)
}

func smokeMetrics() []aieval.Metric {
	return []aieval.Metric{
		aieval.NewExactMatchMetric(),
		aieval.NewContextHitMetric(),
	}
}

func sampleModel(baseModel, sampleID string) string {
	return baseModel + "/" + sampleID
}

func ptrFloat64(value float64) *float64 {
	return &value
}

func shortHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:8])
}

var traceIDPartUnsafePattern = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func sanitizeTraceIDPart(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "sample"
	}
	sanitized := traceIDPartUnsafePattern.ReplaceAllString(trimmed, "-")
	sanitized = strings.Trim(sanitized, "-_")
	if sanitized == "" {
		return "sample"
	}
	return sanitized
}

func resolveLocalPath(path string) string {
	if filepath.IsAbs(path) || pathExists(path) {
		return path
	}

	dir, err := os.Getwd()
	if err != nil {
		return path
	}
	for {
		candidate := filepath.Join(dir, path)
		if pathExists(candidate) {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
		dir = parent
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type goldenFakeProvider struct {
	baseModel string
	responses map[string]llm.ChatResponse
}

func newGoldenFakeProvider(baseModel string, samples []aieval.Sample) (*goldenFakeProvider, error) {
	responses := make(map[string]llm.ChatResponse, len(samples))
	for _, sample := range samples {
		model := sampleModel(baseModel, sample.ID)
		usage, err := llm.NewReportedProviderUsage(usageForSample(sample))
		if err != nil {
			return nil, err
		}
		responses[model] = llm.ChatResponse{
			Content:      sample.GroundTruth,
			Model:        model,
			Usage:        usage,
			FinishReason: llm.FinishStop,
		}
	}
	return &goldenFakeProvider{
		baseModel: baseModel,
		responses: responses,
	}, nil
}

func (p *goldenFakeProvider) Name() string {
	return "p0-smoke-fake"
}

func (p *goldenFakeProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{}
}

func (p *goldenFakeProvider) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("p0 smoke fake llm request is required")
	}

	response, ok := p.responses[req.Model]
	if !ok {
		return nil, fmt.Errorf("p0 smoke fake llm has no response for model %q", req.Model)
	}
	cloned := response
	return &cloned, nil
}

func (p *goldenFakeProvider) ChatStream(context.Context, *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	return nil, fmt.Errorf("p0 smoke fake llm does not support streaming")
}

func usageForSample(sample aieval.Sample) llm.Usage {
	inputTokens := len([]rune(sample.Query)) + len([]rune(strings.Join(sample.RelevantCtx, "\n")))
	outputTokens := len([]rune(sample.GroundTruth))
	return llm.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	}
}

type staticDataset struct {
	samples []aieval.Sample
}

func newStaticDataset(samples []aieval.Sample) *staticDataset {
	return &staticDataset{samples: cloneSamples(samples)}
}

func (d *staticDataset) Load(ctx context.Context) ([]aieval.Sample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return cloneSamples(d.samples), nil
}

type traceRecorder struct {
	mu     sync.RWMutex
	traces []obs.Trace
}

func newTraceRecorder() *traceRecorder {
	return &traceRecorder{}
}

func (r *traceRecorder) Record(_ context.Context, trace obs.Trace) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.traces = append(r.traces, cloneTrace(trace))
}

func (r *traceRecorder) Traces() []obs.Trace {
	r.mu.RLock()
	defer r.mu.RUnlock()

	traces := make([]obs.Trace, len(r.traces))
	for index, trace := range r.traces {
		traces[index] = cloneTrace(trace)
	}
	return traces
}

type teeTracer struct {
	tracers []obs.Tracer
}

func newTeeTracer(tracers ...obs.Tracer) obs.Tracer {
	filtered := make([]obs.Tracer, 0, len(tracers))
	for _, tracer := range tracers {
		if tracer != nil {
			filtered = append(filtered, tracer)
		}
	}
	return teeTracer{tracers: filtered}
}

func (t teeTracer) Record(ctx context.Context, trace obs.Trace) {
	for _, tracer := range t.tracers {
		tracer.Record(ctx, trace)
	}
}

func cloneSamples(samples []aieval.Sample) []aieval.Sample {
	if samples == nil {
		return nil
	}

	cloned := make([]aieval.Sample, len(samples))
	for index, sample := range samples {
		cloned[index] = cloneSample(sample)
	}
	return cloned
}

func cloneSample(sample aieval.Sample) aieval.Sample {
	cloned := sample
	cloned.RelevantCtx = append([]string(nil), sample.RelevantCtx...)
	cloned.Meta = cloneMeta(sample.Meta)
	return cloned
}

func cloneMeta(meta map[string]any) map[string]any {
	if meta == nil {
		return nil
	}

	cloned := make(map[string]any, len(meta))
	for key, value := range meta {
		cloned[key] = cloneValue(value)
	}
	return cloned
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMeta(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

func cloneTrace(trace obs.Trace) obs.Trace {
	cloned := trace
	cloned.TopScores = append([]float64(nil), trace.TopScores...)
	if trace.UserRating != nil {
		value := *trace.UserRating
		cloned.UserRating = &value
	}
	if trace.AutoEvalScore != nil {
		value := *trace.AutoEvalScore
		cloned.AutoEvalScore = &value
	}
	return cloned
}
