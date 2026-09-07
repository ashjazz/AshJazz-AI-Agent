package smoke

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aieval "github.com/ashjazz/Longtermism/pkg/ai/eval"
	evaltestutil "github.com/ashjazz/Longtermism/pkg/ai/eval/testutil"
	"github.com/ashjazz/Longtermism/pkg/ai/llm"
	"github.com/ashjazz/Longtermism/pkg/ai/obs"
	obstestutil "github.com/ashjazz/Longtermism/pkg/ai/obs/testutil"
)

// 未报告 token 的响应不能进入成功 trace 或生成评估报告。
func TestRunP0RejectsUnavailableUsage(t *testing.T) {
	for _, usage := range []llm.ProviderUsage{{}, llm.NewUnavailableProviderUsage()} {
		recorder := obstestutil.NewRecorder()
		model := sampleModel("test-model", "usage-case")
		provider := &goldenFakeProvider{responses: map[string]llm.ChatResponse{
			model: {Model: model, Content: "untrusted answer", Usage: usage, FinishReason: llm.FinishStop},
		}}
		result, err := RunP0(context.Background(), Config{
			Dataset:    evaltestutil.NewStaticDataset([]aieval.Sample{{ID: "usage-case", Query: "question", GroundTruth: "answer"}}),
			PromptRoot: writePromptRoot(t, `{{ .Question }}`), Provider: provider, Model: "test-model", Tracer: recorder,
		})
		if !errors.Is(err, llm.ErrInvalidResponse) || result.Report.SampleCount != 0 {
			t.Fatalf("RunP0() report count=%d error=%v, want no report and invalid response", result.Report.SampleCount, err)
		}
		recorder.AssertCount(t, 1)
		recorder.AssertTrace(t, 0, func(t *testing.T, trace obs.Trace) {
			if trace.OutcomeStatus == successStatus || trace.InputTokens != 0 || trace.OutputTokens != 0 {
				t.Fatal("unavailable usage produced success/token facts")
			}
		})
	}
}

