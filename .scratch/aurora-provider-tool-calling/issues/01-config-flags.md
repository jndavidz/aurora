# 01 — 配置开关：PROVIDER_TOOL_* 与 NATIVE_TOOLS_*

**Type:** task

**What to build:** 在 `internal/config/config.go` 里加两组开关（结构体字段 + `Load()` 读取），供方案 A（工具模拟）与方案 B（原生透传）使用。**只改这一个文件**，不要碰 handler/provider。

```go
// 方案 A：provider 工具模拟（文本协议注入 + <tool_call> 解析）
ProviderToolEmulation bool     // PROVIDER_TOOL_EMULATION，默认 false
ProviderToolModels    []string // PROVIDER_TOOL_MODELS，逗号分隔白名单；空 = 允许所有 provider 模型
ProviderToolRetries   int      // PROVIDER_TOOL_RETRIES，默认 3（沿用 REFUSAL_RETRIES 语义：0 允许，表示不重试）

// 方案 B：原生工具透传（OpenAI 兼容上游）
NativeToolsBase     string   // NATIVE_TOOLS_BASE，如 https://api.deepseek.com/v1
NativeToolsKey      string   // NATIVE_TOOLS_KEY
NativeToolsModelMap []string // NATIVE_TOOLS_MODEL_MAP，逗号分隔，每项 "<客户端 model id>=<上游 model id>"
```

要点：
- 布尔/整数读取复用现有 `getBoolEnv` / `getIntEnv`，CSV 复用 `splitCSV`；不要新写 helper。
- 字段注释按现有风格写「为什么 + 默认值 + 开关语义」，中文，别写空话。
- `NativeToolsModelMap` 保留原始 `a=b` 字符串，解析留给 provider 侧（配置层不做跨包语义）。
- 不要给这两组开关注入任何默认可用状态；默认值必须是「全部关闭 / 空」。

**文件归属：** `internal/config/config.go`（本 ticket 独占）。

**验收：**
- `go build ./...` 通过。
- 新增 `internal/config/config_test.go`（若无则新建）表驱动覆盖：默认值、`PROVIDER_TOOL_RETRIES=0`、`=abc` 回落默认、CSV 空白裁剪。环境变量用 `t.Setenv`。
- `go test ./internal/config/...` 绿。

**Blocked by:** None
