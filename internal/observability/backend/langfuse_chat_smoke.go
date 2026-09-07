package backend

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/ashjazz/Longtermism/internal/observability/smoke"
)

type LangfuseChatSmokeQueryConfig struct {
	BaseURL     string
	Credential  string
	Timeout     time.Duration
	ResolveHost HostResolver
}

type LangfuseChatSmokeQueryClient struct{ query *negativeSmokeQueryClient }

func NewLangfuseChatSmokeQueryClient(config LangfuseChatSmokeQueryConfig) (*LangfuseChatSmokeQueryClient, error) {
	// 仓库锁定的 self-hosted Langfuse v3 使用 legacy v1 observations endpoint；v2
	// 只在 self-hosted v4 可用。这里仍使用 v3 已支持的结构化 filter，避免宽查询。
	query, err := newNegativeSmokeQueryClient("langfuse", "/api/public/observations", config.BaseURL, config.Credential, config.Timeout, config.ResolveHost)
	if err != nil {
		return nil, err
	}
	return &LangfuseChatSmokeQueryClient{query: query}, nil
}

// privacyObservationsDocument 为 Langfuse trace privacy surface 执行封闭的 observations v1
// 查询并返回原始有界平台文档。隐私适配器必须对完整文档做 scan，因此这里不做 DTO 投影：
// 任何字段级解码都会在扫描前丢弃平台实际返回的内容，等于伪造零命中。
func (client *LangfuseChatSmokeQueryClient) privacyObservationsDocument(ctx context.Context, filter string, startedAt, deadline time.Time, limit int) ([]byte, error) {
	if client == nil || client.query == nil || ctx == nil || ctx.Err() != nil {
		return nil, newBackendQueryError("langfuse", "invalid_query")
	}
	return client.query.get(ctx, url.Values{
		"limit":         {strconv.Itoa(limit)},
		"page":          {"1"},
		"filter":        {filter},
		"fromStartTime": {startedAt.UTC().Format(time.RFC3339Nano)},
		"toStartTime":   {deadline.UTC().Format(time.RFC3339Nano)},
	})
}

func (client *LangfuseChatSmokeQueryClient) Query(ctx context.Context, target smoke.ChatSmokeTarget) ([]smoke.ChatObservation, error) {
	if client == nil || client.query == nil || !validChatSmokeTarget(target) {
		return nil, newBackendQueryError("langfuse", "invalid_query")
	}
	filter, err := langfuseChatFilter(target)
	if err != nil {
		return nil, newBackendQueryError("langfuse", "invalid_query")
	}
	body, err := client.query.get(ctx, url.Values{
		"limit": {strconv.Itoa(target.Limit)}, "page": {"1"}, "filter": {filter},
		"fromStartTime": {target.StartedAt.UTC().Format(time.RFC3339Nano)},
		"toStartTime":   {target.Deadline.UTC().Format(time.RFC3339Nano)},
	})
	if err != nil {
		return nil, err
	}
	return decodeLangfuseChatObservations(body, target)
}

func langfuseChatFilter(target smoke.ChatSmokeTarget) (string, error) {
	if !validChatSmokeTarget(target) {
		return "", errors.New("unsafe chat target")
	}
	// v3.185 的服务端查询只使用平台原生 identity 与有界时间窗来缩小候选集。
	// OTel correlation 位于 metadata.attributes，必须从真实返回行验证；把它们
	// 当作旧 metadata filter 会让有效 observation 在服务端被错误排除。
	filters := []map[string]string{
		{"type": "string", "column": "traceId", "operator": "=", "value": target.ServiceTraceID},
		{"type": "string", "column": "id", "operator": "=", "value": target.SpanID},
		{"type": "datetime", "column": "startTime", "operator": ">=", "value": target.StartedAt.UTC().Format(time.RFC3339Nano)},
		{"type": "datetime", "column": "startTime", "operator": "<=", "value": target.Deadline.UTC().Format(time.RFC3339Nano)},
	}
	encoded, err := json.Marshal(filters)
	return string(encoded), err
}

type langfuseChatObservationRow struct {
	ID        string `json:"id"`
	TraceID   string `json:"traceId"`
	StartTime string `json:"startTime"`
	Metadata  *struct {
		Attributes map[string]json.RawMessage `json:"attributes"`
	} `json:"metadata"`
}

type langfuseChatPagination struct {
	Page       *int `json:"page"`
	Limit      *int `json:"limit"`
	TotalItems *int `json:"totalItems"`
	TotalPages *int `json:"totalPages"`
}

func decodeLangfuseChatObservations(body []byte, target smoke.ChatSmokeTarget) ([]smoke.ChatObservation, error) {
	var response struct {
		Data *[]langfuseChatObservationRow `json:"data"`
		Meta *langfuseChatPagination       `json:"meta"`
	}
	if json.Unmarshal(body, &response) != nil || response.Data == nil || response.Meta == nil ||
		response.Meta.Page == nil || response.Meta.Limit == nil || response.Meta.TotalItems == nil || response.Meta.TotalPages == nil {
		return nil, newBackendQueryError("langfuse", "malformed_response")
	}
	data := *response.Data
	expectedPages := 0
	if len(data) > 0 {
		expectedPages = 1
	}
	if *response.Meta.Page != 1 || *response.Meta.Limit != target.Limit || *response.Meta.TotalItems != len(data) ||
		*response.Meta.TotalPages != expectedPages || len(data) > target.Limit || len(data) > 1 {
		return nil, newBackendQueryError("langfuse", "malformed_response")
	}
	if len(data) == 0 {
		return nil, nil
	}
	item := data[0]
	observedAt, err := time.Parse(time.RFC3339Nano, item.StartTime)
	if item.Metadata == nil {
		return nil, newBackendQueryError("langfuse", "malformed_response")
	}
	marker, markerOK := requiredMetadataString(item.Metadata.Attributes, "longtermism.smoke.run_id")
	requestID, requestOK := requiredMetadataString(item.Metadata.Attributes, "request.id")
	aiTraceID, aiTraceOK := requiredMetadataString(item.Metadata.Attributes, "longtermism.ai.trace_id")
	if err != nil || !chatObservationInWindow(observedAt, target) || item.ID != target.SpanID || item.TraceID != target.ServiceTraceID ||
		!markerOK || marker != target.Marker || !requestOK || requestID != target.RequestID || !aiTraceOK || aiTraceID != target.AITraceID {
		return nil, newBackendQueryError("langfuse", "malformed_response")
	}
	// 成功结果只投影已经从平台响应逐项验证的事实，禁止从 query target 回填
	// 缺失 identity，否则旧数据或 schema 漂移会被误报为当前 run 的证据。
	return []smoke.ChatObservation{{
		Marker: marker, RequestID: requestID, AITraceID: aiTraceID,
		ServiceTraceID: item.TraceID, SpanID: item.ID, ObservedAt: observedAt.UTC(),
	}}, nil
}

func requiredMetadataString(metadata map[string]json.RawMessage, key string) (string, bool) {
	var value string
	err := json.Unmarshal(metadata[key], &value)
	return value, err == nil && value != ""
}

var _ chatObservationQuery = (*LangfuseChatSmokeQueryClient)(nil)