func TestRunP0BuildsPromptLLMTraceEvalPath(t *testing.T) {
	recorder := obstestutil.NewRecorder()
	samples := []aieval.Sample{
		{
			ID:          "case-one",
			Query:       "P0 最小闭环是什么？",
			GroundTruth: "prompt -> llm -> obs -> eval",
			RelevantCtx: []string{"P0 最小闭环需要串联 prompt、llm、obs 和 eval。"},
		},
		{
			ID:          "case-two",
			Query:       "短问题也要稳定吗？",
			GroundTruth: "需要稳定",
			RelevantCtx: []string{"短 query 也应该稳定经过 prompt、模型、trace 和评估。"},
		},
	}

	result, err := RunP0(context.Background(), Config{
		Dataset:         evaltestutil.NewStaticDataset(samples),
		DatasetIdentity: aieval.DatasetIdentity{Name: "p0-smoke", Version: "p0-smoke-test"},
		PromptRoot:      writePromptRoot(t, `Q={{ .Question }}; C={{ .Context }}`),
		Tracer:          recorder,
		Now: func() time.Time {
			return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
		},
	})

	if err != nil {
		t.Fatalf("RunP0() error = %v", err)
	}
	if result.Report.Dataset.Name != "p0-smoke" {
		t.Fatalf("dataset name = %q, want p0-smoke", result.Report.Dataset.Name)
	}
	if result.Report.Dataset.Version != "p0-smoke-test" {
		t.Fatalf("dataset version = %q, want p0-smoke-test", result.Report.Dataset.Version)
	}
	if result.Report.SampleCount != len(samples) {
		t.Fatalf("sample count = %d, want %d", result.Report.SampleCount, len(samples))
	}
	assertScore(t, result.Report.Scores, "exact_match", 1)
	assertScore(t, result.Report.Scores, "context_hit", 1)

	recorder.AssertCount(t, len(samples))
	if len(result.Traces) != len(samples) {
		t.Fatalf("result traces length = %d, want %d", len(result.Traces), len(samples))
	}
	for index, sample := range samples {
		recorder.AssertTrace(t, index, func(t *testing.T, trace obs.Trace) {
			t.Helper()

			if trace.TraceID == "" {
				t.Fatalf("trace id is empty")
			}
			if trace.RequestID == "" {
				t.Fatalf("request id is empty")
			}
			if trace.ServiceTraceID == "" {
				t.Fatalf("service trace id is empty")
			}
			if trace.SpanID == "" {
				t.Fatalf("span id is empty")
			}
			if trace.ObservationType != obs.ObservationTypeGeneration {
				t.Fatalf("observation type = %q, want generation", trace.ObservationType)
			}
			if trace.Feature != "p0_eval_smoke" {
				t.Fatalf("feature = %q, want p0_eval_smoke", trace.Feature)
			}
			if trace.QueryHash == "" || trace.QueryHash == sample.Query {
				t.Fatalf("query hash = %q, want non-empty hash different from raw query", trace.QueryHash)
			}
			if trace.QueryLen != len([]rune(sample.Query)) {
				t.Fatalf("query len = %d, want %d", trace.QueryLen, len([]rune(sample.Query)))
			}
			if trace.PromptTemplateVer != DefaultPromptVersion {
				t.Fatalf("prompt version = %q, want %q", trace.PromptTemplateVer, DefaultPromptVersion)
			}
			if trace.PromptHash == "" {
				t.Fatalf("prompt hash is empty")
			}
			if trace.Model == "" || !strings.Contains(trace.Model, sample.ID) {
				t.Fatalf("model = %q, want sample scoped fake model", trace.Model)
			}
			if trace.InputTokens == 0 || trace.OutputTokens == 0 {
				t.Fatalf("usage = input %d output %d, want non-zero", trace.InputTokens, trace.OutputTokens)
			}
			if trace.ChunksRetrieved != len(sample.RelevantCtx) {
				t.Fatalf("chunks retrieved = %d, want %d", trace.ChunksRetrieved, len(sample.RelevantCtx))
			}
			if trace.OutcomeStatus != "success" {
				t.Fatalf("outcome status = %q, want success", trace.OutcomeStatus)
			}

			wantSuffix := sanitizeTraceIDPart(sample.ID)
			if !strings.HasSuffix(trace.RequestID, wantSuffix) {
				t.Fatalf("request id = %q, want suffix %q", trace.RequestID, wantSuffix)
			}
			if !strings.HasSuffix(trace.ServiceTraceID, wantSuffix) {
				t.Fatalf("service trace id = %q, want suffix %q", trace.ServiceTraceID, wantSuffix)
			}
			if !strings.HasSuffix(trace.SpanID, wantSuffix) {
				t.Fatalf("span id = %q, want suffix %q", trace.SpanID, wantSuffix)
			}
			if !strings.HasSuffix(trace.TraceID, wantSuffix) {
				t.Fatalf("ai trace id = %q, want suffix %q", trace.TraceID, wantSuffix)
			}
		})
	}
}

func TestRunP0ReturnsPromptRenderError(t *testing.T) {
	_, err := RunP0(context.Background(), Config{
		Dataset: evaltestutil.NewStaticDataset([]aieval.Sample{
			{
				ID:          "broken-template",
				Query:       "缺变量会怎样？",
				GroundTruth: "不会静默通过",
				RelevantCtx: []string{"模板缺变量必须失败。"},
			},
		}),
		PromptRoot: writePromptRoot(t, `{{ .Missing }}`),
	})

	if err == nil {
		t.Fatalf("RunP0() error = nil, want prompt render error")
	}
	if !strings.Contains(err.Error(), "render p0 smoke prompt") {
		t.Fatalf("error = %q, want render p0 smoke prompt", err.Error())
	}
}

func TestRunP0DefaultLocalAssetsFromNestedWorkingDirectory(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Chdir(filepath.Join("..", "..", ".."))

	result, err := RunP0(context.Background(), Config{})
	if err != nil {
		t.Fatalf("RunP0() error = %v", err)
	}
	if result.Report.SampleCount != 3 {
		t.Fatalf("sample count = %d, want 3", result.Report.SampleCount)
	}
	assertScore(t, result.Report.Scores, "exact_match", 1)
	assertScore(t, result.Report.Scores, "context_hit", 1)
}

