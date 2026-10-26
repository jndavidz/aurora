# 05 — 三段延迟量化验收（远程 + live）

**What to build:** 按 spec WP3 的三段口径量化首音改善；每次单变量、单发、真人节奏（间隔 ≥2.5s），不复跑不重放。

**前置授权：** 会消耗真实账号，需用户批准；遵守防风控纪律（单发、人工节奏、不批量）。

**测量项：**
- **L1 首轮**：aurora chat/completions 流式端点首个 delta（与 2026-10-26 测速同 prompt、同计时法）——目标 ≤2.0s。
- **L1 续轮**：同 `X-Session-Key` 的第二轮（间隔 <TTL）——目标 ≤1.5s。
- **L2 bridge 首段就绪**：`turn_trace` 首条 `tts_start` − `llm_start`——目标 ≤2.6s。
- **L3 真机首音**：`turn_trace` 首条 `play_url` − `llm_start`——只记基线（xiaoai 原生 TTS，不设门槛）。

**决策门（单变量）：** L1 超 2.0s → 关 `DEEPSEEK_WEB_SEARCH` 复测一条；L1 达标 L2 不达标 → 收紧首句约束；L2 达标 L3 差 → 查 TTS（豆包另立项，不在本期）。

**验收：** 四项数据与门槛结论写入本 ticket；未达标项附单变量复测记录与处置。

**Status:** pending

**Blocked by:** 04
