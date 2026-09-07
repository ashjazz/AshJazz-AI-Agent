# Provider usage：缺失事实不能伪装成零

- 日期：2026-08-28
- 关联任务：T205、T215（003-real-observability-backends）
- 关联模块：`pkg/ai/llm`、OpenAI adapter、Agent、chat、P0 smoke
- 状态：已修复并完成离线回归；非 live 验收

## 发生了什么

T205 的失败测试证明：非流式响应没有 `usage`、返回 `null` 或计数不一致时，旧 adapter 仍可能输出一个全零或无效的成功摘要。这会把“缺少计费事实”写成“模型调用了但没有消耗 token”。

T215 首轮审查又发现，直接匿名嵌入 `*Usage` 会暴露可写别名；将响应类型改为可选摘要后，旧消费者直接取字段还会触发空指针。新增 Agent/P0 smoke 回归已实际复现该崩溃。安全测试同时复现了超大成功正文、尾随 JSON 被接受，以及非法工具参数错误回显 call ID。

## 修复与取舍

- `ProviderUsage` 将 availability 与私有可选摘要绑定；构造 reported 事实必须通过非负、100,000,000 安全上限及 `input + output == total` 校验，失败显式返回错误。无效报告与未报告不是同一事实。
- `Summary()` 返回拷贝；显式全零可以 reported，零值对象和 unavailable 不会获得成功摘要。reasoning/cache 细分统计不重复加入总数。
- OpenAI 非流式 DTO 对顶层 usage 和三个字段都保留 presence；缺失、null、非法或不一致 usage 返回稳定 `invalid_response`，不携带原始正文。SSE 的既有映射不随本任务改变。
- 2xx 正文先限制在 1 MiB 内，再严格解析完整 JSON。正文超限、尾随数据和非法工具参数均失败；读取阶段的取消、超时、断流仍保留稳定传输分类，且不透传任意 reader 错误文本。
- Agent/P0 smoke 显式检查摘要。chat 只提取一次已校验摘要并传给后续投影，消除类型迁移引入的崩溃；修正旧成功 fixture 的不完整分量和重复累计总数，不增加兼容默认。
- 测试 fixture 的强制构造 helper 仅放在 `llm/testutil`；生产代码始终处理构造错误，不以 panic 处理上游输入。

这些取舍优先保证成本、预算和评估事实可解释。缺失 usage 会拒绝业务成功，不能以“看起来生成了答案”替代成功契约。

## 回归证据

本次按 RED → GREEN 执行 T205，以及值对象、正文边界、读取错误分类、Agent/P0 unavailable 的补充测试。最终验证：

- `go test -race ./... -count=1`：通过。
- `go vet ./...`：通过。
- `go test -race ./pkg/ai/llm/... -coverprofile=... -count=1`：LLM 核心 100%，OpenAI adapter 91.8%，testutil 89.5%。
- 代码、Go、安全审查的本轮阻断项均已修复。

staticcheck 未完成有效检查：本机工具链版本不兼容，受限环境执行也未实际匹配包；不能把退出成功当作检查通过。本次没有调用真实模型或生成新的 live 通过报告。

## 后续边界

T206/T216 仍保留未完成：本次最小安全迁移不替代 generation、evaluator、evidence、HTTP identity 的完整防御契约验收。

T215 收口时还发现非 2xx `openai/errors.go` 路径无正文读取上限，并透传 provider 的 message/type/code，当时未改写该 HTTP 错误契约。随后已按用户确认的“不解析错误正文，只保留 HTTP 状态和稳定分类”方案完成独立修复，见[非 2xx 错误正文隔离](../../bug-fix-notes/2026-08-28-http-error-body-isolation.md)。更细的供应商诊断留待后续单独完善，不能因此声称整个 adapter 的安全风险已清零。
