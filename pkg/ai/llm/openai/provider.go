// Package openai 提供 OpenAI-compatible Chat Completions adapter。
//
// P0 阶段先采用 OpenAI-compatible 协议，是因为 DeepSeek、OpenAI 兼容代理、
// 本地网关等都可以共享这类 HTTP/JSON 形态；但框架上层仍然只依赖 llm.Provider。
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
)

const providerName = "openai"

const chatCompletionsPath = "/chat/completions"

const defaultRequestTimeout = 60 * time.Second

// maximumUsageTokens 是非流式响应单次 usage 的安全解析上限。
//
// 该上限主要防止兼容供应商返回异常大值后污染成本、指标或触发整数运算风险；
// 它不是模型上下文窗口声明，也不用于猜测缺失 usage。
const maximumUsageTokens = 100_000_000

// 与 chat 的 1 MiB 内容边界一致；先限制 wire body 再解码，未知字段也受保护。
const maximumChatResponseBytes = 1 << 20

// Config 是 OpenAI-compatible provider 的稳定装配配置。
//
// APIKey 只在服务端注入并参与请求认证，绝不能进入客户端、日志或错误信息。
// Capabilities 允许 P0 用静态/配置表声明模型能力，避免运行时网络探测带来不稳定门禁。
type Config struct {
	BaseURL      string
	APIKey       string
	DefaultModel string
	HTTPClient   *http.Client
	Capabilities map[string]llm.ProviderCapabilities
}

// Provider 实现 llm.Provider，并把 OpenAI-compatible 协议映射到框架内部契约。
//
// 字段保持私有，避免调用方绕过构造函数修改 baseURL、apiKey 或 capabilities。
// 这类配置一旦在运行期漂移，会让 trace、成本统计和故障诊断很难复现。
type Provider struct {
	baseURL      string
	apiKey       string
	defaultModel string
	httpClient   *http.Client
	capabilities map[string]llm.ProviderCapabilities
}

// NewProvider 创建 provider，并在装配阶段快速暴露缺失配置。
//
// 生产环境里，baseURL/API key/default model 属于启动期配置问题；如果延迟到用户请求
// 进入 LLM 路径后才失败，resilience 层会难以区分“系统配置错误”和“上游临时不可用”。
func NewProvider(config Config) (*Provider, error) {
	baseURL, err := normalizeBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("openai config apiKey is required")
	}
	defaultModel := strings.TrimSpace(config.DefaultModel)
	if defaultModel == "" {
		return nil, fmt.Errorf("openai config default model is required")
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		// 不直接复用 http.DefaultClient：它的 Timeout 默认为 0，意味着调用方若传入
		// context.Background()，连接或响应读取可能无限阻塞。独立 client 也避免修改
		// 全局默认客户端后影响进程内其它 HTTP 调用。
		httpClient = &http.Client{
			Timeout: defaultRequestTimeout,
			// Bearer credential 只属于调用方显式配置的 base URL。默认 client 禁止
			// 跟随重定向，避免 adapter 独立使用时把认证请求扩散到未授权目标。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	return &Provider{
		baseURL:      baseURL,
		apiKey:       apiKey,
		defaultModel: defaultModel,
		httpClient:   httpClient,
		capabilities: cloneCapabilities(config.Capabilities),
	}, nil
}

// Name 返回 provider 稳定标识。
//
// 这个值会进入 trace、限流 key、健康检查和后续多 provider failover 日志，不能使用
// baseURL 或模型名这类可能包含环境细节、也可能随部署变化的字段。
func (p *Provider) Name() string {
	return providerName
}

// Capabilities 返回模型能力声明。
//
// P0 不做运行时能力探测：当前 adapter 已完整实现普通/流式 tool calling、
// streaming 与 strict structured output，因此默认声明这些协议能力；prompt caching、
// vision 等仍依赖具体模型或供应商的能力保持关闭。
func (p *Provider) Capabilities(model string) llm.ProviderCapabilities {
	if p == nil {
		return llm.ProviderCapabilities{}
	}
	if capability, ok := p.capabilities[model]; ok {
		return capability
	}
	return defaultCapabilities()
}

// Chat 目前只先保留请求边界校验；HTTP 请求映射与响应解析分别由 T030/T031 实现。
func (p *Provider) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if err := validateChatRequest(req); err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	resp, err := p.doChatRequest(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return parseChatResponse(resp)
}

// ChatStream 发起 OpenAI-compatible SSE 请求，并返回按序关闭的 chunk channel。
func (p *Provider) ChatStream(ctx context.Context, req *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	if err := validateChatRequest(req); err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	resp, err := p.doChatRequest(ctx, req, true)
	if err != nil {
		return nil, err
	}
	if err := classifyHTTPStatusError(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}

	chunks := make(chan llm.ChatChunk, 1)
	go streamChatChunks(ctx, resp.Body, chunks)
	return chunks, nil
}

func normalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("openai config baseURL is required")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("openai config baseURL is invalid: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("openai config baseURL must include scheme and host")
	}

	return strings.TrimRight(trimmed, "/"), nil
}

func cloneCapabilities(source map[string]llm.ProviderCapabilities) map[string]llm.ProviderCapabilities {
	if len(source) == 0 {
		return map[string]llm.ProviderCapabilities{}
	}

	cloned := make(map[string]llm.ProviderCapabilities, len(source))
	maps.Copy(cloned, source)
	return cloned
}

func defaultCapabilities() llm.ProviderCapabilities {
	return llm.ProviderCapabilities{
		ToolCalling:         true,
		StrictStructuredOut: true,
		Streaming:           true,
		StreamingToolCall:   true,
	}
}

func validateChatRequest(req *llm.ChatRequest) error {
	if req == nil {
		return fmt.Errorf("openai chat request is required")
	}
	if strings.TrimSpace(req.Model) == "" {
		return fmt.Errorf("openai chat request model is required")
	}
	if len(req.Messages) == 0 {
		return fmt.Errorf("openai chat request messages are required")
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}

	err := ctx.Err()
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("openai request deadline exceeded: %w", errors.Join(llm.ErrUpstream, err))
	}
	return err
}

func (p *Provider) doChatRequest(ctx context.Context, req *llm.ChatRequest, stream bool) (*http.Response, error) {
	payload := mapChatRequest(req, stream)

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return nil, fmt.Errorf("encode openai chat request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+chatCompletionsPath, &body)
	if err != nil {
		return nil, fmt.Errorf("create openai chat request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		if ctxErr := contextError(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("send openai chat request: %w", errors.Join(llm.ErrUpstream, err))
	}
	return resp, nil
}

func mapChatRequest(req *llm.ChatRequest, stream bool) openAIChatRequest {
	mapped := openAIChatRequest{
		Model:           strings.TrimSpace(req.Model),
		Messages:        mapMessages(req.Messages),
		Tools:           mapTools(req.Tools),
		ResponseFormat:  mapStructuredOutput(req.StructuredOutput),
		ReasoningEffort: strings.TrimSpace(req.ReasoningEffort),
		Stream:          stream,
	}
	mapped.Temperature = req.Temperature
	if req.MaxTokens != 0 {
		mapped.MaxTokens = req.MaxTokens
	}
	return mapped
}

func mapMessages(messages []llm.Message) []openAIMessage {
	mapped := make([]openAIMessage, 0, len(messages))
	for _, message := range messages {
		mapped = append(mapped, openAIMessage{
			Role:       string(message.Role),
			Content:    message.Content,
			Name:       strings.TrimSpace(message.Name),
			ToolCallID: strings.TrimSpace(message.ToolCallID),
		})
	}
	return mapped
}

func mapTools(tools []llm.Tool) []openAITool {
	if len(tools) == 0 {
		return nil
	}

	mapped := make([]openAITool, 0, len(tools))
	for _, tool := range tools {
		mapped = append(mapped, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
				Strict:      tool.Strict,
			},
		})
	}
	return mapped
}

func mapStructuredOutput(output *llm.StructuredOutput) *openAIResponseFormat {
	if output == nil {
		return nil
	}

	return &openAIResponseFormat{
		Type: "json_schema",
		JSONSchema: openAIJSONSchema{
			Name:   output.Name,
			Schema: output.Schema,
			Strict: output.Strict,
		},
	}
}

type openAIChatRequest struct {
	Model           string                `json:"model"`
	Messages        []openAIMessage       `json:"messages"`
	Tools           []openAITool          `json:"tools,omitempty"`
	ResponseFormat  *openAIResponseFormat `json:"response_format,omitempty"`
	ReasoningEffort string                `json:"reasoning_effort,omitempty"`
	Temperature     *float64              `json:"temperature,omitempty"`
	MaxTokens       int                   `json:"max_tokens,omitempty"`
	Stream          bool                  `json:"stream,omitempty"`
}

type openAIMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content,omitempty"`
	Name       string `json:"name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict,omitempty"`
}

type openAIResponseFormat struct {
	Type       string           `json:"type"`
	JSONSchema openAIJSONSchema `json:"json_schema"`
}

type openAIJSONSchema struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict"`
}

type openAIChatResponse struct {
	Model   string                   `json:"model"`
	Choices []openAIChoice           `json:"choices"`
	Usage   *openAINonStreamingUsage `json:"usage"`
}

