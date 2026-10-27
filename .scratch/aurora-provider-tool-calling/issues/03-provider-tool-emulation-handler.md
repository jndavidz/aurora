# 03 — 方案 A 核心：`handleProviderToolCalling`（注入 → 收集 → 解析 → 封装 → 重试降级）

**Type:** task

**What to build:** 新建 `internal/handler/provider_tools.go` + `internal/handler/provider_tools_test.go`。这是方案 A 的全部逻辑，**不接管线路由**（接线是 ticket 05）。

> 先读共享笔记 `D:\_work\dev\tmp\aurora-tool-calling-notes\notes.md` §1/§2/§5/§6。
> 参考同型实现：`internal/provider/deepseek_coding.go`（provider 侧模拟，已封存，**不要修改**）；SSE 写法参考 `internal/handler/tool_calling.go:406-460`。

### 3.1 门禁 helper

```go
// providerToolEmulationEnabled 报告本次请求是否走 provider 工具模拟。
func providerToolEmulationEnabled(cfg *config.Config, model string) bool
```
`cfg.ProviderToolEmulation` 为真，且（`len(cfg.ProviderToolModels)==0` 或 model 精确命中白名单，空白项忽略）。

### 3.2 主流程

```go
func (h *ChatHandler) handleProviderToolCalling(c *gin.Context, req *official.APIRequest, tc provider.TextCollector)
```

1. **指令注入**：`toolcall.BuildInstructions(req.Tools, req.ToolChoice)`，按 ChatGPT 通道的写法包进 `<tool_calling_protocol>…</tool_calling_protocol>`，**并入单条 system 消息**（有 system 就拼接，没有就加在最前），不要产生多条 system。操作的是请求**副本**，不得修改调用方 `req`。
2. **收集**：`text, err := tc.CollectText(c, injected)`。上游错误 → `apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")`（与 `deepseek.chatCompletions` 错误出口同型）。
3. **解析**：`toolcall.NewParserWithTagsAndTools(toolcall.DefaultTags, req.Tools)` → `Feed(text)`；无 calls 再 `Flush()`；仍无 → `toolcall.RecoverFromText(text, tools)` 兜底（与 ChatGPT 通道同序）。命中后 `calls[i].Index = i` 重排。
4. **重试**：`ProviderToolRetries` = **额外重试次数**（0 = 只跑一次；负数配置层已回落默认 3）。未命中时若还有额度：把 nudge 文本作为一条 user 消息追加进下一轮请求再收集。nudge 用固定文本（可含可用工具名列表），**不要**直接套 `FinalNudge` 的 coding 语义（它按 role 分流且写死标签，chat 场景不适用）。重试耗尽 → **降级为普通文本**，绝不 5xx。

### 3.3 响应封装（复用现有构造器，别手搓）

| 场景 | 输出 |
|---|---|
| 非流式 + 有 calls | `official.NewChatCompletionWithToolCalls("", "", calls, in, out, req.Model, "", nil)`（该构造器自动 `finish_reason="tool_calls"`） |
| 非流式 + 无 calls | `official.NewChatCompletionWithMetadata(text, in, out, req.Model, "", nil)` |
| 流式 + 有 calls | `s := provider.NewDeferredChunkStream(c, req.Model)` → `s.Started()` 判定 → `s.ToolCallDeltas(toolcall.StreamToToolCallDeltas(calls)...)` → `s.StopToolCall()` → `s.Done()` |
| 流式 + 无 calls | 同 Stream：`s.TextDelta(text)` → `s.Stop("stop")` |

- `model` 回显客户端请求原值。
- tokens：`util.CountToken` 估算（input 用 `countMessagesTokens` 或 provider 包同款 helper，按可复用者取）。
- 上游错误且流未开始：走 `apierrors.JSONError` 502（`DeferredChunkStream` 未 Started 时安全）；流已开始后出错按 `writeCompletionError` 同型（错误帧 + `[DONE]`）——若该 helper 不可复用则在本地实现等价逻辑。

### 3.4 测试（`provider_tools_test.go`）

假 `TextCollector`（canned 文本，按调用次数返回不同内容）驱动，全离线：

1. 文本含 `<tool_call>` → JSON：`choices[0].message.tool_calls[0].function.name`、`finish_reason=="tool_calls"`、`arguments` 可 `json.Unmarshal`；
2. 纯文本无调用 → 原样文本 + `finish_reason=="stop"`；
3. 第一次无调用、第二次有 → 重试生效，且假收集器**第二轮收到的 prompt 含 nudge 文本**；
4. `ProviderToolRetries=0` → 不重试（假收集器只被调用 1 次）；
5. 重试耗尽 → 200 + 文本；
6. 流式两条路径的帧序列：用 `sseDataLines()`（`handler_test.go:154`）断言，末帧 `[DONE]`；
7. 多工具返回 → `index` 为 0,1,…；
8. 门禁 helper：开关关 / 不在白名单 / 白名单命中（只测 helper，路由在 ticket 05）；
9. 注入不污染入参：调用后原 `req.Messages` 长度与内容不变；
10. `CollectText` 返回错误 → 502 且 body 含错误文本。

**文件归属：** `internal/handler/provider_tools.go`、`internal/handler/provider_tools_test.go`。**不要改 `chat_handler.go`**（ticket 05 独占）。

**验收：** `go build ./... && go test ./internal/handler/... ./internal/toolcall/...` 绿；`git diff --stat` 显示 `internal/toolcall` 零改动。

**Blocked by:** 01, 02
