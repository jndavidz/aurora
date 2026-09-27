# 02 — Tracer bullet：DeepSeek chat quick 模式续轮贯通（Responses API 表面）

**What to build:** 端到端打通第一条会话复用路径——带 `X-Session-Key` 的连续两轮请求，第二轮复用上游 session，只发本轮增量。这是本 feature 的 tracer bullet：module（01）→ provider 接线 → 单测全程贯通。

具体行为：

- 连续对话期间同一 clientKey 复用已建立的上游 session：`parent_message_id` = 上轮 `response_message_id`，prompt 只含本轮新内容；复用时不调用 DeleteSession。
- 未命中 / TTL 过期 → 新开 session：prompt 为客户端本轮发来的内容（拍平函数保留，降级为「新会话引导」专用路径；不再有 TTL 过期后全量重放历史的 bootstrap）。TTL 过期即接受记忆断档，aurora 不做记忆。
- vision 请求不进池（与搜索互斥、`model_type` 特殊），维持现行独立会话路径。
- 仅 quick 模式；Responses API 表面（`/v1/responses`）。

**Blocked by:** 01 — Session Strategy deep module（seam 先行）

**Status:** ready-for-agent

- [ ] 带 `X-Session-Key` 连续两轮：第二轮续轮请求体断言 `parent_message_id` 非空、prompt 为增量（参照既有 provider 单测 flatten 断言风格）
- [ ] 首轮/未命中：新会话引导路径，prompt 为本轮内容，session 正常创建与关闭
- [ ] TTL 过期后下一轮：走新会话引导，不重放历史
- [ ] vision 请求不进池断言
- [ ] 手工验证：本地起服务，curl 两轮连续对话，第二轮可感知变快
