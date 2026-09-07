package resilience

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	llmtestutil "github.com/ashjazz/Longtermism/pkg/ai/llm/testutil"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
)

func TestProviderAttemptFactIsImmutableAndLowSensitivity(t *testing.T) {
	const (
		promptCanary     = "private-prompt-message-canary"
		credentialCanary = "sk-live-synthetic-credential-canary"
		responseCanary   = "raw-provider-response-content-canary"
		errorBodyCanary  = "raw-provider-error-body-canary"
		endpointCanary   = "https://provider.invalid/private-endpoint"
	)

	// The observer method shape is an executable security boundary: adding context, request,
	// response, or error parameters would let high-sensitivity state cross the core metrics port.
	observerType := reflect.TypeOf((*ProviderAttemptObserver)(nil)).Elem()
	method, ok := observerType.MethodByName("ObserveProviderAttempt")
	if observerType.NumMethod() != 1 || !ok || method.Type.NumIn() != 1 || method.Type.In(0) != reflect.TypeOf(ProviderAttemptFact{}) ||
		method.Type.NumOut() != 1 || method.Type.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
		t.Fatalf("ProviderAttemptObserver must accept only ProviderAttemptFact and return error; method=%#v", method)
	}

	factType := reflect.TypeOf(ProviderAttemptFact{})
	allowedFactFields := map[string][]reflect.Kind{
		"provider":         {reflect.String},
		"requestedModel":   {reflect.String},
		"actualModel":      {reflect.String},
		"startedAt":        {reflect.Struct},
		"duration":         {reflect.Int64},
		"outcome":          {reflect.String},
		"usage":            {reflect.Struct},
		"costAvailability": {reflect.String},
		"cost":             {reflect.Float64},
		"costAmount":       {reflect.Float64},
		"costCurrency":     {reflect.String},
	}
	for index := 0; index < factType.NumField(); index++ {
		field := factType.Field(index)
		if field.IsExported() {
			t.Fatalf("ProviderAttemptFact field %q is exported; immutable facts require read-only getters", field.Name)
		}
		allowedKinds, allowed := allowedFactFields[field.Name]
		if !allowed || !containsReflectKind(allowedKinds, field.Type.Kind()) {
			t.Fatalf("ProviderAttemptFact field %q type=%v is outside the low-sensitivity field allowlist", field.Name, field.Type)
		}
		switch field.Name {
		case "startedAt":
			if field.Type != reflect.TypeOf(time.Time{}) {
				t.Fatalf("ProviderAttemptFact startedAt type=%v, want time.Time", field.Type)
			}
		case "duration":
			if field.Type != reflect.TypeOf(time.Duration(0)) {
				t.Fatalf("ProviderAttemptFact duration type=%v, want time.Duration", field.Type)
			}
		case "usage":
			if field.Type != reflect.TypeOf(llm.ProviderUsage{}) {
				t.Fatalf("ProviderAttemptFact usage type=%v, want llm.ProviderUsage", field.Type)
			}
		}
		if field.Type == reflect.TypeOf((*error)(nil)).Elem() ||
			field.Type == reflect.TypeOf((*context.Context)(nil)).Elem() ||
			field.Type == reflect.TypeOf((*llm.ChatRequest)(nil)) ||
			field.Type == reflect.TypeOf((*llm.ChatResponse)(nil)) {
			t.Fatalf("ProviderAttemptFact field %q carries forbidden type %v", field.Name, field.Type)
		}
		switch field.Type.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface, reflect.Func:
			t.Fatalf("ProviderAttemptFact field %q uses mutable/opaque kind %v", field.Name, field.Type.Kind())
		}
	}

	usage := llmtestutil.MustReportedUsage(llm.Usage{InputTokens: 6, OutputTokens: 2, TotalTokens: 8})
	response := &llm.ChatResponse{Content: responseCanary, Model: "actual-model", Usage: usage, FinishReason: llm.FinishStop}
	observer := &providerAttemptRecorder{}
	successProvider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{{response: response}}}
	successWrapper := newProviderAttemptTestWrapper(successProvider, observer, 0, func(context.Context, time.Duration) error { return nil })
	identity := obs.NewCorrelationIdentity(
		"request-correlation-canary",
		obs.WithServiceSpan("service-trace-correlation-canary", "span-correlation-canary"),
		obs.WithAITraceID("ai-trace-correlation-canary"),
		obs.WithSessionID("session-correlation-canary"),
		obs.WithEvalRunID("eval-run-correlation-canary"),
	)
	ctx := obs.ContextWithCorrelationIdentity(context.Background(), identity)
	request := &llm.ChatRequest{
		Model:    "requested-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: promptCanary}},
		Tools:    []llm.Tool{{Name: "credential_tool", Description: credentialCanary}},
	}
	if _, err := successWrapper.Chat(ctx, request); err != nil {
		t.Fatalf("success Chat() error=%v", err)
	}

	failureProvider := &providerAttemptScriptedProvider{steps: []providerAttemptStep{{
		err: fmt.Errorf("POST %s Authorization: Bearer %s body=%s: %w", endpointCanary, credentialCanary, errorBodyCanary, llm.ErrUpstream),
	}}}
	failureWrapper := newProviderAttemptTestWrapper(failureProvider, observer, 0, func(context.Context, time.Duration) error { return nil })
	_, err := failureWrapper.Chat(ctx, request)
	if !errors.Is(err, llm.ErrUpstream) {
		t.Fatalf("failure Chat() error=%v, want stable upstream classification", err)
	}

	facts := observer.Facts()
	if len(facts) != 2 {
		t.Fatalf("attempt facts=%d, want success and failure facts", len(facts))
	}
	for index, fact := range facts {
		projection := fmt.Sprintf("%#v|%s|%s|%s|%s", fact, fact.Provider(), fact.RequestedModel(), fact.ActualModel(), fact.Outcome())
		for _, forbidden := range []string{
			promptCanary, credentialCanary, responseCanary, errorBodyCanary, endpointCanary,
			"Authorization", "Bearer", identity.RequestID, identity.ServiceTraceID, identity.SpanID,
			identity.AITraceID, identity.SessionID, identity.EvalRunID,
		} {
			if strings.Contains(projection, forbidden) {
				t.Fatalf("attempt fact %d leaked forbidden value %q", index, forbidden)
			}
		}
		if got := fmt.Sprint(fact.CostAvailability()); got != "unavailable" {
			t.Fatalf("attempt fact %d cost availability=%q, want explicit unavailable", index, got)
		}
	}

	// Mutating caller-owned request/response state or a returned usage summary must not rewrite
	// the already-recorded fact. This protects async observers from time-of-check/time-of-use drift.
	request.Model = credentialCanary
	request.Messages[0].Content = credentialCanary
	response.Model = credentialCanary
	response.Usage = llmtestutil.MustReportedUsage(llm.Usage{})
	summary, ok := facts[0].Usage().Summary()
	if !ok {
		t.Fatal("success fact must retain reported usage")
	}
	summary.InputTokens = 999
	retained, ok := facts[0].Usage().Summary()
	if !ok || retained.InputTokens != 6 || retained.OutputTokens != 2 || retained.TotalTokens != 8 {
		t.Fatalf("immutable usage=%#v ok=%v, want original 6/2/8 snapshot", retained, ok)
	}
	if facts[0].RequestedModel() != "requested-model" || facts[0].ActualModel() != "actual-model" {
		t.Fatalf("immutable model snapshot=%q/%q, want requested-model/actual-model", facts[0].RequestedModel(), facts[0].ActualModel())
	}
}

func containsReflectKind(kinds []reflect.Kind, target reflect.Kind) bool {
	for _, kind := range kinds {
		if kind == target {
			return true
		}
	}
	return false
}
