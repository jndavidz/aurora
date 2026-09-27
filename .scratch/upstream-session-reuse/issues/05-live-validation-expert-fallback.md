# 05 — live 验证 + expert 回退开关

**What to build:** 真实上游的单次手工验证与 expert 档回退开关。

测试纪律：live 测试 = 消耗真实账号的真人化操作，单发、间隔 ≥2s，单条验证即止，不复跑不重放；由 `DS_TEST_TOKEN` 等环境变量守卫，缺省自动跳过。

具体内容：

- live 验证（手工单次各一条）：quick 模式续轮 + expert 模式续轮，验证 `parent_message_id` 行为与 thinking 语义。
- 上游会话有效期未实测（可能短于 30min TTL）：失败降级路径已兜底，验证最坏情况退回每轮新开时体验无中断。
- 若 expert 续轮上游异常：落地配置开关让 expert 档退回每轮新开、quick 档保持复用。

**Blocked by:** 02 — Tracer bullet：DeepSeek chat quick 模式续轮贯通（Responses API 表面）

---

## Comments

### 实现记录 (2026-09-27 夜)

**live 验证（真实上游，单次手工）**

- 上游 `chat_session/create` 回包带 `ttl_seconds=259200`（72h）——远长于 spec 假设的 30min TTL，记忆断档风险比预估低。
- **quick(default) 续轮**：turn1 1.098s → turn2 815ms（↓26%）；turn2 问「我刚才说的数字是多少」正确答出上轮 `47` → 服务端按 session+parent_message_id 记忆生效。`creates==1` 证明续轮未新开 session。
- **expert(thinking) 续轮**：turn1 1.462s → turn2 871ms（↓40%）；同样答出 `47`，且两轮 reasoning 均非空（thinking 语义与续轮兼容）。
- 结论：expert 续轮**无上游异常**，故开关默认保持「复用」，仅备而不用。

**live 验证暴露并修复的 P0 缺陷**

- 原 `session.Pool.newLease` 用硬编码空 token 建上游会话 → 带 `X-Session-Key` 的请求 100% 以 `40003 Authorization Failed` 落成 502；ticket 02 的 wire 测试漏掉了 token 透传断言（假上游不校验 token，所以一直绿）。
- 修复：`Acquire/AcquireNew` 增加 `token` 参数，session 与建立时 token 绑定；池内 entry 的 token 与本轮 token 不同即视为失效（降级新开），同时落地 spec §token 轮换语义。
- 红→绿测试：`session.TestAcquirePassesTokenToUpstream`、`TestAcquireTokenRotationInvalidatesEntry`。

**expert 回退开关（预防性，默认不用）**

- 配置项 `DEEPSEEK_EXPERT_RESUME`（默认 `true`）。置 `0` 时 expert 档 `noResume=true` → 不进池、每轮新开引导；quick 档不受影响。
- 接线点：`deepseekChatFlow.noResume`（并入 `poolable()`）+ `DeepSeek.resumeDisabled(m)`（`chatResponses` 入口）。
- 测试：`config.TestDeepSeekExpertResumeFlag`、`provider.TestLoopExpertResumeDisabledBypassesPool`、`TestLoopExpertResumeDefaultStillPools`。
- 文档：`env.template` 新增该开关说明。

**live 测试落点**：`internal/provider/deepseek_resume_live_test.go`（`DS_TEST_TOKEN` 守卫，缺省 `SKIP`；两轮间隔 3s 对齐真人节奏；`t.Errorf` 而非 `Fatal`，便于单次运行拿到两档全部观察值）。

**Status:** done

- [x] quick 模式 live 续轮一条：`parent_message_id` 生效、响应正常、延迟较首轮明显下降
- [x] expert 模式 live 续轮一条：thinking 语义符合预期
- [x] expert 异常时的回退开关落地（配置项 + 默认行为确定）；expert 正常则开关仅备而不用
