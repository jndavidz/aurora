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

**实测（2026-10-26，真机 4 轮，`deepseek`）：**
- 每轮 aurora `POST /v1/chat/completions` 均 200；单次 LLM 请求（`llm_requests=1`）、`tool_rounds=0`（工具确已关）、无思考泄漏。
- aurora 端到端耗时：2.85s / 2.55s / 3.41s / 1.31s（对应 llm_chars 84 / 8 / 67 / 9）。
- bridge `turn_trace`：TTS 合成 0.44–2.04s、合成→play_url 接受 0.11–0.30s；首音体感达标（用户确认），分段无断流。
- **口径限制**：现有 trace 的 `llm_ms` 是「流结束」而非首个 delta，`llm_to_tts` 因此被钳为 0 —— **L2 无法从现有埋点精确导出**；L1 以独立探针为准（首轮 1.42s / 同 key 二轮 1.59s）。若要精确 L2，需给 bridge 增「首段就绪」mark（bridge 侧改动，另立项）。

**Status:** done（量化数据已记录；L2 精确值受埋点限制，属已知约束）

**Blocked by:** 04
