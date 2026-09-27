# 04 — chat completions 表面接入 + `#new#` 信令

**What to build:** `chat.completions` 表面（`/v1/chat/completions`）同样享受会话复用，并落地程序化新会话信令。

具体行为：

- `user` 字段作 clientKey（剥除 `#new#` 前缀后），缓存键规则与 Responses 表面一致。
- `#new#` 前缀强制新会话：剥前缀后走新会话引导，池内同 key 旧 entry 作废。
- `X-Session-Key` 头在两表面统一生效；`X-Session-Action: new` 头在两表面统一生效。
- 关键可测行为：不带 `X-Session-Key` 头且 `user` 为空的请求完全不进池，每次独立处理（一次性调用与连续对话互不干扰）。

**Blocked by:** 02 — Tracer bullet：DeepSeek chat quick 模式续轮贯通（Responses API 表面）

**Status:** done

- [x] `user` 字段作 clientKey：连续两轮同 `user` 值请求复用 session
- [x] `#new#` 前缀：剥前缀、强制新会话、池内旧 entry 作废断言
- [x] `X-Session-Key` 头与 `X-Session-Action: new` 头在 chat completions 表面生效
- [x] 无头且 `user` 为空：请求不进池，行为与现状一致
- [x] deepseek chat 现有单测与 handler 测试全绿（拍平函数保留，主路径改走 strategy）

## Comments

### 实现记录

- `chatCompletions` 主路径改走 `deepseekChatFlow`（与 Responses 表面同一 seam）：
  `chatStreamTurn` 同型新增 `chatStreamCompletion`；非流式复用 `flow.run`。
- clientKey 解析两表面统一走 `session.ResolveClientKey`（头 → user 剥 `#new#` → 空不进池），
  信令叠加语义无需在表面重复实现。
- 拍平函数保留：`deepseekTurnsFromItems` 收敛本轮输入（与 `flattenChatItems` 同口径
  跳过 function_call/function_call_output，守住「chat 变体不上游注入工具」硬规则）；
  拍平只在新会话引导路径生效，续轮只发末条。
- 顺带修复 ticket 02 接线时丢失的 `ref_file_ids`：vision 路径的已上传/fork 文件 id
  现在经 `deepseekChatFlow.refFileIDs` 随本轮请求发出（两表面共用）。
- 流式失败形态：头部与 role 块延迟到首个增量才写出——保证
  （a）降级重试对客户端不可见；（b）尚未开流的上游失败仍返回干净的 JSON 502
  （保留旧路径行为，不退化出半截 SSE）。已开流后失败发 `data: {"error":...}` + `[DONE]`。
- 测试：`deepseek_chat_completions_wire_test.go` wire 级回环（真 provider + 真 Pool +
  httptest 假上游）覆盖 user 复用/#new# 作废/头信令/无 key 绕行/流式复用/降级透明/
  未开流 502/工具剥离；vision `ref_file_ids` 断言补在 flow 级单测。
