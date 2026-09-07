# 非 2xx 错误正文隔离

- 报告日期：2026-08-28
- 修复日期：2026-08-28
- 报告来源：T215 审查及用户后续确认
- 报告人：项目维护者
- 严重程度：P1（潜在敏感信息泄露与无界读取风险，未确认真实泄露事件）

## 现象描述

Chat 和 ChatStream 收到初始非 2xx HTTP 响应时，会读取供应商错误正文并将 message/type/code 拼入错误。兼容网关若回显请求或凭据，错误链可能携带高敏数据；超大或阻塞正文也会拖延失败路径并增加资源消耗。

## 根因

两条调用路径共用 `classifyHTTPStatusError`，其中 `decodeErrorBody` 无界解析 JSON，`buildHTTPErrorMessage` 将未经安全边界处理的供应商字段作为诊断信息。旧测试还要求保留原始错误文案，只检查“不包含测试 API key”，没有验证供应商主动回显敏感信息的情况。

## 修复方案

用户明确选择当前简化方案：**不读取、不解析错误正文，只保留数字 HTTP 状态和既有稳定错误分类**。

- 移除非 2xx 正文解析及字段拼接，不读取 headers 或 `Status` 文本。
- 429/5xx 保持 `ErrUpstream`，400/401/403 等保持非重试类别，不顺带新增 `ErrRateLimit` 或 `invalid_response` 分类。
- 分类器不关闭正文；原有 Chat/ChatStream 路径各关闭一次。关闭错误不覆盖 HTTP 分类，也不透传原始错误。
- 不执行 drain，因此可能降低 HTTP/1.1 错误连接的复用率；这是避免正文读取及等待的明确取舍。
- 无数据库、配置、依赖或公开接口变更；不改变成功响应解析及 2xx SSE 流内错误处理。

## 修改文件

- `pkg/ai/llm/openai/errors.go` — 移除非 2xx 正文解析，只保留状态与分类。
- `pkg/ai/llm/openai/errors_test.go` — 新增 64 个 Chat/ChatStream × 状态 × 正文形态组合，验证零读取、关闭一次和无泄露。
- `pkg/ai/llm/openai/provider_test.go`、`stream_test.go` — 将旧的正文回显断言改为安全状态断言，保留真实本地 HTTP 测试。
- `docs/journal/0014-provider-usage-facts.md` — 链接本次后续修复，保留 T215 原始审查背景。

## 关联 Commit

未暂存或提交；保留为工作区修改，已有 T214/T215 修改未回退。

## 验证结果

- [x] 新增测试先 RED：旧实现读取正文，合法 JSON 中的标记进入错误链。
- [x] 相同测试修复后 GREEN：所有非 2xx 组合 `Read=0`、`Close=1`，错误只含数字状态和稳定分类。
- [x] `go test -race ./pkg/ai/llm/... ./pkg/ai/resilience -count=3`
- [x] `go test -race ./... -count=1`
- [x] `go build ./...`、`go vet ./...`
- [x] OpenAI adapter 覆盖率 92.5%；本次错误分类函数及状态判定函数均为 100%。
- [x] 通用、Go、安全审查均通过，无本次新增阻断问题。

本次未执行 live 调用；staticcheck 的既有本机工具链问题不在此修复范围，未声明其检查通过。

## 回滚与后续完善

若出现错误分类或资源关闭回归，仅撤销本次独立修改，不整文件恢复包含 T215 改动的 `provider_test.go`。后续若独立提交，可定向回退对应提交；无需数据库或配置回滚。回退会恢复旧安全风险，因此在替代修复完成前应暂停 live，而不是恢复原文诊断后继续使用。

按用户要求，后续另行评估有界、白名单化的供应商诊断策略及连接复用取舍。不得未经新契约和泄露测试直接恢复正文拼接。SSE 流内错误及无 HTTP 响应的 transport 错误是独立边界，本次通过不代表整个 adapter 已完成全面安全验收。
