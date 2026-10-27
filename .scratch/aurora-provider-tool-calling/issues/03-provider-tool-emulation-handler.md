# 03 — 方案 A 核心：`handleProviderToolCalling`（注入 → 收集 → 解析 → 封装 → 重试降级）

**Type:** task

**What to build:** 新建 `internal/handler/provider_tools.go` + `internal/handler/provider_tools_test.go`。这是方案 A 的全部逻辑，**不接管线路由**（接线是 ticket 05）。

### 3.1 门禁 helper（本文件内）

```go
// providerToolEmulationEnabled 报告本次请求是否走 provider 工具模拟。
func providerToolEmulationEnabled(cfg *config.Config, model string) bool
```
判定：`cfg.ProviderToolEmulation` 为真，且（`len(cfg.ProviderToolModels)==0` 或 model 精确命中白名单）。空白项忽略。

### 3.2 主流程

```go
func (h *ChatHandler) handleProviderToolCalling(c *gin.Context, req *official.APIRequest, tc provider.TextCollector)
```

1. **指令注入**：用 `internal/toolcall` 现有 API（`BuildInstructions` 或 `BuildInstructionsWithTags`，按笔记确认签名）把 `req.Tools` + `req.ToolChoice` 渲染成文本。注入方式：**并入已有 system 消息**（没有 system 就在最前面加一条），不要产生多条 system。注入后的请求副本交给 `tc.CollectText`；**不得修改调用方传入的 `req`**。
2. **收集**：`tc.CollectText(c, injected)`。上游错误 → `apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")`（与 `deepseek.chatCompletions` 的错误出口同型，保留错误文本）。
3. **解析**：`toolcall.NewParserWithTagsAndTools(...)` → `Feed(text)` → `Flush()`。解析结果转成 `[]official.ToolCall`（若 official 无对应类型，见 3.4）。
4. **分支**：
   - `len(calls) > 0` → 工具响应（3.3）；
   - `len(calls) == 0` 且还有重试额度（`cfg.ProviderToolRetries`，语义同 `REFUSAL_RETRIES`）→ 下一轮请求追加 `toolcall.FinalNudge(...)`（或等价提示）作为 user 消息再收集；重试前可复用 `looksLike*` 分类器判断是否明显拒绝/答非所问，命中则必须重试，未命中且已有合理文本可提前降级（提前降级不算失败）；
   - 重试耗尽 → **降级为普通文本**（`finish_reason="stop"`），绝不 500。

### 3.3 响应封装

| 场景 | 输出 |
|---|---|
| 非流式 + 有 calls | `chat.completion`：`message.tool_calls[]`（`id` 形如 `call_<随机>`、`type:"function"`、`function.name/arguments`，arguments 为**合法 JSON 字符串**——参数解析失败时回落 `{}` 并在日志记一次）、`finish_reason:"tool_calls"` |
| 非流式 + 无 calls | 普通 `chat.completion`：`message.content` 为收集到的文本，`finish_reason:"stop"` |
| 流式 + 有 calls | SSE：chunk1 `delta.tool_calls[]`（含 index/id/type/function）；chunk2 `delta:{}` + `finish_reason:"tool_calls"`；`data: [DONE]`。**不要求逐字增量** |
| 流式 + 无 calls | SSE：chunk1 `delta.content` = 全文、`finish_reason:"stop"`；`data: [DONE]` |

- `model` 字段回显客户端请求的原值。
- usage：能拿到就填 `usage`（prompt/completion tokens；没有就用 `util.CountToken` 估算），拿不到可省略；不要为此改动 `typings/official` 的既有构造函数。
- SSE 帧格式与现有 `ChunkStream` 保持一致（`data: <json>\n\n`），可直接复用 `NewDeferredChunkStream` 若它支持自定义 delta；不支持就手写帧，但 `data: [DONE]` 收尾必须与现状一致。

### 3.4 结构体落点

- 若 `typings/official` **已有**可表达 tool_calls 的 chat.completion 结构（含 `finish_reason`），直接复用。
- 若没有：**在 `internal/handler/provider_tools.go` 内**定义局部响应结构（json tag 按 OpenAI 规范），不要改 `NewChatCompletion*` 系列（ChatGPT 通道在用）。若选择在 `typings/official` 新增构造器，必须补 JSON 形状单测。

### 3.5 测试（`provider_tools_test.go`）

用一个假 `TextCollector`（canned 文本，按调用次数返回不同内容）驱动，全部离线：

1. 文本含 `<tool_call>` → 非流式 JSON：`choices[0].message.tool_calls[0].function.name`、`finish_reason=="tool_calls"`、arguments 可 `json.Unmarshal`；
2. 纯文本无调用 → 原样文本 + `finish_reason=="stop"`；
3. 第一次无调用、第二次有 → 命中重试，最终返回 calls，且假收集器**收到**了 nudge（断言第二轮 prompt 含提示）；
4. `ProviderToolRetries=0` 时无调用直接降级；
5. 重试耗尽 → 200 + 文本，不是 5xx；
6. 流式（调用方 `req.Stream=true`）→ 帧序列与 §3.3 一致（逐帧断言 `data:` 内容与 `[DONE]` 结尾）；
7. 门禁：`ProviderToolEmulation=false` / 模型不在白名单 → helper 返回 false（只测 helper，路由在 ticket 05 测）；
8. 注入不污染入参：调用后原 `req.Messages` 长度/内容不变。

**文件归属：** `internal/handler/provider_tools.go`、`internal/handler/provider_tools_test.go`。**不要改 `chat_handler.go`**（ticket 05 独占）。

**验收：** `go build ./... && go test ./internal/handler/... ./internal/toolcall/...` 绿；`internal/toolcall` 零改动（`git diff --stat` 证明）。

**Blocked by:** 01, 02
