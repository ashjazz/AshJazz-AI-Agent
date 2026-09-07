package openai

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/ashjazz/Longtermism/pkg/ai/llm"
)

// openAIErrorBody 仅供已建立的 2xx SSE 流解析内嵌 error 事件。
// 初始非 2xx HTTP 响应不再读取或解析该结构。
type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// classifyHTTPStatusError 把 OpenAI-compatible HTTP 状态映射到框架错误语义。
//
// 429/5xx 属于可重试或可降级的上游错误，必须包装 llm.ErrUpstream；
// 400/401/403 这类调用方、认证或权限问题不能进入重试/熔断路径。
// 失败正文、Status 文本和 headers 都是供应商控制的输入，可能回显 prompt 或
// 凭据，因此只保留数值状态码和稳定 sentinel。这里不读取正文，也不为连接
// 复用执行 drain，避免超大/阻塞正文拖延失败路径；关闭仍由 Chat/ChatStream 负责。
func classifyHTTPStatusError(resp *http.Response) error {
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}

	message := fmt.Sprintf("openai chat request failed with status %d", resp.StatusCode)
	if isRetryableHTTPStatus(resp.StatusCode) {
		return fmt.Errorf("%s: %w", message, llm.ErrUpstream)
	}
	return errors.New(message)
}

func isRetryableHTTPStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}
