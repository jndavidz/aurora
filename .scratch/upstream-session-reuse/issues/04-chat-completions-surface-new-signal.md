# 04 — chat completions 表面接入 + `#new#` 信令

**What to build:** `chat.completions` 表面（`/v1/chat/completions`）同样享受会话复用，并落地程序化新会话信令。

具体行为：

- `user` 字段作 clientKey（剥除 `#new#` 前缀后），缓存键规则与 Responses 表面一致。
- `#new#` 前缀强制新会话：剥前缀后走新会话引导，池内同 key 旧 entry 作废。
- `X-Session-Key` 头在两表面统一生效；`X-Session-Action: new` 头在两表面统一生效。
- 关键可测行为：不带 `X-Session-Key` 头且 `user` 为空的请求完全不进池，每次独立处理（一次性调用与连续对话互不干扰）。

**Blocked by:** 02 — Tracer bullet：DeepSeek chat quick 模式续轮贯通（Responses API 表面）

**Status:** ready-for-agent

- [ ] `user` 字段作 clientKey：连续两轮同 `user` 值请求复用 session
- [ ] `#new#` 前缀：剥前缀、强制新会话、池内旧 entry 作废断言
- [ ] `X-Session-Key` 头与 `X-Session-Action: new` 头在 chat completions 表面生效
- [ ] 无头且 `user` 为空：请求不进池，行为与现状一致
- [ ] deepseek chat 现有单测与 handler 测试全绿（拍平函数保留，主路径改走 strategy）
