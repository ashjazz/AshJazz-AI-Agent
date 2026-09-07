package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
)

const contextCancelTestTimeout = 500 * time.Millisecond

func TestNewProviderValidatesRequiredConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		config    Config
		wantField string
	}{
		{
			name: "缺少 baseURL 应快速失败",
			config: Config{
				APIKey:       "test-api-key",
				DefaultModel: "test-model",
			},
			wantField: "baseURL",
		},
		{
			name: "缺少 default model 应快速失败",
			config: Config{
				BaseURL: "https://api.example.test/v1",
				APIKey:  "test-api-key",
			},
			wantField: "model",
		},
		{
			name: "缺少 API key 应快速失败",
			config: Config{
				BaseURL:      "https://api.example.test/v1",
				DefaultModel: "test-model",
			},
			wantField: "apiKey",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Provider 初始化阶段先校验稳定配置，避免到真正发起请求时才发现
			// baseURL 或默认模型为空。生产环境里这类错误应在启动或装配阶段暴露，
			// 而不是等到用户请求进入 LLM 路径后才变成难定位的上游失败。
			provider, err := NewProvider(tt.config)
			if err == nil {
				t.Fatalf("NewProvider() error = nil, want missing %s error", tt.wantField)
			}
			if provider != nil {
				t.Fatalf("NewProvider() provider = %#v, want nil on invalid config", provider)
			}
			assertErrorMentions(t, err, tt.wantField)
		})
	}
}

