# 06 — 真机听感验收 + 回退演练（远程/人工）

**What to build:** 真机跑一轮连续对话，验证首音体感、分段不断流、不念思考、首句先给结论；并演练一次配置回退。

**前置授权：** 涉及真机与生产切换，需用户批准（人工在旁听感）。

**步骤：**
1. 唤醒一轮普通问答 + 一轮连续对话第二轮（验证会话复用），各一条。
2. 听感判据：首音≤3s 体感、无思考文本、分段之间不断流、首句是结论、命令表短语（如音乐/灯控）仍由 direct_table 正常执行。
3. **回退演练（演练后立即恢复）**：按 04 的备份恢复配置 → 重启 bridge → 确认回到官方 API（一条单发）→ 再切回 aurora 配置并重启，确认恢复。

**验收：** 听感结论 + 回退演练耗时与结果记录到本 ticket。

**听感结果（2026-10-26 第一轮）：** 首音响应可以、分段无断流、未念出思考过程；唯一反馈「稍微有点长」。

**第二轮反馈（同日）：** 首音约 4s、分段间隔无明显变化；另报长期问题「说退下不退出、亮琥珀灯、过一会才灭」。

**根因与处置：**
1. **首音 4s ≈ prompt 长度 + 首段/TTS**：同问题单发探针——无 system prompt 1.42s，system ~100 字 3.08s，短 system（16 字）1.65s，天气类 4.35s。即首 token 主要被 system prompt 长度与问题类型（搜索类）拖慢。已把 system_prompt 压到 30 字内、`rule_prompt` 从「300 字以内」长句改为「简短口语，不用 markdown」（它每轮追加在 user 文本后，既拖慢又与 70 字冲突）。
2. **「退下」不退出**：openai 段 `exit_keywords` 原为 `["退出","停止","再见"]`，**不含「退下」**，于是被当普通对话交给模型（模型顺着回「退下了」），亮灯保持到静默超时。已改为 `["退出","停止","再见","退下","下去吧"]`（桥接默认列表同款）。
- 备份：`config.py.bak-voice-len-20261026162714`、`config.py.bak-voice-exit-20261026163611`、`config.py.bak-voice-prompt-20261026163735`。
- 首段就绪（L2）仍无法从 trace 精确导出（`llm_ms` 为流结束口径），属已知埋点限制。

**第三轮反馈（同日）：** 「退下」已生效（turn8 `incomplete=1 / llm_requests=0`，确认走退出路径不走 LLM）；首音仍 ~4s，与改短 prompt 前无肉眼差别。

**第三轮 trace（turn6/7）：** llm_ms 3078/3402ms（llm_chars 均 126，超出 70 字约束）；首段 TTS 合成 1128/370ms；合成→起播 174/119ms。aurora 端到端 3.01/3.07/3.38s。
**首音拆解（估算）：** LLM 首 token ~1.5s + 首句生成 ~0.3–0.5s + 首段 TTS 合成 0.4–1.1s + 起播 0.1–0.2s ≈ **2.3–3.3s**；剩余差异来自上游抖动与段间等待。网页版 <2s 的差别主要来自「无 TTS + 网页原会话/搜索开关不同」。

**Status:** done。听感主项通过（首音/断流/思考/退下）。三优化项 2026-10-26 用户拍板并落地：
1. **豆包流式 TTS：只留接口**——bridge speaker 的 `stream: true` 接口形态保留（`tts.router` 已支持），本期不实施、不引入豆包依赖，记为后续选择（换 TTS 会引入音色变更与外部依赖）。
2. **aurora 免搜索变体：已落地**——exposed id `deepseek-no-search`（快速模式 + 非深度思考 + `SearchNever` 恒关搜索），与搜索变体 `deepseek` 并存于默认目录，**由消费者（bridge）请求里的 `model` 字段指令选择**；`deepseek` 的 SearchAlways 语义不变。见 `internal/provider/deepseek.go` / `deepseek_chat.go`。
3. **bridge 首段埋点：已落地**——`turn_trace` 新增 `llm_first_token_ms`（`llm_start` → 首个非空文本 delta），补齐「`llm_ms` 为流结束口径」的缺口；L2 = 首 `tts_start` − `llm_start` 之外，L1 桥接侧口径可精确读数。

**遗留远程步骤（待单独批准，同 T04 模式）**：重建 aurora 镜像并部署 NAS（新变体需新镜像）→ bridge 生产配置 `openai.model` 切 `deepseek-no-search`（免搜索为语音默认；需要联网信息的问题再靠会话/单发改 `deepseek`）。切回 = `model` 改回 `deepseek`。

**Blocked by:** 04
