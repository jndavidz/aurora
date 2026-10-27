# 07 — Review 修复（P1×4 + P2×9）

**Type:** task

**What to build:** 按代码审查结论修 `feat/provider-tool-calling` 上的问题。基线：`a8b8b7c`（T06 已并入）。**不改 `internal/toolcall`**。

## P1（必须修，附回归用例）

### 7.1 工具结果轮不得再催工具（`internal/handler/provider_tools.go`）
ChatGPT 通道有守卫（`tool_calling.go:304-306`）：**末条消息 role 是 `tool`/`function` 且文本非空** → 直接接受文本，不重试。
新路径缺失 → bridge 第二轮（末条 `role=tool`）模型给终答「已开始播放」时会被 nudge 逼着再吐 `song_request` → **重复点歌/重复发红外**（可反复到重试耗尽）。
修法：重试前判 `req.Messages` 末条 role；命中即接受。
用例：末条 `role=tool` + 假收集器返回纯文本 → 只调用 1 次、返回文本、不追加 nudge。
调换顺序：先判「是否该接受」，再判「是否该重试」。

### 7.2 去掉 `RecoverFromText` 兜底（安全）
`RecoverFromText` 会走 `InferToolFromParams`：正文里任何「key 是某工具参数子集」的 JSON 都会被伪造成调用。真实场景：模型解释「`song_request` 需要 `{"query":"歌曲名"}`」→ 网关点歌「歌曲名」并真的起播。
修法：A 路径只保留 `Feed` → `Flush`，**删除 `RecoverFromText` 兜底**（`Flush` 已覆盖未闭合标签）。同时在代码注释写明「宁可降级为文本，也不猜工具调用」，并同步 `docs/ARCHITECTURE.md` §5.5 的解析段（去掉 RecoverFromText、补一句理由）。
用例：正文含 `我记得 song_request 需要 {"query": "歌曲名"} 这种参数` + 声明了 song_request → **不得**产生 tool_calls，返回纯文本。

### 7.3 native 槽位要尊重熔断（`internal/handler/chat_handler.go` 两处）
B 分派只看 `native != nil && native.Handles(model)`，`ProviderBreaker.Tripped` 无人消费 → 上游持续 5xx 时所有工具请求打空，A 永远轮不到。
修法：加 `!h.providers.Breaker().Tripped(native.Name())`（`Breaker()` 可能为 nil 时已判 nil，验证一下）。熔断期间自然落到 A。
用例：手工 `Breaker().RecordFailure("openai-native", ...)` 三次 → 带 tools 且槽位命中的请求走 A（或 provider 现状），不走 native。

### 7.4 重试耗尽要保留有效文本；全空不得 200（`provider_tools.go`）
现状每轮覆盖 `text`，耗尽时用末轮文本；末轮若为空串 → 200 + `content:""`，bridge 判「没收到回复」。
修法：跟踪「最后一个非空文本」；耗尽时若有非空文本则用它，否则 `apierrors.JSONError(c, 502, "api_error", "provider tool emulation: empty reply", nil, "no_valid_reply")`。流式同理：空文本时流尚未开始，走同一 502。
用例：三轮分别返回「完整答案」→ "" → "" → 返回「完整答案」；三轮全空 → 502 `no_valid_reply`。

## P2（同批修，除注明「只改文档」的）

### 7.5 `tool_choice:"none"` 不注入、不重试
客户端显式禁止工具时，注入协议 + nudge 会强迫模型违反契约。门禁改成看 `req`：`ToolChoice.IsForcedNone()` → 直接走 provider 原路径。用例：`ToolChoice{Type:"none"}` + tools + 开关开 → 不进模拟。

### 7.6 `-coding` 变体不得 502
门禁只看「provider + TextCollector + tools」；`CollectText` 对非 chat 变体返回 error → 502（改动前是正常 coding 流程）。
修法：门禁跳过 `-coding` 后缀模型（沿用 handler 既有的 coding 判定，别新造规则）。用例：`deepseek-coding` + tools + 开关开 → 走 `p.ChatCompletions`。

### 7.7 重试加退避（防风控）
重试间无间隔，且每轮新开会话/重传图片/可能换 token。最少修：重试前 `select` 一个 context-aware 退避（例如 `400ms * attempt`，上限 2s；`c.Request.Context().Done()` 直接退出）。用例：用可注入的 clock/sleep 或断言总耗时不小于退避下界（避免 flaky：用 `time.Sleep` 缩短到毫秒级可配置，或用接口注入）。图片/token 复用**不做**（要改 `TextCollector` 签名，超出本批范围）——在 `docs/ARCHITECTURE.md` §5.5 补一句「重试是 N 次独立收集（新会话/重传图），仅在失败路径发生」。

### 7.8 native 流式缺 SSE 头
只设了 `Content-Type`。同包已有 `sseHeaders`（`internal/provider/wire.go:23-28`）→ 流式时复用（`Cache-Control: no-cache`、`Connection: keep-alive`、`X-Accel-Buffering: no`）。用例：流式响应头断言三件套。

### 7.9 native 消息重序列化白名单化
`APIMessage` 的 `attachments` 是 aurora 扩展，会泄漏给严格上游 → 400。修法：`nativeToolsMessages` 里 delete `attachments`（保留 role/content/tool_calls/tool_call_id/name 等标准字段）。用例：带 attachments 的请求，上游收到的 body 不含该键。

### 7.10 native `stop` 空数组
`"stop": []` 会转发成 `{"Values":null}`。修法：`req.Stop != nil` 时无条件重写；`len(Values)==0` → 删除 `stop` 字段。用例：空 Stop → 上游 body 无 `stop`。

### 7.11 native 别名 + 无 tools → 明确 400
现在会落 ChatGPT 兜底（消耗账号、返回莫名 4xx）。修法：`len(Tools)==0` 且槽位 `Handles(model)` → 400 `invalid_request_error`，文案说明该模型仅接受带 tools 的 chat 请求。两表面都加。用例 + `env.template`/`docs` 同步说明。

### 7.12 文档与模板一致性
`env.template`：`PROVIDER_TOOL_MODELS=deepseek-no-search` 必须**注释掉**（默认空）；注释改成「按 `ResolveCanonical` 回填后的 id 匹配（大小写/空白容错后）」。`docs/ARCHITECTURE.md` §5.5 同步 7.2/7.7/7.11 的结论。

### 7.13 补测试缺口
- 流式帧**顺序**（role → tool_calls → finish_reason → `[DONE]` 相邻）与多工具流式 `index` 0,1；
- 7.2 的 strict-fallback 用例；
- 4KB 截断的**多字节**用例（中文/emoji 跨边界 → 合法 UTF-8 且不超 4096 字节 + 后缀）；
- `Responses` + tools + 无 native → A 不接管（走 `p.Responses`）；
- native：`tool_choice` function 对象形态、`stop` 空数组、上游 500（非 429）原样。

## 验收
- `go build ./...`、`go test -count=1 ./internal/handler/ ./internal/provider/ ./internal/config/ ./internal/toolcall/` 全绿。
- `git diff --stat local-toolfix...HEAD` 里 `internal/toolcall` 仍为零改动。
- 新增/修改用例覆盖 7.1–7.13 的每一条（缺一不可）。
- commit 中文，形如 `fix(provider-tool): review 修复——工具轮守卫/去参数推断/熔断/文本保留 + P2 项`。

**Status:** done（2026-10-27，t07-review-fixes；7.1–7.13 全部落实，含回归用例）
**Blocked by:** 06