func TestGoldenFakeProviderBoundaries(t *testing.T) {
	provider, err := newGoldenFakeProvider(DefaultModel, []aieval.Sample{
		{
			ID:          "case-one",
			Query:       "q",
			GroundTruth: "a",
			RelevantCtx: []string{"ctx"},
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "p0-smoke-fake" {
		t.Fatalf("provider name = %q, want p0-smoke-fake", provider.Name())
	}
	if provider.Capabilities("any") != (llm.ProviderCapabilities{}) {
		t.Fatalf("capabilities = %#v, want zero capabilities", provider.Capabilities("any"))
	}
	if _, err := provider.Chat(context.Background(), nil); err == nil {
		t.Fatalf("Chat(nil) error = nil, want error")
	}
	if _, err := provider.Chat(context.Background(), &llm.ChatRequest{Model: "missing"}); err == nil {
		t.Fatalf("Chat(missing model) error = nil, want error")
	}
	if _, err := provider.ChatStream(context.Background(), &llm.ChatRequest{}); err == nil {
		t.Fatalf("ChatStream() error = nil, want unsupported error")
	}
}

func TestStaticDatasetAndTraceCopiesAreDefensive(t *testing.T) {
	dataset := newStaticDataset([]aieval.Sample{
		{
			ID:          "copy-case",
			Query:       "q",
			GroundTruth: "a",
			RelevantCtx: []string{"ctx"},
			Meta: map[string]any{
				"nested": map[string]any{
					"tags": []any{"a", "b"},
				},
			},
		},
	})

	first, err := dataset.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	first[0].RelevantCtx[0] = "mutated"
	first[0].Meta["nested"].(map[string]any)["tags"].([]any)[0] = "mutated"

	second, err := dataset.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() second error = %v", err)
	}
	if second[0].RelevantCtx[0] != "ctx" {
		t.Fatalf("relevant ctx = %q, want ctx", second[0].RelevantCtx[0])
	}
	if second[0].Meta["nested"].(map[string]any)["tags"].([]any)[0] != "a" {
		t.Fatalf("nested meta was mutated: %#v", second[0].Meta)
	}

	recorder := newTraceRecorder()
	userRating := 1
	autoScore := 0.75
	recorder.Record(context.Background(), obs.Trace{
		TraceID:       "trace-copy",
		TopScores:     []float64{0.9},
		UserRating:    &userRating,
		AutoEvalScore: &autoScore,
	})

	traces := recorder.Traces()
	traces[0].TopScores[0] = 0.1
	*traces[0].UserRating = -1
	*traces[0].AutoEvalScore = 0.1

	again := recorder.Traces()
	if again[0].TopScores[0] != 0.9 {
		t.Fatalf("top score = %v, want 0.9", again[0].TopScores[0])
	}
	if *again[0].UserRating != 1 {
		t.Fatalf("user rating = %d, want 1", *again[0].UserRating)
	}
	if *again[0].AutoEvalScore != 0.75 {
		t.Fatalf("auto eval score = %v, want 0.75", *again[0].AutoEvalScore)
	}
}

func writePromptRoot(t *testing.T, source string) string {
	t.Helper()

	root := t.TempDir()
	templateDir := filepath.Join(root, DefaultPromptName)
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("create prompt template dir: %v", err)
	}
	templatePath := filepath.Join(templateDir, DefaultPromptVersion+".tmpl")
	if err := os.WriteFile(templatePath, []byte(source), 0o644); err != nil {
		t.Fatalf("write prompt template: %v", err)
	}
	return root
}

func assertScore(t *testing.T, scores map[string]float64, metricName string, want float64) {
	t.Helper()

	got, ok := scores[metricName]
	if !ok {
		t.Fatalf("score %q is missing from %#v", metricName, scores)
	}
	if got != want {
		t.Fatalf("score %q = %v, want %v", metricName, got, want)
	}
}
