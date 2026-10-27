# 05 — Handler 接线：B 优先、A 兜底（仅 `/v1/chat/completions`）

**Type:** task

**What to build:** 在 `internal/handler/chat_handler.go` 两个入口插入分派，并补路由单测。**本 ticket 独占 `chat_handler.go`**。

### 5.1 `Nightmare`（`/v1/chat/completions`，L85 附近的 provider 分派前）

```go
if len(original_request.Tools) > 0 {
    // B：原生透传优先（槽位非空且模型命中映射时）
    if native := h.providers.NativeTools(); native != nil && native.Handles(original_request.Model) {
        defer h.recordProviderOutcome(native.Name(), c)
        c.Set(provider.ProviderNameCtxKey, native.Name())
        native.ChatCompletions(c, &original_request)
        return
    }
}
// ... 现有 ResolveCanonical 分派（原样保留，含 canonical 回填与熔断 defer）
if p != nil {
    if len(original_request.Tools) > 0 && providerToolEmulationEnabled(h.cfg, original_request.Model) {
        if tc, ok := p.(provider.TextCollector); ok {
            h.handleProviderToolCalling(c, &original_request, tc)
            return
        }
    }
    p.ChatCompletions(c, &original_request)
    return
}
```

要求：
- A 的判定用 ticket 03 的 `providerToolEmulationEnabled`，模型用 **canonical 回填之后**的值。
- provider 不是 `TextCollector`（或开关关闭 / 不在白名单）→ 维持现状调 `p.ChatCompletions`，行为与今天逐字一致。
- `recordProviderOutcome` / `ProviderNameCtxKey` 的既有语义（熔断、流式失败上报）必须覆盖新增分派。

### 5.2 `Responses`（`/v1/responses`）

- **B**：同样在 provider 分派前插入槽位判断（`native.Responses` 本轮固定返回 400，见 ticket 04）；要用同样的 `recordProviderOutcome` + ctx key。
- **A**：本轮**不接** `Responses` 表面；带 tools 的 Responses 请求维持现状（provider 内部丢弃工具信息）。在该分支加一行注释说明「A 仅 chat 表面，Responses 工具模拟为后续」，并在 ticket 06 文档里写明。

### 5.3 测试（`internal/handler/provider_routing_test.go`）

用假 provider（实现 `Provider`，可按需实现 `TextCollector`）注入 `provider.NewRegistry()`，`httptest` 建 gin 上下文：

1. 带 tools + 槽位命中 → 调用 native（假实现记调用标记），`finish` 正常；
2. 带 tools + 槽位未命中 + 开关开 + provider 是 TextCollector → 走模拟（断言 provider 自己的 `ChatCompletions` 未被调用）；
3. 带 tools + 开关关 → 走 provider 的 `ChatCompletions`（**回归判据**）；
4. 带 tools + 开关开但模型不在白名单 → 走 provider 的 `ChatCompletions`；
5. 无 tools → 两条表面都不进任何新分支（回归判据）；
6. provider 非 TextCollector + 开关开 → 走 `ChatCompletions`（不 panic，不误降级）；
7. `Responses` + tools + 槽位命中 → 走 native.Responses。

**文件归属：** `internal/handler/chat_handler.go`、`internal/handler/provider_routing_test.go`。

**验收：** `go build ./... && go test ./internal/handler/...` 绿；`git diff` 里 `chat_handler.go` 只新增分派块与注释，未改动既有 ChatGPT 路径。

**Blocked by:** 03, 04
