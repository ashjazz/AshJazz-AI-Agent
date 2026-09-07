package llm_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	"github.com/ashjazz/Longtermism/pkg/ai/llm/openai"
	"github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
)

// replacementProviderFactory 为同一组契约场景创建可替换 provider。
//
// T086 关注的是“替换后上层能力预期不变”：fake provider 用于默认离线测试，
// OpenAI-compatible adapter 用于真实 HTTP/JSON/SSE 协议映射；两者不能把不同的
// content、tool call、usage、错误分类或流式结束语义暴露给 llm.Provider 调用方。
type replacementProviderFactory struct {
	name string
	new  func(t *testing.T) llm.Provider
}

func TestProviderAdaptersAreReplaceable(t *testing.T) {
	t.Parallel()

	factories := []replacementProviderFactory{
		{name: "fake", new: newReplacementFakeProvider},
		{name: "openai-compatible", new: newReplacementOpenAIProvider},
	}

	t.Run("capabilities stay consistent", func(t *testing.T) {
		t.Parallel()

		for _, factory := range factories {
			factory := factory
			t.Run(factory.name, func(t *testing.T) {
				t.Parallel()

				got := factory.new(t).Capabilities("chat-model")
				if got != replacementCapabilities() {
					t.Fatalf("Capabilities() = %#v, want %#v", got, replacementCapabilities())
				}
			})
		}
	})

	t.Run("chat response stays consistent", func(t *testing.T) {
		t.Parallel()

		for _, factory := range factories {
			factory := factory
			t.Run(factory.name, func(t *testing.T) {
				t.Parallel()

				got, err := factory.new(t).Chat(context.Background(), replacementChatRequest())
				if err != nil {
					t.Fatalf("Chat() error = %v", err)
				}
				assertChatResponse(t, got, replacementChatResponse())
			})
		}
	})

	t.Run("tool call response stays consistent", func(t *testing.T) {
		t.Parallel()

		for _, factory := range factories {
			factory := factory
			t.Run(factory.name, func(t *testing.T) {
				t.Parallel()

				got, err := factory.new(t).Chat(context.Background(), replacementToolRequest())
				if err != nil {
					t.Fatalf("Chat() tool call error = %v", err)
				}
				if len(got.ToolCalls) != 1 {
					t.Fatalf("ToolCalls length = %d, want 1", len(got.ToolCalls))
				}
				assertToolCall(t, got.ToolCalls[0], replacementToolCall())
			})
		}
	})

	t.Run("stream response stays consistent", func(t *testing.T) {
		t.Parallel()

		for _, factory := range factories {
			factory := factory
			t.Run(factory.name, func(t *testing.T) {
				t.Parallel()

				deltas, usage, finishReason := collectReplacementStream(t, factory.new(t), replacementStreamRequest())
				assertStringSlice(t, deltas, []string{"hel", "lo"})
				if usage == nil || *usage != replacementUsage() {
					t.Fatalf("stream usage = %#v, want %#v", usage, replacementUsage())
				}
				if finishReason != llm.FinishStop {
					t.Fatalf("stream finish reason = %q, want %q", finishReason, llm.FinishStop)
				}
			})
		}
	})

	t.Run("stream tool call response stays consistent", func(t *testing.T) {
		t.Parallel()

		for _, factory := range factories {
			factory := factory
			t.Run(factory.name, func(t *testing.T) {
				t.Parallel()

				chunks, err := factory.new(t).ChatStream(context.Background(), replacementStreamToolRequest())
				if err != nil {
					t.Fatalf("ChatStream() tool call error = %v", err)
				}

				var sawToolFinish bool
				var gotCalls []llm.ToolCall
				for chunk := range chunks {
					if chunk.Err != nil {
						t.Fatalf("ChatStream() tool call chunk error = %v", chunk.Err)
					}
					if chunk.FinishReason == llm.FinishToolCall {
						sawToolFinish = true
					}
					if chunk.DeltaToolCall != nil {
						if !sawToolFinish {
							t.Fatal("ChatStream() emitted tool call before finish_reason=tool_calls")
						}
						gotCalls = append(gotCalls, *chunk.DeltaToolCall)
					}
				}
				if len(gotCalls) != 1 {
					t.Fatalf("stream tool calls length = %d, want 1", len(gotCalls))
				}
				assertToolCall(t, gotCalls[0], replacementToolCall())
			})
		}
	})

	t.Run("upstream error classification stays consistent", func(t *testing.T) {
		t.Parallel()

		for _, factory := range factories {
			factory := factory
			t.Run(factory.name, func(t *testing.T) {
				t.Parallel()

				_, err := factory.new(t).Chat(context.Background(), replacementUpstreamRequest())
				if !errors.Is(err, llm.ErrUpstream) {
					t.Fatalf("Chat() upstream error = %v, want errors.Is ErrUpstream", err)
				}
			})
		}
	})

	t.Run("context cancellation stays consistent", func(t *testing.T) {
		t.Parallel()

		for _, factory := range factories {
			factory := factory
			t.Run(factory.name, func(t *testing.T) {
				t.Parallel()

				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				startedAt := time.Now()
				_, err := factory.new(t).Chat(ctx, replacementCancelRequest())
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Chat() cancel error = %v, want context.Canceled", err)
				}
				if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
					t.Fatalf("Chat() cancel took %s, want under 100ms", elapsed)
				}
			})
		}
	})
}

