# Chat usage invalid-response 分类修复

- **报告日期**：2026-08-28
- **修复日期**：2026-08-31
- **报告来源**：真实 chat live 失败复盘与 spec-kit T206 RED 契约
- **报告人**：项目维护者
- **严重程度**：P1

## 现象描述

OpenAI-compatible adapter 或未来 provider 将缺失、`null`、不一致的 usage 拒绝为
`llm.ErrInvalidResponse` 后，chat usecase 会将该错误归为通用 `provider_failure`，而不是稳定的
`invalid_response`。直接、包装以及同时返回 response+error 的形态都可复现。

## 根因

T215 已实现不可混淆的 `ProviderUsage` 与成功边界的 reported usage 快照，但
`ClassifyChatModelFailure` 只识别应用层 `ErrChatInvalidResponse`，遗漏了 provider 层
`llm.ErrInvalidResponse`。因此 provider 主动拒绝的不完整响应没有进入同一应用错误分类。

## 修复方案

在通用 upstream 分支之前识别 `llm.ErrInvalidResponse`，归一为
`ChatFailureClassInvalidResponse`，并直接返回应用层 `ErrChatInvalidResponse` sentinel。
不包装 provider 原始错误，避免响应正文、endpoint 或 credential 经错误链越过 adapter。

缺失 usage 继续在 generation/evaluator/evidence/API 成功投影前失败；reported zero 与合法非零
usage 仍以经过验证的值快照进入全部下游消费者。`evaluator.go` 已有一致性二次校验，无需重复
引入 availability 状态。

## 修改文件

- `internal/logic/chat/chat.go` — 增加 provider invalid-response 的稳定低敏归一化。
- `internal/logic/chat/chat_usage_test.go` — T206 正反契约验证 fail-closed、身份和事实隔离。
- `specs/003-real-observability-backends/tasks.md` — 记录 T216 实施及低敏验证证据。

## 关联 Commit

- 尚未提交；本轮未执行 `git add` 或 `git commit`。

## 验证结果

- [x] T206 的 7 个复现/正向子用例全部通过
- [x] chat 包 race 测试通过，覆盖率 93.3%
- [x] 代码、Go 与安全审查通过，无 CRITICAL/HIGH 问题
- [x] `go build ./...`、`go vet ./...` 与 `git diff --check` 通过
- [x] 最终全仓 race 通过；验证期间观察到的无关 SigNoz smoke 时序波动已隔离连续 5 次通过并在最终全仓重跑中通过

本修复未读取真实凭据，未启动 Docker，也未执行真实模型或付费 live。
