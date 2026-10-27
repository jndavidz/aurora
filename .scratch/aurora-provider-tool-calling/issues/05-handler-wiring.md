# 05 — Handler 接线：B 优先、A 兜底（A 仅 `/v1/chat/completions`）

**Type:** task

**What to build:** 在 `internal/handler/chat_handler.go` 两个入口插入分派，并补路由单测。**本 ticket 独占 `chat_handler.go`**。

> 分派顺序陷阱见共享笔记 `D:\_work\dev\tmp\aurora-tool-calling-notes\notes.md` §8 坑 1：现有 provider 分派在 `:85-99` 直接 return，tools 判断（`:121`）永远到不了。

### 5.1 `Nightmare`（`/v1/chat/completions`）

在现有 provider 分派**之前**插入 B 的尝试、在 provider 分派**之内**插入 A：

```go
// B：原生透传优先（只登记在槽位，不进 providers 列表）
if len(original_request.Tools) > 0 {
    if native := h.providers.NativeTools(); native != nil && native.Handles(original_request.Model) {
        defer h.recordProviderOutcome(native.Name(), c)
        c.Set(provider.ProviderNameCtxKey, native.Name())
        native.ChatCompletions(c, &original_request)
        return
    }
}
// ... 现有 ResolveCanonical 分派保持原样（canonical 回填 + 熔断 defer）...
// A：Provider 工具模拟
if len(original_request.Tools) > 0 && providerToolEmulationEnabled(h.cfg, original_request.Model) {
    if tc, ok := p.(provider.TextCollector); ok {
        h.handleProviderToolCalling(c, &original_request, tc)
        return
    }
}
p.ChatCompletions(c, &original_request)
```

要求：
- A 的模型用 **canonical 回填之后**的值。
- provider 非 `TextCollector` / 开关关 / 不在白名单 → 维持现状 `p.ChatCompletions`，行为与今天逐字一致。
- 熔断与流式失败上报（`recordProviderOutcome`、`ProviderNameCtxKey`）必须覆盖新增分派。

### 5.2 `Responses`（`/v1/responses`）

- **B**：同样在 provider 分派前插入槽位判断（`native.Responses` 本轮固定 400，见 ticket 04），同样挂 `recordProviderOutcome` + ctx key。
- **A**：本轮**不接** `Responses` 表面；带 tools 的 Responses 请求维持现状。加一行注释说明「A 仅 chat 表面，Responses 工具模拟为后续」，文档侧由 ticket 06 写明。

### 5.3 测试（`internal/handler/provider_routing_test.go`）

自写 fake `Provider`（记调用标记，可按需实现 `TextCollector`），注入 `provider.NewRegistry()`，gin 上下文按 `handler_test.go:19-23` 的惯例：

1. 带 tools + 槽位命中 → 调 native，provider 自身 `ChatCompletions` 未被调用；
2. 带 tools + 槽位未命中 + 开关开 + 是 TextCollector → 走模拟（provider `ChatCompletions` 未被调用）；
3. 带 tools + 开关关 → 走 provider `ChatCompletions`（**回归判据**）；
4. 带 tools + 开关开但不在白名单 → 走 provider `ChatCompletions`；
5. 无 tools → 新旧分支都不进（回归判据）；
6. 开关开但 provider 非 TextCollector → 走 `ChatCompletions`，不 panic；
7. `Responses` + tools + 槽位命中 → 走 native.Responses。

**文件归属：** `internal/handler/chat_handler.go`、`internal/handler/provider_routing_test.go`。

**验收：** `go build ./... && go test ./internal/handler/...` 绿；`git diff chat_handler.go` 只有新增分派块与注释，既有 ChatGPT 路径零改动。

**Blocked by:** 03, 04
