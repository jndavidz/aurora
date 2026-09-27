# 03 — 失败降级 + token 失效

**What to build:** 续轮失败对客户端透明的自动恢复。

具体行为：

- 续轮 completion 失败（上游 4xx / session 失效 / 风控）：丢弃池内 entry → 新开 session 重试一次 → 仍失败则返回错误。客户端全程无感。
- token 轮换语义：session 与建立时的 token 绑定；池内 token 热加载或轮换后旧 entry 视为失效（降级新开）。

**Blocked by:** 02 — Tracer bullet：DeepSeek chat quick 模式续轮贯通（Responses API 表面）

**Status:** ready-for-agent

- [ ] fake 上游注入续轮失败：断言池内 entry 被丢弃、自动新开 session 重试一次成功
- [ ] fake 上游连续失败：重试一次后仍失败则向客户端返回错误（不无限重试）
- [ ] token 热加载/轮换后：池内旧 entry 失效，下一轮降级新开
- [ ] 降级路径对客户端响应形态与正常路径一致（无错误外泄）
