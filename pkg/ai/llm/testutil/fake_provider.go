// Package testutil 提供 llm 包测试专用工具。
//
// 这里的 fake provider 不是“简化版生产 provider”，而是一个可控的测试替身：
// 调用方可以精确指定某个 model 返回成功响应、tool call、流式 chunk 或上游错误。
// 这样单元测试能稳定覆盖生产语义，而不需要真实 API key、网络或供应商服务。
package testutil

import (
	"context"
	"fmt"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
)

const defaultFakeProviderName = "fake"

// FakeProviderConfig 描述 fake provider 的全部可控行为。
//
// 本工具按 req.Model 选择响应，因此测试可以用不同 model 名表达不同场景：
// chat-model 表示普通成功、tool-model 表示工具调用、stream-model 表示流式响应、
// upstream-model 表示上游失败。这种设计让测试意图非常直观。
type FakeProviderConfig struct {
	Name                string
	DefaultCapabilities llm.ProviderCapabilities
	Capabilities        map[string]llm.ProviderCapabilities
	ChatResponses       map[string]llm.ChatResponse
	ChatErrors          map[string]error
	StreamChunks        map[string][]llm.ChatChunk
	StreamErrors        map[string]error
}

// FakeProvider 是 llm.Provider 的内存实现，仅用于测试和本地示例。
//
// 它故意不做任何网络调用，也不读取环境变量。真实 provider adapter 要解决协议映射、
// 鉴权、HTTP 错误分类等问题；FakeProvider 只负责稳定复现上层契约需要的行为。
type FakeProvider struct {
	name                string
	defaultCapabilities llm.ProviderCapabilities
	capabilities        map[string]llm.ProviderCapabilities
	chatResponses       map[string]llm.ChatResponse
	chatErrors          map[string]error
	streamChunks        map[string][]llm.ChatChunk
	streamErrors        map[string]error
}

// NewFakeProvider 创建一个不可变配置的 fake provider。
//
// 构造时复制 map 和响应对象，是为了避免测试在运行过程中修改原始 config，
// 导致并行测试之间共享可变状态。这一点和生产里的 retry/failover 安全性是同一个思路。
func NewFakeProvider(config FakeProviderConfig) *FakeProvider {
	name := config.Name
	if name == "" {
		name = defaultFakeProviderName
	}

	return &FakeProvider{
		name:                name,
		defaultCapabilities: config.DefaultCapabilities,
		capabilities:        cloneCapabilitiesMap(config.Capabilities),
		chatResponses:       cloneChatResponseMap(config.ChatResponses),
		chatErrors:          cloneErrorMap(config.ChatErrors),
		streamChunks:        cloneStreamChunkMap(config.StreamChunks),
		streamErrors:        cloneErrorMap(config.StreamErrors),
	}
}

func (p *FakeProvider) Name() string {
	return p.name
}

func (p *FakeProvider) Capabilities(model string) llm.ProviderCapabilities {
	if capabilities, ok := p.capabilities[model]; ok {
		return capabilities
	}
	return p.defaultCapabilities
}

func (p *FakeProvider) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("fake provider: nil chat request")
	}

	// ChatErrors 用来模拟 429、5xx、timeout 等上游失败。
	// 测试中通常会配置 llm.ErrUpstream 或包装它的错误，验证 resilience 层可以 errors.Is 识别。
	if err, ok := p.chatErrors[req.Model]; ok {
		return nil, err
	}

	response, ok := p.chatResponses[req.Model]
	if !ok {
		return nil, fmt.Errorf("fake provider: no chat response for model %q", req.Model)
	}
	cloned := cloneChatResponse(response)
	return &cloned, nil
}

func (p *FakeProvider) ChatStream(ctx context.Context, req *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("fake provider: nil stream request")
	}
	if err, ok := p.streamErrors[req.Model]; ok {
		return nil, err
	}

	chunks, ok := p.streamChunks[req.Model]
	if !ok {
		return nil, fmt.Errorf("fake provider: no stream chunks for model %q", req.Model)
	}

	out := make(chan llm.ChatChunk, len(chunks))
	go func() {
		defer close(out)

		for _, chunk := range chunks {
			// 每个 chunk 发送前都检查 context，是为了模拟真实流式传输里的取消语义：
			// 用户断开连接或请求超时后，provider 应尽快停止继续生产 token。
			if err := ctx.Err(); err != nil {
				out <- llm.ChatChunk{Err: err}
				return
			}
			out <- cloneChatChunk(chunk)
		}
	}()

	return out, nil
}

func cloneCapabilitiesMap(input map[string]llm.ProviderCapabilities) map[string]llm.ProviderCapabilities {
	if input == nil {
		return map[string]llm.ProviderCapabilities{}
	}

	cloned := make(map[string]llm.ProviderCapabilities, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func cloneChatResponseMap(input map[string]llm.ChatResponse) map[string]llm.ChatResponse {
	if input == nil {
		return map[string]llm.ChatResponse{}
	}

	cloned := make(map[string]llm.ChatResponse, len(input))
	for key, value := range input {
		cloned[key] = cloneChatResponse(value)
	}
	return cloned
}

func cloneErrorMap(input map[string]error) map[string]error {
	if input == nil {
		return map[string]error{}
	}

	cloned := make(map[string]error, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func cloneStreamChunkMap(input map[string][]llm.ChatChunk) map[string][]llm.ChatChunk {
	if input == nil {
		return map[string][]llm.ChatChunk{}
	}

	cloned := make(map[string][]llm.ChatChunk, len(input))
	for key, value := range input {
		cloned[key] = cloneChatChunks(value)
	}
	return cloned
}

func cloneChatResponse(input llm.ChatResponse) llm.ChatResponse {
	cloned := input
	// ProviderUsage 只有私有不可变摘要，值拷贝不暴露可写别名。
	cloned.ToolCalls = cloneToolCalls(input.ToolCalls)
	return cloned
}

func cloneChatChunks(input []llm.ChatChunk) []llm.ChatChunk {
	if input == nil {
		return nil
	}

	cloned := make([]llm.ChatChunk, len(input))
	for i, chunk := range input {
		cloned[i] = cloneChatChunk(chunk)
	}
	return cloned
}

func cloneChatChunk(input llm.ChatChunk) llm.ChatChunk {
	cloned := input
	if input.DeltaToolCall != nil {
		toolCall := cloneToolCall(*input.DeltaToolCall)
		cloned.DeltaToolCall = &toolCall
	}
	if input.Usage != nil {
		usage := *input.Usage
		cloned.Usage = &usage
	}
	return cloned
}

func cloneToolCalls(input []llm.ToolCall) []llm.ToolCall {
	if input == nil {
		return nil
	}

	cloned := make([]llm.ToolCall, len(input))
	for i, toolCall := range input {
		cloned[i] = cloneToolCall(toolCall)
	}
	return cloned
}

func cloneToolCall(input llm.ToolCall) llm.ToolCall {
	cloned := input
	cloned.Arguments = cloneMap(input.Arguments)
	return cloned
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}

	cloned := make(map[string]any, len(input))
	for key, value := range input {
		cloned[key] = cloneValue(value)
	}
	return cloned
}

// cloneValue 递归复制 tool arguments 中的 JSON-like 值。
//
// ToolCall.Arguments 往往包含嵌套 object/array。浅拷贝只能隔离最外层 map，
// 仍会让并行测试通过嵌套值互相污染，掩盖 Agent executor 的状态问题。
func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
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