func TestProviderChatValidatesRequiredRequest(t *testing.T) {
	t.Parallel()

	provider, err := NewProvider(Config{
		BaseURL:      "https://api.example.test/v1",
		APIKey:       "test-api-key",
		DefaultModel: "test-model",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	tests := []struct {
		name      string
		request   *llm.ChatRequest
		wantField string
	}{
		{
			name: "缺少 model 应快速失败",
			request: &llm.ChatRequest{
				Messages: []llm.Message{
					{Role: llm.RoleUser, Content: "hello"},
				},
			},
			wantField: "model",
		},
		{
			name: "缺少 messages 应快速失败",
			request: &llm.ChatRequest{
				Model: "test-model",
			},
			wantField: "messages",
		},
		{
			name:      "nil request 应快速失败",
			request:   nil,
			wantField: "request",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Chat 请求校验属于调用边界校验：缺少模型或消息是调用方错误，
			// 不能伪装成 llm.ErrUpstream。否则 resilience 层会错误地重试或熔断，
			// 把本可立即修复的参数问题误判为供应商不可用。
			got, err := provider.Chat(context.Background(), tt.request)
			if err == nil {
				t.Fatalf("Chat() error = nil, want missing %s error", tt.wantField)
			}
			if got != nil {
				t.Fatalf("Chat() response = %#v, want nil on invalid request", got)
			}
			assertErrorMentions(t, err, tt.wantField)
		})
	}
}

func TestProviderNameAndCapabilities(t *testing.T) {
	t.Parallel()

	customCapabilities := llm.ProviderCapabilities{
		ToolCalling:         true,
		StrictStructuredOut: true,
		Streaming:           true,
		ReasoningEffort:     true,
	}
	provider, err := NewProvider(Config{
		BaseURL:      "https://api.example.test/v1",
		APIKey:       "test-api-key",
		DefaultModel: "gpt-capability",
		Capabilities: map[string]llm.ProviderCapabilities{
			"gpt-capability": customCapabilities,
		},
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	if provider.Name() != "openai" {
		t.Fatalf("Name() = %q, want %q", provider.Name(), "openai")
	}
	if got := provider.Capabilities("gpt-capability"); got != customCapabilities {
		t.Fatalf("Capabilities(custom model) = %#v, want %#v", got, customCapabilities)
	}

	// 未配置的模型走 adapter 的保守默认能力声明。P0 阶段不做网络探测，
	// 因此这里宁可少声明能力，也不要让 Agent 路由误以为所有模型都支持高级特性。
	defaultCapabilities := provider.Capabilities("unknown-model")
	if !defaultCapabilities.ToolCalling || !defaultCapabilities.Streaming {
		t.Fatalf("Capabilities(unknown model) = %#v, want basic tool calling and streaming support", defaultCapabilities)
	}
	if !defaultCapabilities.StreamingToolCall {
		t.Fatalf("Capabilities(unknown model) = %#v, want implemented streaming tool call support", defaultCapabilities)
	}
	if defaultCapabilities.PromptCaching || defaultCapabilities.Vision {
		t.Fatalf("Capabilities(unknown model) = %#v, want unsupported advanced capabilities disabled", defaultCapabilities)
	}
}

func TestNewProviderNormalizesStableConfig(t *testing.T) {
	t.Parallel()

	provider, err := NewProvider(Config{
		BaseURL:      " https://api.example.test/v1/ ",
		APIKey:       " test-api-key \n",
		DefaultModel: " gpt-normalized ",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	// 配置常来自环境变量或配置文件，末尾空白很常见。构造阶段统一清洗，
	// 可以避免后续 Authorization header、默认模型名和 baseURL 拼接出现隐蔽问题。
	if provider.baseURL != "https://api.example.test/v1" {
		t.Fatalf("provider baseURL = %q, want normalized URL", provider.baseURL)
	}
	if provider.apiKey != "test-api-key" {
		t.Fatalf("provider apiKey = %q, want trimmed API key", provider.apiKey)
	}
	if provider.defaultModel != "gpt-normalized" {
		t.Fatalf("provider defaultModel = %q, want trimmed model", provider.defaultModel)
	}
}

func TestNewProviderUsesHardenedDefaultHTTPClient(t *testing.T) {
	t.Parallel()

	provider, err := NewProvider(Config{
		BaseURL:      "https://api.example.com/v1",
		APIKey:       "test-api-key",
		DefaultModel: "gpt-timeout",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	// 调用方可能传入 context.Background()。provider 自身仍需提供最后一道超时保护，
	// 避免连接、TLS 握手或响应读取永久占用 goroutine 和连接池。
	if provider.httpClient.Timeout != defaultRequestTimeout {
		t.Fatalf("HTTP client timeout = %v, want %v", provider.httpClient.Timeout, defaultRequestTimeout)
	}
	// Authorization 属于当前 base URL 的信任边界，不能因为上游返回 3xx 就被默认
	// client 带到新的请求目标。这里要求 adapter 独立使用时也 fail closed，而不是只
	// 依赖应用装配层恰好注入了更严格的 client。
	if provider.httpClient.CheckRedirect == nil {
		t.Fatal("default HTTP client CheckRedirect = nil, want redirects rejected")
	}
	if err := provider.httpClient.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("default HTTP client CheckRedirect() error = %v, want http.ErrUseLastResponse", err)
	}
}

func TestProviderChatMapsRequestToOpenAICompatiblePayload(t *testing.T) {
	t.Parallel()

	requestSeen := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("request method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("request path = %s, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-api-key" {
			t.Fatalf("Authorization header = %q, want bearer token", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode request payload: %v", err)
		}
		requestSeen <- payload

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl_mapping_test",
			"object":  "chat.completion",
			"model":   "gpt-request-mapping",
			"choices": []map[string]any{},
			"usage": map[string]any{
				"prompt_tokens":     0,
				"completion_tokens": 0,
				"total_tokens":      0,
			},
		})
		if err != nil {
			t.Fatalf("failed to encode placeholder response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(Config{
		BaseURL:      server.URL + "/v1",
		APIKey:       "test-api-key",
		DefaultModel: "gpt-default",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	temperature := 0.2
	_, err = provider.Chat(context.Background(), &llm.ChatRequest{
		Model: "gpt-request-mapping",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You are concise."},
			{Role: llm.RoleUser, Content: "hello"},
			{Role: llm.RoleTool, Content: `{"ok":true}`, ToolCallID: "call_weather_001"},
		},
		Tools: []llm.Tool{
			{
				Name:        "get_weather",
				Description: "Get weather by city.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
				},
				Strict: true,
			},
		},
		StructuredOutput: &llm.StructuredOutput{
			Name:   "answer_schema",
			Schema: map[string]any{"type": "object"},
			Strict: true,
		},
		ReasoningEffort: "medium",
		Temperature:     &temperature,
		MaxTokens:       128,
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	var payload map[string]any
	select {
	case payload = <-requestSeen:
	case <-time.After(contextCancelTestTimeout):
		t.Fatal("server did not receive mapped Chat request before timeout")
	}

	// 这个测试把内部 llm.ChatRequest 到 OpenAI-compatible JSON 的边界钉住。
	// 后续 T031/T032 可以替换响应解析和错误分类，但不能悄悄改变请求协议。
	assertPayloadString(t, payload, "model", "gpt-request-mapping")
	assertPayloadFloat(t, payload, "temperature", 0.2)
	assertPayloadFloat(t, payload, "max_tokens", 128)
	assertPayloadString(t, payload, "reasoning_effort", "medium")
	assertRequestMessages(t, payload)
	assertRequestTools(t, payload)
	assertRequestStructuredOutput(t, payload)
}

func TestProviderChatPreservesExplicitZeroTemperature(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request payload: %v", err)
		}

		assertPayloadFloat(t, payload, "temperature", 0)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"model": "gpt-zero-temperature",
			"choices": []map[string]any{
				{
					"message":       map[string]any{"content": "deterministic"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     0,
				"completion_tokens": 0,
				"total_tokens":      0,
			},
		}); err != nil {
			t.Fatalf("encode response payload: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	temperature := 0.0
	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "gpt-zero-temperature",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	_, err = provider.Chat(context.Background(), &llm.ChatRequest{
		Model:       "gpt-zero-temperature",
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "be deterministic"}},
		Temperature: &temperature,
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
}

func TestProviderChatParsesSuccessfulResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// 这里返回的是 OpenAI-compatible Chat Completions 的典型响应形态。
		// 测试重点不是供应商原始 JSON 本身，而是 adapter 必须把它稳定映射到
		// 框架内部的 llm.ChatResponse：content/model/finish_reason/usage。
		err := json.NewEncoder(w).Encode(map[string]any{
			"id":     "chatcmpl_test",
			"object": "chat.completion",
			"model":  "gpt-test-actual",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "hello from adapter",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     9,
				"completion_tokens": 4,
				"total_tokens":      13,
			},
		})
		if err != nil {
			t.Fatalf("failed to encode test response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "gpt-test",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	got, err := provider.Chat(context.Background(), &llm.ChatRequest{
		Model: "gpt-test",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "say hello"},
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got == nil {
		t.Fatal("Chat() response = nil, want parsed response")
	}

	// 这些字段会被后续 obs trace、eval report、成本控制和 Agent executor 共同消费。
	// 因此 adapter 不能只返回 content，而要保留模型实际返回身份、结束原因和 token 用量。
	if got.Content != "hello from adapter" {
		t.Fatalf("Chat() content = %q, want %q", got.Content, "hello from adapter")
	}
	if got.Model != "gpt-test-actual" {
		t.Fatalf("Chat() model = %q, want %q", got.Model, "gpt-test-actual")
	}
	if got.FinishReason != llm.FinishStop {
		t.Fatalf("Chat() finish reason = %q, want %q", got.FinishReason, llm.FinishStop)
	}
	assertReportedUsageForTest(t, got, llm.Usage{
		InputTokens:  9,
		OutputTokens: 4,
		TotalTokens:  13,
	})
}

func TestProviderChatParsesToolCallResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// OpenAI-compatible 协议中，tool/function arguments 通常是一个 JSON 字符串。
		// adapter 的责任是把它解析成结构化 map，交给 Agent executor 使用；
		// executor 不应该再去拼字符串或解析自然语言式 Thought/Action。
		err := json.NewEncoder(w).Encode(map[string]any{
			"id":     "chatcmpl_tool_test",
			"object": "chat.completion",
			"model":  "gpt-tool-actual",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{
							{
								"id":   "call_weather_001",
								"type": "function",
								"function": map[string]any{
									"name":      "get_weather",
									"arguments": `{"city":"Shanghai","unit":"celsius"}`,
								},
							},
						},
					},
					"finish_reason": "tool_calls",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     15,
				"completion_tokens": 8,
				"total_tokens":      23,
			},
		})
		if err != nil {
			t.Fatalf("failed to encode test response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "gpt-tool",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	got, err := provider.Chat(context.Background(), &llm.ChatRequest{
		Model: "gpt-tool",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "weather in Shanghai"},
		},
		Tools: []llm.Tool{
			{
				Name:        "get_weather",
				Description: "Get weather by city.",
				Parameters: map[string]any{
					"type": "object",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got == nil {
		t.Fatal("Chat() response = nil, want parsed tool call response")
	}
	if got.FinishReason != llm.FinishToolCall {
		t.Fatalf("Chat() finish reason = %q, want %q", got.FinishReason, llm.FinishToolCall)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("Chat() tool calls length = %d, want 1", len(got.ToolCalls))
	}
	assertToolCall(t, got.ToolCalls[0], llm.ToolCall{
		ID:   "call_weather_001",
		Name: "get_weather",
		Arguments: map[string]any{
			"city": "Shanghai",
			"unit": "celsius",
		},
	})
}

func TestProviderChatRequiresExplicitConsistentUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		includeUsage bool
		usageJSON    string
		wantUsage    llm.Usage
		wantSuccess  bool
	}{
		{name: "missing usage", includeUsage: false},
		{name: "null usage", includeUsage: true, usageJSON: `null`},
		{name: "empty usage object", includeUsage: true, usageJSON: `{}`},
		{name: "missing prompt tokens", includeUsage: true, usageJSON: `{"completion_tokens":4,"total_tokens":4}`},
		{name: "missing completion tokens", includeUsage: true, usageJSON: `{"prompt_tokens":9,"total_tokens":9}`},
		{name: "missing total tokens", includeUsage: true, usageJSON: `{"prompt_tokens":9,"completion_tokens":4}`},
		{name: "negative prompt tokens", includeUsage: true, usageJSON: `{"prompt_tokens":-1,"completion_tokens":4,"total_tokens":3}`},
		{name: "negative completion tokens", includeUsage: true, usageJSON: `{"prompt_tokens":9,"completion_tokens":-1,"total_tokens":8}`},
		{name: "negative total tokens", includeUsage: true, usageJSON: `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":-1}`},
		{name: "total below components", includeUsage: true, usageJSON: `{"prompt_tokens":9,"completion_tokens":4,"total_tokens":12}`},
		{name: "total above components", includeUsage: true, usageJSON: `{"prompt_tokens":9,"completion_tokens":4,"total_tokens":14}`},
		{name: "usage exceeds safe bound", includeUsage: true, usageJSON: `{"prompt_tokens":100000001,"completion_tokens":0,"total_tokens":100000001}`},
		{
			name: "explicit zero usage", includeUsage: true,
			usageJSON: `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`,
			wantUsage: llm.Usage{}, wantSuccess: true,
		},
		{
			name: "standard nonzero usage", includeUsage: true,
			usageJSON: `{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}`,
			wantUsage: llm.Usage{InputTokens: 9, OutputTokens: 4, TotalTokens: 13}, wantSuccess: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := openAIChatResponseWithUsageForTest(tt.includeUsage, tt.usageJSON)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write([]byte(body)); err != nil {
					t.Errorf("write usage fixture: %v", err)
				}
			}))
			t.Cleanup(server.Close)

			provider, err := NewProvider(Config{
				BaseURL: server.URL, APIKey: "secret-t205-api-key", DefaultModel: "request-model-t205",
			})
			if err != nil {
				t.Fatal("NewProvider() returned an unexpected error")
			}
			got, chatErr := provider.Chat(context.Background(), &llm.ChatRequest{
				Model: "request-model-t205", Messages: []llm.Message{{Role: llm.RoleUser, Content: "request-t205"}},
			})

			if tt.wantSuccess {
				if chatErr != nil || got == nil {
					t.Fatalf("Chat() response_present=%v error_present=%v, want true/false", got != nil, chatErr != nil)
				}
				assertReportedUsageForTest(t, got, tt.wantUsage)
				return
			}
			if chatErr == nil || got != nil {
				t.Errorf("Chat() response_present=%v error_present=%v, want false/true", got != nil, chatErr != nil)
			}
			if chatErr != nil {
				var classified interface{ Class() string }
				if !errors.As(chatErr, &classified) || classified.Class() != "invalid_response" {
					t.Error("Chat() error lacks machine-readable invalid_response classification")
				}
			}
			for index, forbidden := range []string{
				"raw-t205-provider-body", "secret-t205-api-key", server.URL,
				"Bearer", "actual-model-t205", "request-model-t205", "request-t205",
			} {
				if strings.Contains(strings.ToLower(chatErrString(chatErr)), strings.ToLower(forbidden)) {
					t.Errorf("usage error leaked forbidden category %d", index)
				}
			}
		})
	}
}

// 成功状态码不使正文可信：超大响应、第二个 JSON 和工具参数错误都必须在
// adapter 内终止，不能让正文或 provider 控制的 tool-call ID 进入错误链。
func TestProviderChatRejectsUnsafeSuccessfulBody(t *testing.T) {
	t.Parallel()
	valid := openAIChatResponseWithUsageForTest(true, `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`)
	tests := []struct{ name, body string }{
		{"oversized content", strings.Replace(valid, "raw-t205-provider-body", strings.Repeat("x", (1<<20)+1), 1)},
		{"second JSON", valid + `{ "secret-upstream-marker": true }`},
		{"trailing garbage", valid + "secret-upstream-marker"},
		{"invalid tool arguments", `{"choices":[{"message":{"tool_calls":[{"id":"secret-upstream-marker","function":{"name":"lookup","arguments":"[invalid"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// 超限时客户端主动关流可能导致 write 失败，此处只验证客户端契约。
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)
			provider, err := NewProvider(Config{BaseURL: server.URL, APIKey: "test-key", DefaultModel: "test-model"})
			if err != nil {
				t.Fatal(err)
			}
			response, err := provider.Chat(context.Background(), &llm.ChatRequest{
				Model: "test-model", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
			})
			if response != nil || !errors.Is(err, llm.ErrInvalidResponse) {
				t.Fatalf("Chat() response_present=%v error=%v, want nil/invalid response", response != nil, err)
			}
			var classified interface{ Class() string }
			if !errors.As(err, &classified) || classified.Class() != "invalid_response" {
				t.Error("error lacks stable invalid_response class")
			}
			if strings.Contains(err.Error(), "secret-upstream-marker") {
				t.Error("error leaked upstream content")
			}
		})
	}
}

// 响应头已到达后的读失败仍是传输故障，不能误报为 usage/schema 无效；
// 但任意 reader 错误文本也不能越过 adapter。
func TestParseChatResponsePreservesSafeReadFailureCategory(t *testing.T) {
	for _, tt := range []struct {
		name     string
		cause    error
		want     error
		upstream bool
	}{
		{"cancelled", context.Canceled, context.Canceled, false},
		{"deadline", context.DeadlineExceeded, context.DeadlineExceeded, true},
		{"connection interrupted", io.ErrUnexpectedEOF, llm.ErrUpstream, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response, err := parseChatResponse(&http.Response{StatusCode: http.StatusOK,
				Body: io.NopCloser(chatBodyErrorReader{fmt.Errorf("secret-upstream-marker: %w", tt.cause)}),
			})
			if response != nil || !errors.Is(err, tt.want) || errors.Is(err, llm.ErrUpstream) != tt.upstream {
				t.Fatalf("read failure lost category: response_present=%v error=%v", response != nil, err)
			}
			if errors.Is(err, llm.ErrInvalidResponse) || strings.Contains(err.Error(), "secret-upstream-marker") {
				t.Fatal("read failure misclassified or raw error leaked")
			}
		})
	}
}

type chatBodyErrorReader struct{ err error }

func (reader chatBodyErrorReader) Read([]byte) (int, error) { return 0, reader.err }

func TestProviderChatPreservesOptionalResponseDefaultsWithExplicitZeroUsage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// content 与 finish_reason 在兼容供应商中可能缺省，但 usage 是非流式成功
		// 契约的必需事实。显式零对象和字段缺失不能共享同一个 Go 零值语义。
		err := json.NewEncoder(w).Encode(map[string]any{
			"id":     "chatcmpl_defaults_test",
			"object": "chat.completion",
			"model":  "gpt-defaults-actual",
			"choices": []map[string]any{
				{
					"index":   0,
					"message": map[string]any{"role": "assistant"},
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     0,
				"completion_tokens": 0,
				"total_tokens":      0,
			},
		})
		if err != nil {
			t.Fatalf("failed to encode default response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "gpt-defaults",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	got, err := provider.Chat(context.Background(), &llm.ChatRequest{
		Model: "gpt-defaults",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "return defaults"},
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got == nil {
		t.Fatal("Chat() response = nil, want response with defaults")
	}
	if got.Model != "gpt-defaults-actual" {
		t.Fatalf("Chat() model = %q, want %q", got.Model, "gpt-defaults-actual")
	}
	if got.Content != "" {
		t.Fatalf("Chat() content = %q, want empty default", got.Content)
	}
	if got.FinishReason != "" {
		t.Fatalf("Chat() finish reason = %q, want empty default", got.FinishReason)
	}
	assertReportedUsageForTest(t, got, llm.Usage{})
}

func openAIChatResponseWithUsageForTest(includeUsage bool, usageJSON string) string {
	fields := []string{
		`"id":"chatcmpl_usage_t205"`,
		`"object":"chat.completion"`,
		`"model":"actual-model-t205"`,
		`"choices":[{"index":0,"message":{"role":"assistant","content":"raw-t205-provider-body"},"finish_reason":"stop"}]`,
		`"synthetic_canary":"raw-t205-provider-body"`,
	}
	if includeUsage {
		fields = append(fields, `"usage":`+usageJSON)
	}
	return "{" + strings.Join(fields, ",") + "}"
}

func chatErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func assertReportedUsageForTest(t *testing.T, response *llm.ChatResponse, want llm.Usage) {
	t.Helper()

	providerUsage := providerUsageValueForTest(t, response)
	availability, summary := providerUsageFieldsForTest(t, providerUsage)
	if availability.String() != "reported" {
		t.Error("ProviderUsage availability is not reported")
		return
	}
	if summary.IsNil() {
		t.Error("reported ProviderUsage lacks summary")
		return
	}
	got := usageFromReflectValueForTest(t, summary.Elem())
	if got != want {
		t.Errorf("ProviderUsage summary = %#v, want %#v", got, want)
	}
}

func providerUsageValueForTest(t *testing.T, response *llm.ChatResponse) reflect.Value {
	t.Helper()
	responseValue := reflect.ValueOf(response).Elem()
	responseType := responseValue.Type()
	usageType := reflect.TypeOf(llm.Usage{})
	usagePointerType := reflect.TypeOf((*llm.Usage)(nil))
	var providerUsage reflect.Value
	providerUsageCount := 0
	hasSiblingSummary := false
	for index := 0; index < responseType.NumField(); index++ {
		fieldType := responseType.Field(index).Type
		if fieldType.Kind() == reflect.Struct && fieldType.Name() == "ProviderUsage" {
			providerUsage = responseValue.Field(index)
			providerUsageCount++
			continue
		}
		if fieldType == usageType || fieldType == usagePointerType {
			hasSiblingSummary = true
		}
	}
	if providerUsageCount != 1 || !providerUsage.IsValid() {
		t.Fatal("ChatResponse must carry exactly one ProviderUsage value object")
	}
	if hasSiblingSummary {
		t.Fatal("ChatResponse must not retain a sibling Usage summary beside ProviderUsage")
	}
	return providerUsage
}

func providerUsageFieldsForTest(t *testing.T, providerUsage reflect.Value) (reflect.Value, reflect.Value) {
	t.Helper()
	var availability reflect.Value
	var summary reflect.Value
	availabilityCount := 0
	summaryCount := 0
	for index := 0; index < providerUsage.NumField(); index++ {
		field := providerUsage.Field(index)
		if field.Kind() == reflect.String && field.Type().Name() != "" {
			availability = field
			availabilityCount++
		}
		if field.Type() == reflect.TypeOf((*llm.Usage)(nil)) {
			summary = field
			summaryCount++
		}
	}
	if availabilityCount != 1 || summaryCount != 1 || !availability.IsValid() || !summary.IsValid() {
		t.Fatal("ProviderUsage must carry exactly one availability enum and one optional summary")
	}
	return availability, summary
}

func usageFromReflectValueForTest(t *testing.T, usage reflect.Value) llm.Usage {
	t.Helper()
	read := func(name string) int {
		field := usage.FieldByName(name)
		if !field.IsValid() || field.Kind() != reflect.Int {
			t.Fatalf("ProviderUsage summary lacks integer %s", name)
		}
		return int(field.Int())
	}
	return llm.Usage{
		InputTokens: read("InputTokens"), OutputTokens: read("OutputTokens"),
		ReasoningTokens: read("ReasoningTokens"), CacheReadTokens: read("CacheReadTokens"),
		CacheWriteTokens: read("CacheWriteTokens"), TotalTokens: read("TotalTokens"),
	}
}

func TestProviderChatParsesFinishReasonVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		rawReason     string
		wantReason    llm.FinishReason
		wantContent   string
		wantUsage     llm.Usage
		responseModel string
	}{
		{
			name:          "stop 映射为正常结束",
			rawReason:     "stop",
			wantReason:    llm.FinishStop,
			wantContent:   "completed answer",
			responseModel: "gpt-stop-actual",
			wantUsage: llm.Usage{
				InputTokens:  10,
				OutputTokens: 5,
				TotalTokens:  15,
			},
		},
		{
			name:          "length 映射为长度截断",
			rawReason:     "length",
			wantReason:    llm.FinishLength,
			wantContent:   "truncated answer",
			responseModel: "gpt-length-actual",
			wantUsage: llm.Usage{
				InputTokens:  11,
				OutputTokens: 6,
				TotalTokens:  17,
			},
		},
		{
			name:          "content_filter 映射为内容过滤",
			rawReason:     "content_filter",
			wantReason:    llm.FinishContentFilter,
			wantContent:   "",
			responseModel: "gpt-filter-actual",
			wantUsage: llm.Usage{
				InputTokens:  12,
				OutputTokens: 0,
				TotalTokens:  12,
			},
		},
		{
			name:          "未知结束原因保留原始值",
			rawReason:     "provider_custom_reason",
			wantReason:    llm.FinishReason("provider_custom_reason"),
			wantContent:   "custom reason answer",
			responseModel: "gpt-custom-actual",
			wantUsage: llm.Usage{
				InputTokens:  13,
				OutputTokens: 7,
				TotalTokens:  20,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				// 这里专门验证“有值响应”的稳定映射，而不是缺省值兜底。
				// finish_reason 会影响上层是否重试、是否提示用户被截断、是否进入内容安全路径；
				// 因此不能只测 stop happy path。
				err := json.NewEncoder(w).Encode(map[string]any{
					"id":     "chatcmpl_reason_test",
					"object": "chat.completion",
					"model":  tt.responseModel,
					"choices": []map[string]any{
						{
							"index": 0,
							"message": map[string]any{
								"role":    "assistant",
								"content": tt.wantContent,
							},
							"finish_reason": tt.rawReason,
						},
					},
					"usage": map[string]any{
						"prompt_tokens":     tt.wantUsage.InputTokens,
						"completion_tokens": tt.wantUsage.OutputTokens,
						"total_tokens":      tt.wantUsage.TotalTokens,
					},
				})
				if err != nil {
					t.Fatalf("failed to encode finish reason response: %v", err)
				}
			}))
			t.Cleanup(server.Close)

			provider, err := NewProvider(Config{
				BaseURL:      server.URL,
				APIKey:       "test-api-key",
				DefaultModel: "gpt-reason",
			})
			if err != nil {
				t.Fatalf("NewProvider() error = %v", err)
			}

			got, err := provider.Chat(context.Background(), &llm.ChatRequest{
				Model: "gpt-reason",
				Messages: []llm.Message{
					{Role: llm.RoleUser, Content: "finish reason"},
				},
			})
			if err != nil {
				t.Fatalf("Chat() error = %v", err)
			}
			if got == nil {
				t.Fatal("Chat() response = nil, want parsed response")
			}
			if got.Model != tt.responseModel {
				t.Fatalf("Chat() model = %q, want %q", got.Model, tt.responseModel)
			}
			if got.Content != tt.wantContent {
				t.Fatalf("Chat() content = %q, want %q", got.Content, tt.wantContent)
			}
			if got.FinishReason != tt.wantReason {
				t.Fatalf("Chat() finish reason = %q, want %q", got.FinishReason, tt.wantReason)
			}
			assertReportedUsageForTest(t, got, tt.wantUsage)
		})
	}
}

func TestProviderChatMapsHTTPStatusErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		statusCode   int
		wantUpstream bool
	}{
		{
			name:         "429 rate limit 是可重试上游错误",
			statusCode:   http.StatusTooManyRequests,
			wantUpstream: true,
		},
		{
			name:         "500 server error 是可重试上游错误",
			statusCode:   http.StatusInternalServerError,
			wantUpstream: true,
		},
		{
			name:         "400 bad request 是调用方错误",
			statusCode:   http.StatusBadRequest,
			wantUpstream: false,
		},
		{
			name:         "401 unauthorized 是配置或认证错误",
			statusCode:   http.StatusUnauthorized,
			wantUpstream: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				err := json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"message": http.StatusText(tt.statusCode),
						"type":    "test_error",
					},
				})
				if err != nil {
					t.Fatalf("failed to encode test error response: %v", err)
				}
			}))
			t.Cleanup(server.Close)

			provider, err := NewProvider(Config{
				BaseURL:      server.URL,
				APIKey:       "test-api-key",
				DefaultModel: "gpt-error",
			})
			if err != nil {
				t.Fatalf("NewProvider() error = %v", err)
			}

			_, err = provider.Chat(context.Background(), &llm.ChatRequest{
				Model: "gpt-error",
				Messages: []llm.Message{
					{Role: llm.RoleUser, Content: "trigger status"},
				},
			})
			if err == nil {
				t.Fatal("Chat() error = nil, want HTTP status mapping error")
			}

			// 这条边界会直接影响 resilience：429/5xx 应进入重试、熔断、降级路径；
			// 400/401 是调用方或配置问题，误包装成 ErrUpstream 会导致无意义重试。
			if errors.Is(err, llm.ErrUpstream) != tt.wantUpstream {
				t.Fatalf("errors.Is(ErrUpstream) = %v, want %v; err = %v", errors.Is(err, llm.ErrUpstream), tt.wantUpstream, err)
			}
		})
	}
}

func TestProviderChatHTTPErrorKeepsOnlyStatusWithoutBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		err := json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "quota exhausted for project",
				"type":    "rate_limit_error",
			},
		})
		if err != nil {
			t.Fatalf("failed to encode diagnostic error response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "secret-test-api-key",
		DefaultModel: "gpt-error",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	_, err = provider.Chat(context.Background(), &llm.ChatRequest{
		Model: "gpt-error",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "trigger diagnostic error"},
		},
	})
	if err == nil {
		t.Fatal("Chat() error = nil, want HTTP diagnostic error")
	}
	if !errors.Is(err, llm.ErrUpstream) {
		t.Fatalf("Chat() error = %v, want errors.Is ErrUpstream", err)
	}

	errorText := err.Error()
	if !strings.Contains(errorText, "429") {
		t.Fatal("HTTP error lost numeric status")
	}
	// 旧断言要求回显供应商 message/type，不能用该不安全契约保护泄露行为。
	for _, forbidden := range []string{"rate_limit_error", "quota exhausted", "secret-test-api-key"} {
		if strings.Contains(errorText, forbidden) {
			t.Fatal("HTTP error leaked body or credential")
		}
	}
}

func TestProviderChatMapsTransportErrorAsUpstream(t *testing.T) {
	t.Parallel()

	provider, err := NewProvider(Config{
		BaseURL:      "https://api.example.test/v1",
		APIKey:       "secret-test-api-key",
		DefaultModel: "gpt-transport",
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				// 这里模拟的是“请求没有拿到 HTTP response”的传输层失败，
				// 例如 DNS、连接拒绝、TLS 握手失败或连接被重置。此时没有 status code
				// 可以分类，所以应按 provider/upstream 不可用处理。
				return nil, errors.New("connection refused")
			}),
		},
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	_, err = provider.Chat(context.Background(), &llm.ChatRequest{
		Model: "gpt-transport",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "trigger transport error"},
		},
	})
	if err == nil {
		t.Fatal("Chat() error = nil, want transport error")
	}
	if !errors.Is(err, llm.ErrUpstream) {
		t.Fatalf("Chat() error = %v, want errors.Is ErrUpstream", err)
	}

	errorText := err.Error()
	for _, want := range []string{"send openai chat request", "connection refused"} {
		if !strings.Contains(errorText, want) {
			t.Fatalf("error = %q, want diagnostic fragment %q", errorText, want)
		}
	}
	if strings.Contains(errorText, "secret-test-api-key") {
		t.Fatalf("error = %q, want no API key leakage", errorText)
	}
}

func TestProviderChatMapsContextDeadlineExceededAsUpstream(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "gpt-timeout",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	// 这里使用已经过期的 deadline，而不是 time.Sleep。这样测试稳定、快速，
	// 也明确表达“调用方超时控制触发后，provider 应把它归入可降级上游错误”。
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err = provider.Chat(ctx, &llm.ChatRequest{
		Model: "gpt-timeout",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "trigger timeout"},
		},
	})
	if !errors.Is(err, llm.ErrUpstream) {
		t.Fatalf("Chat() timeout error = %v, want errors.Is ErrUpstream", err)
	}
}

func TestProviderChatReturnsPromptlyWhenContextCanceled(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{})
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)

		// 服务端一直等到客户端请求上下文取消。这样测试不依赖真实时间等待，
		// 而是验证 provider 是否真的把 ctx 传进 HTTP request；否则 Chat 会卡在这里。
		select {
		case <-r.Context().Done():
		case <-releaseServer:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		close(releaseServer)
	})

	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "gpt-cancel",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := provider.Chat(ctx, &llm.ChatRequest{
			Model: "gpt-cancel",
			Messages: []llm.Message{
				{Role: llm.RoleUser, Content: "cancel me"},
			},
		})
		errCh <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(contextCancelTestTimeout):
		t.Fatal("server did not receive request before timeout")
	}

	cancel()

	select {
	case err := <-errCh:
		// 主动 cancel 是调用方生命周期控制，不应伪装成 provider 故障。
		// 这条边界可以防止 resilience 层对用户已取消的请求做无意义重试。
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Chat() cancel error = %v, want errors.Is context.Canceled", err)
		}
		if errors.Is(err, llm.ErrUpstream) {
			t.Fatalf("Chat() cancel error = %v, want not errors.Is ErrUpstream", err)
		}
	case <-time.After(contextCancelTestTimeout):
		t.Fatal("Chat() did not return promptly after context cancellation")
	}
}

func assertErrorMentions(t *testing.T, err error, want string) {
	t.Helper()

	if err == nil {
		t.Fatalf("error = nil, want message containing %q", want)
	}
	if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
		t.Fatalf("error = %q, want message containing %q", err.Error(), want)
	}
}

// assertUsage 仍由流式协议测试复用；非流式响应通过 ProviderUsage helper 同时验证
// availability 与 token summary，避免只比较数字而漏掉 presence 事实。
func assertUsage(t *testing.T, got llm.Usage, want llm.Usage) {
	t.Helper()

	if got.InputTokens != want.InputTokens {
		t.Fatalf("usage input tokens = %d, want %d", got.InputTokens, want.InputTokens)
	}
	if got.OutputTokens != want.OutputTokens {
		t.Fatalf("usage output tokens = %d, want %d", got.OutputTokens, want.OutputTokens)
	}
	if got.TotalTokens != want.TotalTokens {
		t.Fatalf("usage total tokens = %d, want %d", got.TotalTokens, want.TotalTokens)
	}
}

func assertToolCall(t *testing.T, got llm.ToolCall, want llm.ToolCall) {
	t.Helper()

	if got.ID != want.ID {
		t.Fatalf("tool call id = %q, want %q", got.ID, want.ID)
	}
	if got.Name != want.Name {
		t.Fatalf("tool call name = %q, want %q", got.Name, want.Name)
	}
	for key, wantValue := range want.Arguments {
		gotValue, ok := got.Arguments[key]
		if !ok {
			t.Fatalf("tool call arguments missing key %q", key)
		}
		if gotValue != wantValue {
			t.Fatalf("tool call arguments[%q] = %#v, want %#v", key, gotValue, wantValue)
		}
	}
}

func assertPayloadString(t *testing.T, payload map[string]any, key string, want string) {
	t.Helper()

	got, ok := payload[key].(string)
	if !ok {
		t.Fatalf("payload[%q] = %#v, want string %q", key, payload[key], want)
	}
	if got != want {
		t.Fatalf("payload[%q] = %q, want %q", key, got, want)
	}
}

func assertPayloadFloat(t *testing.T, payload map[string]any, key string, want float64) {
	t.Helper()

	got, ok := payload[key].(float64)
	if !ok {
		t.Fatalf("payload[%q] = %#v, want number %v", key, payload[key], want)
	}
	if got != want {
		t.Fatalf("payload[%q] = %v, want %v", key, got, want)
	}
}

func assertRequestMessages(t *testing.T, payload map[string]any) {
	t.Helper()

	messages, ok := payload["messages"].([]any)
	if !ok {
		t.Fatalf("payload messages = %#v, want array", payload["messages"])
	}
	if len(messages) != 3 {
		t.Fatalf("messages length = %d, want 3", len(messages))
	}

	assertMessagePayload(t, messages[0], map[string]string{
		"role":    string(llm.RoleSystem),
		"content": "You are concise.",
	})
	assertMessagePayload(t, messages[1], map[string]string{
		"role":    string(llm.RoleUser),
		"content": "hello",
	})
	assertMessagePayload(t, messages[2], map[string]string{
		"role":         string(llm.RoleTool),
		"content":      `{"ok":true}`,
		"tool_call_id": "call_weather_001",
	})
}

func assertMessagePayload(t *testing.T, raw any, want map[string]string) {
	t.Helper()

	message, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want object", raw)
	}
	for key, wantValue := range want {
		got, ok := message[key].(string)
		if !ok {
			t.Fatalf("message[%q] = %#v, want string %q", key, message[key], wantValue)
		}
		if got != wantValue {
			t.Fatalf("message[%q] = %q, want %q", key, got, wantValue)
		}
	}
}

func assertRequestTools(t *testing.T, payload map[string]any) {
	t.Helper()

	tools, ok := payload["tools"].([]any)
	if !ok {
		t.Fatalf("payload tools = %#v, want array", payload["tools"])
	}
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1", len(tools))
	}

	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool = %#v, want object", tools[0])
	}
	if tool["type"] != "function" {
		t.Fatalf("tool type = %#v, want function", tool["type"])
	}

	function, ok := tool["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool function = %#v, want object", tool["function"])
	}
	if function["name"] != "get_weather" {
		t.Fatalf("function name = %#v, want get_weather", function["name"])
	}
	if function["description"] != "Get weather by city." {
		t.Fatalf("function description = %#v, want description", function["description"])
	}
	if function["strict"] != true {
		t.Fatalf("function strict = %#v, want true", function["strict"])
	}
	parameters, ok := function["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("function parameters = %#v, want object", function["parameters"])
	}
	if parameters["type"] != "object" {
		t.Fatalf("parameters type = %#v, want object", parameters["type"])
	}
	properties, ok := parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("parameters properties = %#v, want object", parameters["properties"])
	}
	city, ok := properties["city"].(map[string]any)
	if !ok {
		t.Fatalf("properties city = %#v, want object", properties["city"])
	}
	if city["type"] != "string" {
		t.Fatalf("city type = %#v, want string", city["type"])
	}
}

func assertRequestStructuredOutput(t *testing.T, payload map[string]any) {
	t.Helper()

	responseFormat, ok := payload["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format = %#v, want object", payload["response_format"])
	}
	if responseFormat["type"] != "json_schema" {
		t.Fatalf("response_format type = %#v, want json_schema", responseFormat["type"])
	}

	jsonSchema, ok := responseFormat["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema = %#v, want object", responseFormat["json_schema"])
	}
	if jsonSchema["name"] != "answer_schema" {
		t.Fatalf("json_schema name = %#v, want answer_schema", jsonSchema["name"])
	}
	if jsonSchema["strict"] != true {
		t.Fatalf("json_schema strict = %#v, want true", jsonSchema["strict"])
	}
	schema, ok := jsonSchema["schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema schema = %#v, want object", jsonSchema["schema"])
	}
	if schema["type"] != "object" {
		t.Fatalf("schema type = %#v, want object", schema["type"])
	}
}

type roundTripFunc func(r *http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}
