# 04 — 方案 B：`openai-native` 原生透传 provider

**Type:** task

**What to build:** 新建 `internal/provider/openai_native.go` + 单测；在 `internal/provider/provider.go` 加一个「原生 provider 槽位」；在 `internal/handler/router.go` 按配置注册。**不改 `chat_handler.go`**（接线在 ticket 05）。

### 4.1 配置解析与暴露的模型

- `NATIVE_TOOLS_MODEL_MAP` 每项 `"<客户端 model id>=<上游 model id>"`；空白项忽略；任一项缺 `=` 或任一侧为空 → **整个 provider 不注册**并 `log.Printf` 一条明确警告（fail-closed，不做部分注册）。
- `Models()` 返回左值（客户端 id）列表，`Caps` 至少含 `CapFunctionCall`。
- `Handles(model)` 精确匹配左值。
- `Name()` 返回 `"openai-native"`。

### 4.2 Registry 槽位（`internal/provider/provider.go`）

```go
// SetNativeTools 登记「原生工具透传」provider，供 handler 在带 tools 的
// 请求上优先分派（B > A）。未配置时为 nil。
func (r *Registry) SetNativeTools(p Provider)
func (r *Registry) NativeTools() Provider
```
- 槽位与 `providers` 列表**互相独立**：provider 照常 `Register`（参与 `/v1/models`），槽位只用于优先级判断。
- 不改 `Provider` 接口、不改 `Resolve` 的既有行为。

### 4.3 转发实现（`ChatCompletions`）

- `POST <NATIVE_TOOLS_BASE>/chat/completions`，`Authorization: Bearer <NATIVE_TOOLS_KEY>`，`Content-Type: application/json`。
- 请求体：`model` 换成映射后的上游 id；`messages` / `tools` / `tool_choice` / `stream` **原样转发**（用 `json.RawMessage` 或原结构体均可，但不得丢字段——至少不能丢 `tools`、`tool_choice`、`tool` 角色消息）；不透传 aurora 自有字段（如 `artifact_delivery`）。
- 响应：**原样回吐**。非流式 `io.Copy` 上游 body + 上游 `Content-Type`；流式逐帧透传（不解析 `delta.tool_calls`，直接 pipe）。
- 上游 4xx/5xx：保留上游状态码与响应体（bridge 只认 `error` 字段，原样最安全）。网络错误 → 502 + 错误文本。
- 超时：`http.Client` 60s；不重试（透传层不做隐式重试）。
- `Responses`（本轮范围）：返回 400 `invalid_request_error`，错误文本明确「openai-native 仅支持 /v1/chat/completions（本轮）」。**不要**伪造 Responses 事件。

### 4.4 注册（`internal/handler/router.go`）

```go
if cfg.NativeToolsBase != "" && cfg.NativeToolsKey != "" {
    if native := provider.NewNativeTools(cfg); native != nil {
        registry.Register(native)
        registry.SetNativeTools(native)
    }
}
```
- `NewNativeTools` 在映射非法/为空时返回 nil（已打日志）。
- 注册位置放在 provider 列表末尾即可（`/v1/models` 顺序不参与优先级，优先级靠槽位）。

### 4.5 测试（`internal/provider/openai_native_test.go`）

`httptest.NewServer` 假上游：

1. 非流式：断言收到请求的 `model == 映射值`、`tools` 与 `tool_choice` 原样到达、响应 body 逐字节透传（含 `tool_calls`）；
2. 流式：假上游返回 3 帧 SSE，客户端收到**同样的 3 帧**；
3. 上游 429 + 错误体 → 状态码与 body 原样；
4. 映射解析：`a=b, ,bad, c=` → 返回 nil（不注册）；`a=b,c=d` → `Models()` 为 `[a c]`、`Handles("a")` 真、`Handles("b")` 假；
5. `Responses` 返回 400 且 body 说明「仅支持 chat/completions」；
6. 编译期断言 `var _ Provider = (*NativeTools)(nil)`。

**文件归属：** `internal/provider/openai_native.go`、`internal/provider/openai_native_test.go`、`internal/provider/provider.go`（仅加槽位）、`internal/handler/router.go`（仅注册块）。

**验收：** `go build ./... && go test ./internal/provider/...` 绿；无真实网络调用。

**Blocked by:** 01
