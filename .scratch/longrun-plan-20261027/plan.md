# Plan: 长时间离线期间的可执行工作计划（2026-10-27）

Status: ready-for-agent（用户回来后逐项触发；标注「需用户」的项不可无人值守）

来源：用户 2026-10-27 指示「安排长时间工作计划」；对接核实与延迟盘点结论见本目录 issues/ 与
memory `aurora-consumer-openxiaoai-latency`。业界对标（stream everywhere / time-to-first-audio
优先 / ≤1s 自然阈值）来自 2026-10-27 联网调研（arXiv 2508.04721、Deepgram/Twilio/RTC League 指南）。

## A 层：零上游接触，可无人值守（回来后可让我直接跑）

- **A1 其余 `*web` 通道 UA 对齐**（spec Further Notes 遗留项）：glm/doubao/grok/kimi/qianwen/yuanbao
  Chrome/144、minimax/mimo Chrome/146 → Chrome/150；首选「由 TLS profile 同源生成」方案
  （spec 已写明方向），侵入面大则退字面量对齐。纯本地 + 单测，`-p 1` 防 OOM。
- **A2 架构深化扫描**：跑 improve-codebase-architecture 只读扫描出报告，候选进下一期 spec。
- **A3 文档一致性**：`bash /mnt/d/_work/dev/scripts/check/doc-consistency-check.wsl.sh`
  排查过时断言（voice-latency 收口后重点查「官方 API 直连」「单一 exposed id」类残留）。

## B 层：代码就绪，等触发条件（回来后第一件事）

- **B1 bridge 镜像重部署**（埋点 `llm_first_token_ms` 进生产）：**前置 = B-29 重构系列收尾**
  （main 未推送 6 commits，B-29 仅到 S4 step 2，另一会话进行中；代码烤进镜像，不能带半成品）。
  路径：bridge/deploy/REBUILD.md 路径 A（WSL build → save/load → compose up）；
  部署后验证启动行 + 真机一轮看 `llm_first_token_ms` 读数。
- **B2 真机听感验收**（需用户在场）：免搜索变体首音对比 + `llm_first_token_ms` 量化 L2；
  联网类问题单发改 `deepseek` 验证搜索变体仍可用。

## C 层：新立项候选（需拍板，按延迟收益排序）

对标业界：当前链路 L3 ~4s vs 自然阈值 ~1s；LLM TTFT ~1.5s（网页逆向硬约束）之外，
**TTS 首段合成 0.4–1.1s 是最大可压缩段**（业界流式 TTS 起播 ~0.1–0.3s）。

- **C1 豆包流式 TTS**（接口已留）：把首段合成从阻塞落盘改为边合成边播；用户曾缓办，
  回来后重新拍板——这是对标数据下剩余空间最大的一项。
- **C2 bridge 换话题 `X-Session-Action: new` 接线**：小代码量；消除「换话题仍带旧上下文 +
  池内旧会话」的语义问题。
- **C3 VAD/endpointing 调优**（bridge 侧从未动过）：业界经验「激进 endpointing 显著压体感首音」；
  需真机配合调参。
- **C4 mimo 通道故障修复**（凭证/协议待查；live 单发验证）。

## 纪律（所有项通用）

live 一律单发、真人节奏；凭证不回显不落文档；git 走 WSL；bridge 配置变更双层同看 +
重启看 `[OpenAI] Enabled` 启动行；部署后回读验证。
