package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
)

func TestProviderChatStreamParsesDeltaOrderAndFinalMetadata(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		// OpenAI-compatible 流式响应以 SSE data 行持续返回 JSON chunk。
		// adapter 的核心职责是保持 delta 到达顺序，并把最后一个 chunk 中的
		// finish_reason/usage 传给上层；这些字段会影响 UI 流式展示、成本统计和 eval 复盘。
		writeStreamPayload(t, w, map[string]any{
			"id":    "chatcmpl_stream_test",
			"model": "gpt-stream-actual",
			"choices": []map[string]any{
				{
					"index": 0,
					"delta": map[string]any{
						"content": "hel",
					},
				},
			},
		})
		writeStreamPayload(t, w, map[string]any{
			"id":    "chatcmpl_stream_test",
			"model": "gpt-stream-actual",
			"choices": []map[string]any{
				{
					"index": 0,
					"delta": map[string]any{
						"content": "lo",
					},
				},
			},
		})
		writeStreamPayload(t, w, map[string]any{
			"id":    "chatcmpl_stream_test",
			"model": "gpt-stream-actual",
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     6,
				"completion_tokens": 2,
				"total_tokens":      8,
			},
		})
		writeStreamData(t, w, "[DONE]")
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "gpt-stream",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	chunks, err := provider.ChatStream(context.Background(), &llm.ChatRequest{
		Model: "gpt-stream",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "stream hello"},
		},
		Stream: true,
	})
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}

	var deltas []string
	var finalReason llm.FinishReason
	var finalUsage *llm.Usage
	for chunk := range chunks {
		if chunk.Err != nil {
			t.Fatalf("ChatStream() chunk error = %v, want nil in successful stream", chunk.Err)
		}
		if chunk.DeltaContent != "" {
			deltas = append(deltas, chunk.DeltaContent)
		}
		if chunk.FinishReason != "" {
			finalReason = chunk.FinishReason
		}
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
		}
	}

	assertStringSlice(t, deltas, []string{"hel", "lo"})
	if finalReason != llm.FinishStop {
		t.Fatalf("final finish reason = %q, want %q", finalReason, llm.FinishStop)
	}
	if finalUsage == nil {
		t.Fatal("final usage = nil, want usage on final chunk")
	}
	assertUsage(t, *finalUsage, llm.Usage{
		InputTokens:  6,
		OutputTokens: 2,
		TotalTokens:  8,
	})
}

func TestProviderChatStreamExposesStreamErrorChunk(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		// 有些上游会在 HTTP 连接已经建立后才通过 SSE 返回错误对象。
		// 这种失败不能只靠 ChatStream 的初始 error 表达，否则调用方会误以为流正常结束；
		// 因此 provider 需要把流中错误作为 llm.ChatChunk.Err 暴露并关闭 channel。
		writeStreamPayload(t, w, map[string]any{
			"id":    "chatcmpl_stream_error_test",
			"model": "gpt-stream-actual",
			"choices": []map[string]any{
				{
					"index": 0,
					"delta": map[string]any{
						"content": "partial",
					},
				},
			},
		})
		writeStreamPayload(t, w, map[string]any{
			"error": map[string]any{
				"message": "stream exploded",
				"type":    "server_error",
			},
		})
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "gpt-stream",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	chunks, err := provider.ChatStream(context.Background(), &llm.ChatRequest{
		Model: "gpt-stream",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "trigger stream error"},
		},
		Stream: true,
	})
	if err != nil {
		t.Fatalf("ChatStream() initial error = %v, want stream-level error chunk", err)
	}

	var sawPartial bool
	var streamErr error
	for chunk := range chunks {
		if chunk.DeltaContent == "partial" {
			sawPartial = true
		}
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}

	if !sawPartial {
		t.Fatal("ChatStream() did not emit partial delta before stream error")
	}
	assertErrorMentions(t, streamErr, "stream exploded")
}

func TestProviderChatStreamReportsUnexpectedEOFBeforeDone(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeStreamPayload(t, w, map[string]any{
			"choices": []map[string]any{
				{
					"index": 0,
					"delta": map[string]any{"content": "partial"},
				},
			},
		})
		// 故意不发送 [DONE]：模拟代理、网关或上游在半途中关闭连接。
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "gpt-stream",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	chunks, err := provider.ChatStream(context.Background(), &llm.ChatRequest{
		Model:    "gpt-stream",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "stream"}},
	})
	if err != nil {
		t.Fatalf("ChatStream() initial error = %v", err)
	}

	var streamErr error
	for chunk := range chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("ChatStream() stream error = nil, want unexpected EOF error")
	}
	if !errors.Is(streamErr, llm.ErrUpstream) {
		t.Fatalf("ChatStream() stream error = %v, want errors.Is ErrUpstream", streamErr)
	}
}

func TestProviderChatStreamMapsInitialHTTPError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		err := json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "stream quota exhausted",
				"type":    "rate_limit_error",
			},
		})
		if err != nil {
			t.Fatalf("failed to encode stream HTTP error: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(Config{
		BaseURL:      server.URL,
		APIKey:       "test-api-key",
		DefaultModel: "gpt-stream",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	chunks, err := provider.ChatStream(context.Background(), &llm.ChatRequest{
		Model: "gpt-stream",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "trigger stream rate limit"},
		},
		Stream: true,
	})
	if err == nil {
		t.Fatal("ChatStream() error = nil, want initial HTTP error")
	}
	if chunks != nil {
		t.Fatalf("ChatStream() chunks = %#v, want nil on initial HTTP error", chunks)
	}
	if !errors.Is(err, llm.ErrUpstream) {
		t.Fatalf("ChatStream() error = %v, want errors.Is ErrUpstream", err)
	}
	// SSE 建立前的 HTTP 失败也只保留状态，不允许旁路回显错误正文。
	assertStatusOnlyError(t, err, http.StatusTooManyRequests, true)
}

func writeStreamPayload(t *testing.T, w http.ResponseWriter, payload map[string]any) {
	t.Helper()

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal stream payload: %v", err)
	}
	writeStreamData(t, w, string(data))
}

func writeStreamData(t *testing.T, w http.ResponseWriter, data string) {
	t.Helper()

	if _, err := w.Write([]byte("data: " + data + "\n\n")); err != nil {
		t.Fatalf("failed to write stream data: %v", err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func assertStringSlice(t *testing.T, got []string, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("slice length = %d, want %d; got = %#v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("slice[%d] = %q, want %q; got = %#v", index, got[index], want[index], got)
		}
	}
}
