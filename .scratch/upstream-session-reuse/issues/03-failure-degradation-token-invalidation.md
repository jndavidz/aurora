# 03 — 失败降级 + token 失效

**What to build:** 续轮失败对客户端透明的自动恢复。

具体行为：

- 续轮 completion 失败（上游 4xx / session 失效 / 风控）：丢弃池内 entry → 新开 session 重试一次 → 仍失败则返回错误。客户端全程无感。
- token 轮换语义：session 与建立时的 token 绑定；池内 token 热加载或轮换后旧 entry 视为失效（降级新开）。

**Blocked by:** 02 — Tracer bullet：DeepSeek chat quick 模式续轮贯通（Responses API 表面）

**Status:** done

- [x] fake 上游注入续轮失败：断言池内 entry 被丢弃、自动新开 session 重试一次成功
- [x] fake 上游连续失败：重试一次后仍失败则向客户端返回错误（不无限重试）
- [x] token 热加载/轮换后：池内旧 entry 失效，下一轮降级新开（Pool 需新增 token 有效性校验口子，见 ticket 01 review 备注）
- [x] 降级路径对客户端响应形态与正常路径一致（无错误外泄）

## Comments

### 实现记录 (2026-09-27)

- **token 口子落地**（ticket 01 review 备注排期项）：`Pool.Acquire/AcquireNew` 增加第三参 `token`；`Acquire` 命中条件加 `e.token == token`，不等即摘旧条 + 删上游 + 新开（`TestAcquireTokenRotationInvalidatesEntry`）；`newLease` 不再硬编码空 token——**修复前带 `X-Session-Key` 的请求 100% 502**（`newLease(clientKey, modelID, "")`）。wire 级 `TestWireTokenRotationInvalidatesPoolEntry` 用真 token 文件 mtime 轮换直证降级新开。
- **流式降级路径修复两处**：① `runOnce` 原先以 `(nil, false)` 报告发送失败，`!ok` 分支读 `res.Err` 构成 nil 解引用 → 连续失败必然 panic（`TestStreamDoubleFailureEmitsFailedEvent` 复现，red → 改为透传 `error`）；② SSE header 原在建会话前设置，导致 acquire 失败时 `JSONError` 写出 `Content-Type: text/event-stream` 的 JSON body —— 现改为 acquire 成功后才建 SSE writer。
- **透明性**：上报失败路径 flushing 前正文/思维链皆空（成功时才 flush delta），故重试对客户端完全透明（`TestStreamDegradeRetryIsTransparent` 断言输出流不含 `response.failed`）；重试仍失败才发单个 `response.failed` 事件（不走 JSON 错误，SSE 头已发）。
- 三层覆盖：fake pool 分支（`deepseek_chat_session_test.go`）→ 流式客户端形态（`deepseek_chat_stream_test.go`）→ wire 回环真池 + 真 token 文件（`deepseek_chat_wire_test.go`）。
- 未做（ticket 04/05 范围）：`/v1/chat/completions` 表面未接池；live 验证待做。