type openAIChoice struct {
	Message      openAIResponseMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

type openAIResponseMessage struct {
	Content   string           `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAINonStreamingUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
}

// openAIUsage 保留流式协议的既有末尾 chunk 映射；非流式响应使用上面的
// presence-aware DTO，避免 T215 的严格成功契约无意改变 SSE 行为。
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func parseChatResponse(resp *http.Response) (*llm.ChatResponse, error) {
	if err := classifyHTTPStatusError(resp); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maximumChatResponseBytes+1))
	if err != nil {
		// 读 body 时仍可能发生取消、超时或断流。保留稳定传输语义，但不包装
		// 任意 reader 的原始错误，以免兼容网关把正文/凭据混进错误文本。
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.Join(llm.ErrUpstream, context.DeadlineExceeded)
		}
		return nil, llm.ErrUpstream
	}
	if len(body) > maximumChatResponseBytes {
		return nil, invalidOpenAIResponseError{}
	}
	var decoded openAIChatResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		// 2xx body 也是不可信外部输入。decoder 的原始错误可能携带 provider
		// 字段和值，因此只向 adapter 外返回稳定、低敏的协议分类。
		return nil, invalidOpenAIResponseError{}
	}

	providerUsage, err := mapNonStreamingUsage(decoded.Usage)
	if err != nil {
		return nil, err
	}

	result := &llm.ChatResponse{
		Model: decoded.Model,
		Usage: providerUsage,
	}
	if len(decoded.Choices) == 0 {
		return result, nil
	}

	choice := decoded.Choices[0]
	result.Content = choice.Message.Content
	result.FinishReason = mapFinishReason(choice.FinishReason)

	toolCalls, err := mapToolCalls(choice.Message.ToolCalls)
	if err != nil {
		return nil, err
	}
	result.ToolCalls = toolCalls
	return result, nil
}

// mapNonStreamingUsage 在 adapter 边界把 JSON presence 转成领域事实。
//
// 顶层 missing/null 和子字段 missing 都会解析为 nil，因此能与显式的数值 0
// 区分。任何失败只返回稳定类别，不携带 upstream body、model、endpoint 或凭据。
func mapNonStreamingUsage(input *openAINonStreamingUsage) (llm.ProviderUsage, error) {
	if input == nil || input.PromptTokens == nil || input.CompletionTokens == nil || input.TotalTokens == nil {
		return llm.NewUnavailableProviderUsage(), invalidOpenAIResponseError{}
	}

	promptTokens := *input.PromptTokens
	completionTokens := *input.CompletionTokens
	totalTokens := *input.TotalTokens
	if promptTokens < 0 || completionTokens < 0 || totalTokens < 0 {
		return llm.NewUnavailableProviderUsage(), invalidOpenAIResponseError{}
	}
	if promptTokens > maximumUsageTokens || completionTokens > maximumUsageTokens || totalTokens > maximumUsageTokens {
		return llm.NewUnavailableProviderUsage(), invalidOpenAIResponseError{}
	}
	if totalTokens != promptTokens+completionTokens {
		return llm.NewUnavailableProviderUsage(), invalidOpenAIResponseError{}
	}

	usage, err := llm.NewReportedProviderUsage(llm.Usage{
		InputTokens:  int(promptTokens),
		OutputTokens: int(completionTokens),
		TotalTokens:  int(totalTokens),
	})
	if err != nil {
		return llm.ProviderUsage{}, invalidOpenAIResponseError{}
	}
	return usage, nil
}

// invalidOpenAIResponseError 只暴露稳定分类。原始上游正文停留在 adapter 的
// bounded decode 过程内，不能经错误链进入日志、HTTP response 或 smoke report。
type invalidOpenAIResponseError struct{}

func (invalidOpenAIResponseError) Error() string { return llm.ErrInvalidResponse.Error() }

func (invalidOpenAIResponseError) Class() string { return "invalid_response" }

func (invalidOpenAIResponseError) Is(target error) bool { return target == llm.ErrInvalidResponse }

func mapFinishReason(reason string) llm.FinishReason {
	switch reason {
	case string(llm.FinishStop):
		return llm.FinishStop
	case string(llm.FinishLength):
		return llm.FinishLength
	case string(llm.FinishToolCall):
		return llm.FinishToolCall
	case string(llm.FinishContentFilter):
		return llm.FinishContentFilter
	default:
		return llm.FinishReason(reason)
	}
}

func mapToolCalls(calls []openAIToolCall) ([]llm.ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}

	mapped := make([]llm.ToolCall, 0, len(calls))
	for _, call := range calls {
		arguments, err := decodeToolArguments(call.Function.Arguments)
		if err != nil {
			return nil, invalidOpenAIResponseError{}
		}
		mapped = append(mapped, llm.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: arguments,
		})
	}
	return mapped, nil
}

func decodeToolArguments(raw string) (map[string]any, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]any{}, nil
	}

	var arguments map[string]any
	if err := json.Unmarshal([]byte(trimmed), &arguments); err != nil {
		return nil, err
	}
	return arguments, nil
}
