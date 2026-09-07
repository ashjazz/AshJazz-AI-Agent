package llm

import (
	"errors"
	"reflect"
	"testing"
)

func TestProviderUsagePreservesImmutableReportedFacts(t *testing.T) {
	// 不能导出共享指针，否则复制响应后仍能篡改另一份观测/计费事实。
	typ := reflect.TypeOf(ProviderUsage{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Anonymous || field.IsExported() {
			t.Errorf("ProviderUsage field %s must be private and named", field.Name)
		}
	}
	for _, want := range []Usage{{}, {InputTokens: 9, OutputTokens: 4, TotalTokens: 13}} {
		reported, err := NewReportedProviderUsage(want)
		if err != nil || reported.Availability() != UsageReported {
			t.Fatalf("reported constructor failed: %v", err)
		}
		copyOfValue := reported
		summary, ok := reported.Summary()
		if !ok || summary != want {
			t.Fatal("reported summary lost its explicit facts")
		}
		summary.TotalTokens++
		got, ok := copyOfValue.Summary()
		if !ok || got != want {
			t.Fatal("summary mutation changed copied value object")
		}
	}
	for _, unavailable := range []ProviderUsage{{}, NewUnavailableProviderUsage()} {
		if _, ok := unavailable.Summary(); ok || unavailable.Availability() == UsageReported {
			t.Fatal("unavailable usage became reported zero")
		}
	}
}

func TestProviderUsageRejectsInvalidReportedFacts(t *testing.T) {
	for _, invalid := range []Usage{
		{InputTokens: -1}, {OutputTokens: -1}, {TotalTokens: -1},
		{ReasoningTokens: -1}, {CacheReadTokens: -1}, {CacheWriteTokens: -1},
		{InputTokens: 1, OutputTokens: 2, TotalTokens: 2},
		{InputTokens: 1, OutputTokens: 2, TotalTokens: 4},
		{InputTokens: 100_000_001, TotalTokens: 100_000_001},
	} {
		usage, err := NewReportedProviderUsage(invalid)
		if !errors.Is(err, ErrInvalidResponse) || usage.Availability() == UsageReported {
			t.Fatalf("invalid usage became reported: %#v, %v", usage, err)
		}
		if _, ok := usage.Summary(); ok {
			t.Fatal("invalid usage exposed a summary")
		}
	}
}
