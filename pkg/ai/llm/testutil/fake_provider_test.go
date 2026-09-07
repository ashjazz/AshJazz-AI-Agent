package testutil

import (
	"context"
	"errors"
	"testing"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
)

func TestFakeProviderReturnsDeepCopies(t *testing.T) {
	t.Parallel()

	provider := NewFakeProvider(FakeProviderConfig{
		ChatResponses: map[string]llm.ChatResponse{
			"tool-model": {
				Usage: MustReportedUsage(llm.Usage{}),
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call-1",
						Name: "search",
						Arguments: map[string]any{
							"filter": map[string]any{"tenant": "original"},
							"tags":   []any{"original"},
						},
					},
				},
			},
		},
	})

	first, err := provider.Chat(context.Background(), &llm.ChatRequest{Model: "tool-model"})
	if err != nil {
		t.Fatalf("first Chat() error = %v", err)
	}
	first.ToolCalls[0].Arguments["filter"].(map[string]any)["tenant"] = "mutated"
	first.ToolCalls[0].Arguments["tags"].([]any)[0] = "mutated"

	second, err := provider.Chat(context.Background(), &llm.ChatRequest{Model: "tool-model"})
	if err != nil {
		t.Fatalf("second Chat() error = %v", err)
	}
	if got := second.ToolCalls[0].Arguments["filter"].(map[string]any)["tenant"]; got != "original" {
		t.Fatalf("nested tenant = %q, want original", got)
	}
	if got := second.ToolCalls[0].Arguments["tags"].([]any)[0]; got != "original" {
		t.Fatalf("nested tag = %q, want original", got)
	}
}

func TestFakeProviderCoversConfiguredBehaviors(t *testing.T) {
	t.Parallel()

	upstreamErr := errors.New("upstream unavailable")
	provider := NewFakeProvider(FakeProviderConfig{
		Name: "configured-fake",
		DefaultCapabilities: llm.ProviderCapabilities{
			Streaming: true,
		},
		Capabilities: map[string]llm.ProviderCapabilities{
			"tool-model": {ToolCalling: true},
		},
		ChatErrors: map[string]error{
			"error-model": upstreamErr,
		},
		StreamChunks: map[string][]llm.ChatChunk{
			"stream-model": {
				{DeltaContent: "hello"},
				{Usage: &llm.Usage{InputTokens: 1, TotalTokens: 1}},
			},
		},
		StreamErrors: map[string]error{
			"stream-error-model": upstreamErr,
		},
	})

	if provider.Name() != "configured-fake" {
		t.Fatalf("Name() = %q, want configured-fake", provider.Name())
	}
	if !provider.Capabilities("tool-model").ToolCalling {
		t.Fatal("tool-model ToolCalling = false, want true")
	}
	if !provider.Capabilities("unknown-model").Streaming {
		t.Fatal("default Streaming = false, want true")
	}

	if _, err := provider.Chat(context.Background(), &llm.ChatRequest{Model: "error-model"}); !errors.Is(err, upstreamErr) {
		t.Fatalf("Chat() error = %v, want configured error", err)
	}
	if _, err := provider.Chat(context.Background(), &llm.ChatRequest{Model: "missing-model"}); err == nil {
		t.Fatal("Chat() error = nil, want missing response error")
	}

	chunks, err := provider.ChatStream(context.Background(), &llm.ChatRequest{Model: "stream-model"})
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	var got []llm.ChatChunk
	for chunk := range chunks {
		got = append(got, chunk)
	}
	if len(got) != 2 || got[0].DeltaContent != "hello" || got[1].Usage == nil {
		t.Fatalf("stream chunks = %#v, want configured chunks", got)
	}

	if _, err := provider.ChatStream(context.Background(), &llm.ChatRequest{Model: "stream-error-model"}); !errors.Is(err, upstreamErr) {
		t.Fatalf("ChatStream() error = %v, want configured error", err)
	}
	if _, err := provider.ChatStream(context.Background(), &llm.ChatRequest{Model: "missing-stream"}); err == nil {
		t.Fatal("ChatStream() error = nil, want missing stream error")
	}
}

func TestFakeProviderHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	provider := NewFakeProvider(FakeProviderConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := provider.Chat(ctx, &llm.ChatRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat() error = %v, want context.Canceled", err)
	}
	if _, err := provider.ChatStream(ctx, &llm.ChatRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ChatStream() error = %v, want context.Canceled", err)
	}
}
