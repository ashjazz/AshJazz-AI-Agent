package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
)

// 非 2xx 只依据状态码分类。Read=0 比“读取有上限”更严格：无论正文多大、
// 是否合法 JSON 或会否阻塞，都不得影响失败路径的延迟、分配和错误内容。
func TestProviderHTTPStatusErrorsIgnoreBody(t *testing.T) {
	t.Parallel()
	statuses := []struct {
		code     int
		upstream bool
	}{
		{302, false}, {400, false}, {401, false}, {403, false},
		{429, true}, {500, true}, {503, true}, {599, true},
	}
	bodies := []struct {
		name      string
		newReader func() io.Reader
	}{
		{"json", func() io.Reader {
			return strings.NewReader(`{"error":{"message":"body-message-canary","type":"body-type-canary","code":"body-code-canary"}}`)
		}},
		{"malformed", func() io.Reader { return strings.NewReader("<html>body-canary</html>") }},
		{"empty", func() io.Reader { return strings.NewReader("") }},
		{"unreadable", func() io.Reader { return chatBodyErrorReader{errors.New("read-error-canary")} }},
	}
	for _, stream := range []bool{false, true} {
		for _, status := range statuses {
			for _, body := range bodies {
				t.Run(fmt.Sprintf("stream=%t/status=%d/%s", stream, status.code, body.name), func(t *testing.T) {
					tracked := &httpErrorTrackingBody{reader: body.newReader()}
					err := callProviderWithHTTPError(t, stream, status.code, tracked)
					assertStatusOnlyError(t, err, status.code, status.upstream)
					if tracked.readCalls != 0 || tracked.closeCalls != 1 {
						t.Fatalf("body Read/Close = %d/%d, want 0/1", tracked.readCalls, tracked.closeCalls)
					}
				})
			}
		}
	}
}

func callProviderWithHTTPError(t *testing.T, stream bool, status int, body io.ReadCloser) error {
	t.Helper()
	provider, err := NewProvider(Config{
		BaseURL: "https://endpoint-canary.example/v1", APIKey: "test-key-canary", DefaultModel: "test-model",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status, Status: fmt.Sprintf("%d status-text-canary", status),
				Header: http.Header{"X-Provider-Diagnostic": {"header-canary"}},
				// 故意声明极大长度，验证不根据不可信长度分配或读取正文。
				ContentLength: 1 << 40, Body: body, Request: req,
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := &llm.ChatRequest{Model: "test-model", Messages: []llm.Message{{Role: llm.RoleUser, Content: "request-canary"}}}
	if stream {
		chunks, err := provider.ChatStream(context.Background(), request)
		if chunks != nil {
			t.Fatal("non-2xx response must not start a stream")
		}
		return err
	}
	response, err := provider.Chat(context.Background(), request)
	if response != nil {
		t.Fatal("non-2xx response must not return provider facts")
	}
	return err
}

func assertStatusOnlyError(t *testing.T, err error, status int, upstream bool) {
	t.Helper()
	if err == nil || errors.Is(err, llm.ErrUpstream) != upstream {
		t.Fatalf("HTTP error lost existing retry classification: error_present=%t", err != nil)
	}
	want := fmt.Sprintf("openai chat request failed with status %d", status)
	if upstream {
		want += ": " + llm.ErrUpstream.Error()
	}
	if err.Error() != want {
		t.Error("HTTP error contains more than numeric status and stable classification")
	}
	// 不能只清理 Error() 文案而把原始错误留在 unwrap 链供日志再次展开。
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if strings.Contains(cause.Error(), "canary") {
			t.Error("HTTP error chain leaked provider-controlled data")
		}
	}
	if errors.Is(err, llm.ErrInvalidResponse) || errors.Is(err, llm.ErrRateLimit) {
		t.Error("HTTP error unexpectedly changed existing sentinel semantics")
	}
}

type httpErrorTrackingBody struct {
	reader                io.Reader
	readCalls, closeCalls int
}

func (body *httpErrorTrackingBody) Read(p []byte) (int, error) {
	body.readCalls++
	return body.reader.Read(p)
}

func (body *httpErrorTrackingBody) Close() error {
	body.closeCalls++
	// 清理失败也不能覆盖已有状态分类或泄露低层错误。
	return errors.New("close-error-canary")
}
