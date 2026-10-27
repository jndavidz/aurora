# 04 — 方案 B：`openai-native` 原生透传 provider

**Type:** task

**What to build:** 新建 `internal/provider/openai_native.go` + 单测；`internal/provider/provider.go` 加「原生 provider 槽位」；`internal/handler/router.go` 按配置登记槽位。**不改 `chat_handler.go`**（接线在 ticket 05）。

> 先读共享笔记 `D:\_work\dev\tmp\aurora-tool-calling-notes\notes.md` §1/§7/§8。

### 4.1 映射解析与模型目录

- `NATIVE_TOOLS_MODEL_MAP` 每项 `"<客户端 model id>=<上游 model id>"`；空白项忽略。
- 非法（缺 `=`、任一侧为空、左值重复）→ **整个 provider 不注册** + `log.Printf` 一条明确警告（fail-closed，不做部分注册）。
- `Models()` 返回左值列表，`Caps` 含 `CapFunctionCall`；`Handles(model)` 精确匹配左值；`Name()` = `"openai-native"`。

### 4.2 Registry 槽位（`internal/provider/provider.go`）

```go
// SetNativeTools 登记「原生工具透传」provider，供 handler 在带 tools 的
// 请求上优先分派（B > A）。未配置时为 nil。
func (r *Registry) SetNativeTools(p Provider)
func (r *Registry) NativeTools() Provider
```

- 槽位与 `providers` 列表**互相独立**。
- **绝对不要 `Register(native)`**：`Register` 是全量路由，会把同名模型的普通对话、`/v1/responses`、熔断与 `/v1/models` 全部劫持过去（笔记 §8 坑 5）。代价是 native 的模型 id 不进 `/v1/models`——预期行为，写进文档（ticket 06）。
- 不改 `Provider` 接口与 `Resolve` 既有行为。

### 4.3 转发实现（`ChatCompletions`）

- `POST <NATIVE_TOOLS_BASE>/chat/completions`，`Authorization: Bearer <NATIVE_TOOLS_KEY>`，`Content-Type: application/json`。
- 请求体：`model` 换成映射后的上游 id；`messages`（含 `role=tool` / `assistant.tool_calls`）/`tools`/`tool_choice`/`stream` **原样转发**；**不要**透传 aurora 自有字段（`artifact_delivery`、`stream_options` 等可丢）。
- 响应：原样回吐。非流式 `io.Copy` body + 上游 `Content-Type`/状态码；流式逐帧 pipe（不解析 `delta.tool_calls`）。
- 上游 4xx/5xx 保留状态码与 body；网络错误 → 502 + 错误文本。
- `http.Client` 60s 超时；不重试。
- `Responses`（本轮范围）：400 `invalid_request_error`，文本明确「openai-native 仅支持 /v1/chat/completions（本轮）」。不要伪造 Responses 事件。

### 4.4 注册（`internal/handler/router.go`）

```go
if cfg.NativeToolsBase != "" && cfg.NativeToolsKey != "" {
    if native := provider.NewNativeTools(cfg); native != nil {
        registry.SetNativeTools(native)   // 只登记槽位，不 Register
    }
}
```

### 4.5 测试（`internal/provider/openai_native_test.go`）

`httptest.NewServer` 假上游：

1. 非流式：`model` 为映射值、`tools`/`tool_choice`/`messages`（含 tool 角色）原样到达；响应 body 逐字节透传（含 `tool_calls`）；
2. 流式：假上游 3 帧 SSE → 客户端收到**同样 3 帧**；
3. 上游 429 + 错误体 → 状态码与 body 原样；
4. 映射解析：`a=b, ,bad, c=` → nil；`a=b,c=d` → `Models()==[a,c]`、`Handles("a")` 真、`Handles("b")` 假、重复左值 `a=b,a=c` → nil；
5. `Responses` 返回 400；
6. 编译期断言 `var _ Provider = (*NativeTools)(nil)`。

**文件归属：** `internal/provider/openai_native.go`、`internal/provider/openai_native_test.go`、`internal/provider/provider.go`（仅槽位）、`internal/handler/router.go`（仅登记块）。

**验收：** `go build ./... && go test ./internal/provider/... ./internal/handler/...` 绿；无真实网络。

**Status:** done（`04349e3` → 并入 `fd3ef4f`；T07 修订见 `07` 票）

**Blocked by:** 01

## Comments
- 2026-10-27 实现：只登记槽位、**不 `Register`**（避免劫持全量路由）；映射 fail-closed；handler 已消费原始 body，故按 `official.APIRequest` 重序列化转发。
- review 后变更：流式补 `sseHeaders`、消息白名单化（剥 `attachments`）、空 `stop` 删字段、槽位命中尊重熔断。