func newReplacementFakeProvider(t *testing.T) llm.Provider {
	t.Helper()

	return testutil.NewFakeProvider(testutil.FakeProviderConfig{
		Name: "fake-replacement-provider",
		Capabilities: map[string]llm.ProviderCapabilities{
			"chat-model":        replacementCapabilities(),
			"tool-model":        replacementCapabilities(),
			"stream-model":      replacementCapabilities(),
			"stream-tool-model": replacementCapabilities(),
			"upstream-model":    replacementCapabilities(),
			"cancel-model":      replacementCapabilities(),
		},
		ChatResponses: map[string]llm.ChatResponse{
			"chat-model":   replacementChatResponse(),
			"tool-model":   replacementToolResponse(),
			"cancel-model": replacementCancelResponse(),
		},
		ChatErrors: map[string]error{
			"upstream-model": llm.ErrUpstream,
		},
		StreamChunks: map[string][]llm.ChatChunk{
			"stream-model": {
				{DeltaContent: "hel"},
				{DeltaContent: "lo"},
				{FinishReason: llm.FinishStop, Usage: usagePointer(replacementUsage())},
			},
			"stream-tool-model": {
				{FinishReason: llm.FinishToolCall},
				{DeltaToolCall: toolCallPointer(replacementToolCall())},
			},
		},
	})
}

func newReplacementOpenAIProvider(t *testing.T) llm.Provider {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleOpenAIContractRequest(t, w, r)
	}))
	t.Cleanup(server.Close)

	provider, err := openai.NewProvider(openai.Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "chat-model",
		Capabilities: map[string]llm.ProviderCapabilities{
			"chat-model":        replacementCapabilities(),
			"tool-model":        replacementCapabilities(),
			"stream-model":      replacementCapabilities(),
			"stream-tool-model": replacementCapabilities(),
			"upstream-model":    replacementCapabilities(),
			"cancel-model":      replacementCapabilities(),
		},
	})
	if err != nil {
		t.Fatalf("openai.NewProvider() error = %v", err)
	}
	return provider
}

func replacementCapabilities() llm.ProviderCapabilities {
	return llm.ProviderCapabilities{
		ToolCalling:         true,
		StrictStructuredOut: true,
		Streaming:           true,
		StreamingToolCall:   true,
	}
}

func replacementUsage() llm.Usage {
	return llm.Usage{
		InputTokens:  7,
		OutputTokens: 5,
		TotalTokens:  12,
	}
}

func replacementToolCall() llm.ToolCall {
	return llm.ToolCall{
		ID:   "call_weather",
		Name: "get_weather",
		Arguments: map[string]any{
			"city": "Shanghai",
		},
	}
}

func replacementChatRequest() *llm.ChatRequest {
	return &llm.ChatRequest{
		Model:    "chat-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
	}
}

func replacementChatResponse() llm.ChatResponse {
	return llm.ChatResponse{
		Content:      "hello from openai adapter",
		Model:        "chat-model",
		Usage:        testutil.MustReportedUsage(replacementUsage()),
		FinishReason: llm.FinishStop,
	}
}

func replacementToolRequest() *llm.ChatRequest {
	return &llm.ChatRequest{
		Model:    "tool-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "weather"}},
		Tools: []llm.Tool{
			{
				Name:        "get_weather",
				Description: "Get weather by city.",
				Parameters:  map[string]any{"type": "object"},
			},
		},
	}
}

func replacementToolResponse() llm.ChatResponse {
	return llm.ChatResponse{
		Model:        "tool-model",
		Usage:        testutil.MustReportedUsage(replacementUsage()),
		FinishReason: llm.FinishToolCall,
		ToolCalls:    []llm.ToolCall{replacementToolCall()},
	}
}

func replacementStreamRequest() *llm.ChatRequest {
	return &llm.ChatRequest{
		Model:    "stream-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "stream"}},
	}
}

func replacementStreamToolRequest() *llm.ChatRequest {
	return &llm.ChatRequest{
		Model:    "stream-tool-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "stream weather tool"}},
		Tools: []llm.Tool{
			{
				Name:        "get_weather",
				Description: "Get weather by city.",
				Parameters:  map[string]any{"type": "object"},
			},
		},
	}
}

func replacementUpstreamRequest() *llm.ChatRequest {
	return &llm.ChatRequest{
		Model:    "upstream-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "fail"}},
	}
}

func replacementCancelRequest() *llm.ChatRequest {
	return &llm.ChatRequest{
		Model:    "cancel-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "cancel"}},
	}
}

func replacementCancelResponse() llm.ChatResponse {
	return llm.ChatResponse{
		Content:      "should not be returned after cancel",
		Model:        "cancel-model",
		Usage:        testutil.MustReportedUsage(replacementUsage()),
		FinishReason: llm.FinishStop,
	}
}

func collectReplacementStream(t *testing.T, provider llm.Provider, req *llm.ChatRequest) ([]string, *llm.Usage, llm.FinishReason) {
	t.Helper()

	chunks, err := provider.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}

	var deltas []string
	var usage *llm.Usage
	var finishReason llm.FinishReason
	for chunk := range chunks {
		if chunk.Err != nil {
			t.Fatalf("ChatStream() chunk error = %v", chunk.Err)
		}
		if chunk.DeltaContent != "" {
			deltas = append(deltas, chunk.DeltaContent)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
	}
	return deltas, usage, finishReason
}

func usagePointer(usage llm.Usage) *llm.Usage {
	return &usage
}

func toolCallPointer(toolCall llm.ToolCall) *llm.ToolCall {
	return &toolCall
}
