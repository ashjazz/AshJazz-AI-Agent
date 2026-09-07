package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	"github.com/ashjazz/Longtermism/pkg/ai/llm/openai"
	llmtestutil "github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
	"github.com/ashjazz/Longtermism/pkg/ai/resilience"
)

const t069Secret = "T069_SECRET_MUST_NOT_LEAK"

func TestBuildLLMProviderDisabledUsesOfflineProviderWithoutReadingEnvironment(t *testing.T) {
	var lookupCalls, factoryCalls, observerFactoryCalls int
	offline := &scriptedLLMProvider{response: &llm.ChatResponse{Content: "offline", Model: "offline-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{})}}

	provider, snapshot, err := BuildLLMProvider(context.Background(), LLMProviderConfigInput{ChatEnabled: false}, LLMProviderDependencies{
		LookupEnv: func(string) string { lookupCalls++; return t069Secret },
		NewOpenAI: func(openai.Config) (llm.Provider, error) {
			factoryCalls++
			return nil, errors.New("must not construct provider")
		},
		NewAttemptObserver: func(LLMProviderConfigSnapshot) (resilience.ProviderAttemptObserver, error) {
			observerFactoryCalls++
			return &providerAttemptRecorder{}, nil
		},
		NewOfflineFake: func() llm.Provider { return offline },
	})
	if err != nil {
		t.Fatalf("BuildLLMProvider() error = %v", err)
	}
	if lookupCalls != 0 || factoryCalls != 0 || observerFactoryCalls != 0 {
		t.Fatalf("disabled chat reads=%d provider_factories=%d observer_factories=%d, want all zero", lookupCalls, factoryCalls, observerFactoryCalls)
	}
	if snapshot.Enabled || snapshot.CredentialPresent || snapshot.BaseURLPresent {
		t.Fatalf("disabled chat snapshot = %#v, want no configured provider", snapshot)
	}
	response, err := provider.Chat(context.Background(), validLLMChatRequest())
	if err != nil || response.Content != "offline" || offline.chatCalls != 1 {
		t.Fatalf("offline provider result=%#v error=%v calls=%d", response, err, offline.chatCalls)
	}
}

func TestBuildLLMProviderInjectsOneAttemptObserverIntoResilienceWrapper(t *testing.T) {
	baseProvider := &scriptedLLMProvider{
		name: "openai-compatible",
		response: &llm.ChatResponse{
			Content:      "ok",
			Model:        "chat-model",
			FinishReason: llm.FinishStop,
			Usage:        llmtestutil.MustReportedUsage(llm.Usage{}),
		},
	}
	recorder := &providerAttemptRecorder{}
	observerFactoryCalls := 0

	provider, snapshot, err := BuildLLMProvider(
		context.Background(),
		enabledLLMProviderInput("OPENAI_BASE_URL", "OPENAI_API_KEY", "chat-model"),
		LLMProviderDependencies{
			LookupEnv: lookupEnv(map[string]string{
				"OPENAI_BASE_URL": "https://api.example.test/v1",
				"OPENAI_API_KEY":  t069Secret,
			}),
			NewOpenAI: func(openai.Config) (llm.Provider, error) { return baseProvider, nil },
			NewAttemptObserver: func(got LLMProviderConfigSnapshot) (resilience.ProviderAttemptObserver, error) {
				observerFactoryCalls++
				if !got.Enabled || got.Provider != "openai" || got.DefaultModel != "chat-model" || !got.CredentialPresent || !got.BaseURLPresent {
					t.Fatalf("attempt observer snapshot = %#v, want validated low-sensitivity provider facts", got)
				}
				return recorder, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("BuildLLMProvider() error = %v", err)
	}
	if observerFactoryCalls != 1 {
		t.Fatalf("attempt observer factory calls = %d, want 1", observerFactoryCalls)
	}
	if snapshot.DefaultModel != "chat-model" {
		t.Fatalf("provider snapshot = %#v, want configured model", snapshot)
	}

	response, err := provider.Chat(context.Background(), validLLMChatRequest())
	if err != nil || response != baseProvider.response {
		t.Fatalf("assembled provider response/error = %p/%v, want %p/nil", response, err, baseProvider.response)
	}
	if baseProvider.chatCalls != 1 || len(recorder.facts) != 1 {
		t.Fatalf("network attempts=%d observed facts=%d, want 1/1", baseProvider.chatCalls, len(recorder.facts))
	}
	if fact := recorder.facts[0]; fact.Provider() != "openai-compatible" || fact.RequestedModel() != "chat-model" || fact.Outcome() != resilience.ProviderAttemptSucceeded {
		t.Fatalf("observed attempt fact = provider:%q requested_model:%q outcome:%q", fact.Provider(), fact.RequestedModel(), fact.Outcome())
	}
}

func TestBuildLLMProviderEnabledFailsFastForMissingConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		input LLMProviderConfigInput
		env   map[string]string
	}{
		{name: "missing base URL environment name", input: enabledLLMProviderInput("", "OPENAI_API_KEY", "chat-model")},
		{name: "missing API key environment name", input: enabledLLMProviderInput("OPENAI_BASE_URL", "", "chat-model")},
		{name: "missing default model", input: enabledLLMProviderInput("OPENAI_BASE_URL", "OPENAI_API_KEY", "")},
		{name: "blank base URL value", input: enabledLLMProviderInput("OPENAI_BASE_URL", "OPENAI_API_KEY", "chat-model"), env: map[string]string{"OPENAI_BASE_URL": " ", "OPENAI_API_KEY": t069Secret}},
		{name: "blank API key value", input: enabledLLMProviderInput("OPENAI_BASE_URL", "OPENAI_API_KEY", "chat-model"), env: map[string]string{"OPENAI_BASE_URL": "https://api.example.test/v1", "OPENAI_API_KEY": "\t"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factoryCalls := 0
			provider, _, err := BuildLLMProvider(context.Background(), tt.input, LLMProviderDependencies{
				LookupEnv: lookupEnv(tt.env),
				NewOpenAI: func(openai.Config) (llm.Provider, error) { factoryCalls++; return nil, nil },
			})
			if err == nil || provider != nil {
				t.Fatalf("BuildLLMProvider() provider=%#v error=%v, want fail-fast", provider, err)
			}
			if factoryCalls != 0 {
				t.Fatalf("OpenAI factory calls=%d, want 0", factoryCalls)
			}
			if strings.Contains(fmt.Sprint(err), t069Secret) {
				t.Fatalf("configuration error leaked secret: %v", err)
			}
		})
	}
}

func TestBuildLLMProviderConfiguresSafeTransportAndExecutionPolicy(t *testing.T) {
	providerStub := &scriptedLLMProvider{response: &llm.ChatResponse{Content: "ok", Model: "chat-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{})}}
	var factoryConfig openai.Config
	provider, snapshot, err := BuildLLMProvider(context.Background(), enabledLLMProviderInput("OPENAI_BASE_URL", "OPENAI_API_KEY", "chat-model"), LLMProviderDependencies{
		LookupEnv: lookupEnv(map[string]string{"OPENAI_BASE_URL": "https://api.example.test/v1", "OPENAI_API_KEY": t069Secret}),
		NewOpenAI: func(config openai.Config) (llm.Provider, error) { factoryConfig = config; return providerStub, nil },
	})
	if err != nil {
		t.Fatalf("BuildLLMProvider() error = %v", err)
	}
	if snapshot.Timeout != 60*time.Second || snapshot.RetryMax != 2 || snapshot.RetryBackoff != time.Second {
		t.Fatalf("execution policy snapshot=%#v, want defaults", snapshot)
	}
	if factoryConfig.HTTPClient == nil || factoryConfig.HTTPClient.Timeout != 60*time.Second {
		t.Fatalf("HTTP client=%#v, want 60s timeout", factoryConfig.HTTPClient)
	}
	if factoryConfig.HTTPClient.CheckRedirect == nil || factoryConfig.HTTPClient.CheckRedirect(&http.Request{}, nil) == nil {
		t.Fatal("DI-owned HTTP client must reject redirects before a Bearer credential can be forwarded")
	}
	if _, err := provider.Chat(context.Background(), validLLMChatRequest()); err != nil || providerStub.chatCalls != 1 {
		t.Fatalf("assembled provider chat error=%v calls=%d", err, providerStub.chatCalls)
	}
}

func TestBuildLLMProviderUsesInjectedFakeWithoutExternalCallsOrSecretLeakage(t *testing.T) {
	fake := &scriptedLLMProvider{response: &llm.ChatResponse{Content: "fake", Model: "chat-model", Usage: llmtestutil.MustReportedUsage(llm.Usage{})}}
	networkCalls := 0
	provider, snapshot, err := BuildLLMProvider(context.Background(), enabledLLMProviderInput("OPENAI_BASE_URL", "OPENAI_API_KEY", "chat-model"), LLMProviderDependencies{
		LookupEnv: lookupEnv(map[string]string{"OPENAI_BASE_URL": "https://api.example.test/v1", "OPENAI_API_KEY": t069Secret}),
		NewHTTPClient: func(timeout time.Duration) *http.Client {
			return &http.Client{Timeout: timeout, Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				networkCalls++
				return nil, errors.New("test transport must not be called")
			})}
		},
		NewOpenAI: func(config openai.Config) (llm.Provider, error) {
			if config.HTTPClient == nil {
				t.Fatal("fake must receive DI-owned HTTP client")
			}
			return fake, nil
		},
	})
	if err != nil {
		t.Fatalf("BuildLLMProvider() error = %v", err)
	}
	if _, err := provider.Chat(context.Background(), validLLMChatRequest()); err != nil || fake.chatCalls != 1 {
		t.Fatalf("injected fake chat error=%v calls=%d", err, fake.chatCalls)
	}
	if networkCalls != 0 {
		t.Fatalf("injected fake made %d HTTP calls, want zero", networkCalls)
	}
	if rendered := fmt.Sprintf("%+v", snapshot); strings.Contains(rendered, t069Secret) || strings.Contains(rendered, "https://api.example.test/v1") {
		t.Fatalf("safe snapshot leaked a credential or endpoint: %s", rendered)
	}
}

func TestBuildLLMProviderSanitizesFactoryError(t *testing.T) {
	provider, _, err := BuildLLMProvider(context.Background(), enabledLLMProviderInput("OPENAI_BASE_URL", "OPENAI_API_KEY", "chat-model"), LLMProviderDependencies{
		LookupEnv: lookupEnv(map[string]string{"OPENAI_BASE_URL": "https://api.example.test/v1", "OPENAI_API_KEY": t069Secret}),
		NewOpenAI: func(openai.Config) (llm.Provider, error) { return nil, fmt.Errorf("factory leaked %s", t069Secret) },
	})
	if provider != nil || err == nil || strings.Contains(fmt.Sprint(err), t069Secret) {
		t.Fatalf("factory failure provider=%#v error=%v, want sanitized error", provider, err)
	}
}

func TestBuildLLMProviderRejectsUnsafeProductionBaseURLBeforeFactory(t *testing.T) {
	tests := []struct{ name, baseURL string }{
		{name: "plaintext HTTP", baseURL: "http://api.example.test/v1"},
		{name: "URL userinfo", baseURL: "https://user:password@api.example.test/v1"},
		{name: "query string", baseURL: "https://api.example.test/v1?target=other"},
		{name: "fragment", baseURL: "https://api.example.test/v1#fragment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factoryCalls := 0
			input := enabledLLMProviderInput("OPENAI_BASE_URL", "OPENAI_API_KEY", "chat-model")
			input.Environment = productionEnvironment
			provider, _, err := BuildLLMProvider(context.Background(), input, LLMProviderDependencies{
				LookupEnv: lookupEnv(map[string]string{"OPENAI_BASE_URL": tt.baseURL, "OPENAI_API_KEY": t069Secret}),
				NewOpenAI: func(openai.Config) (llm.Provider, error) { factoryCalls++; return nil, nil },
			})
			if provider != nil || err == nil || factoryCalls != 0 {
				t.Fatalf("unsafe BaseURL provider=%#v error=%v factoryCalls=%d", provider, err, factoryCalls)
			}
			if rendered := fmt.Sprint(err); strings.Contains(rendered, t069Secret) || strings.Contains(rendered, tt.baseURL) {
				t.Fatalf("unsafe BaseURL error leaked a secret or endpoint: %s", rendered)
			}
		})
	}
}

func TestBuildLLMProviderRejectsInvalidExecutionConfiguration(t *testing.T) {
	tests := []LLMProviderConfigInput{
		{Timeout: "not-a-duration"}, {RetryMax: -1}, {RetryMax: 3}, {RetryBackoff: -time.Second},
	}
	for _, input := range tests {
		if _, _, _, err := resolveLLMResilience(input); err == nil {
			t.Fatal("resolveLLMResilience() error = nil, want invalid configuration rejection")
		}
	}
}

func enabledLLMProviderInput(baseURLEnvName, apiKeyEnvName, model string) LLMProviderConfigInput {
	return LLMProviderConfigInput{ChatEnabled: true, DefaultProvider: "openai", BaseURLEnvName: baseURLEnvName, APIKeyEnvName: apiKeyEnvName, DefaultModel: model, Timeout: "60s", RetryMax: 2, RetryBackoff: time.Second}
}

func lookupEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func validLLMChatRequest() *llm.ChatRequest {
	return &llm.ChatRequest{Model: "chat-model", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}}
}

type scriptedLLMProvider struct {
	name       string
	response   *llm.ChatResponse
	chatErrors []error
	chatCalls  int
}

func (p *scriptedLLMProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "scripted"
}
func (*scriptedLLMProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{}
}
func (p *scriptedLLMProvider) Chat(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
	call := p.chatCalls
	p.chatCalls++
	if call < len(p.chatErrors) && p.chatErrors[call] != nil {
		return nil, p.chatErrors[call]
	}
	return p.response, nil
}
func (*scriptedLLMProvider) ChatStream(context.Context, *llm.ChatRequest) (<-chan llm.ChatChunk, error) {
	return nil, errors.New("streaming is not used by this cmd contract")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

type providerAttemptRecorder struct {
	facts []resilience.ProviderAttemptFact
}

func (recorder *providerAttemptRecorder) ObserveProviderAttempt(fact resilience.ProviderAttemptFact) error {
	recorder.facts = append(recorder.facts, fact)
	return nil
}
