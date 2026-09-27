# 05 — live 验证 + expert 回退开关

**What to build:** 真实上游的单次手工验证与 expert 档回退开关。

测试纪律：live 测试 = 消耗真实账号的真人化操作，单发、间隔 ≥2s，单条验证即止，不复跑不重放；由 `DS_TEST_TOKEN` 等环境变量守卫，缺省自动跳过。

具体内容：

- live 验证（手工单次各一条）：quick 模式续轮 + expert 模式续轮，验证 `parent_message_id` 行为与 thinking 语义。
- 上游会话有效期未实测（可能短于 30min TTL）：失败降级路径已兜底，验证最坏情况退回每轮新开时体验无中断。
- 若 expert 续轮上游异常：落地配置开关让 expert 档退回每轮新开、quick 档保持复用。

**Blocked by:** 02 — Tracer bullet：DeepSeek chat quick 模式续轮贯通（Responses API 表面）

**Status:** ready-for-agent

- [ ] quick 模式 live 续轮一条：`parent_message_id` 生效、响应正常、延迟较首轮明显下降
- [ ] expert 模式 live 续轮一条：thinking 语义符合预期
- [ ] expert 异常时的回退开关落地（配置项 + 默认行为确定）；expert 正常则开关仅备而不用
