# 02 — Provider 侧接缝：TextCollector + CollectText + 工具历史翻译

**Type:** task

**What to build:** 让 provider 能以「非流式、取回全文」的方式跑一轮，并让 provider 的请求翻译层认识 `role=tool` / `assistant.tool_calls` 历史。**不改 `internal/toolcall`，不改 handler。**

### 2.1 新增可选接口（`internal/provider/provider.go`）

```go
// TextCollector 由需要接入工具模拟的 provider 实现：以非流式方式取回
// 一轮全文（不含 reasoning），供 handler 侧解析 <tool_call>。
type TextCollector interface {
    CollectText(c *gin.Context, req *official.APIRequest) (text string, usage *official.Usage, err error)
}
```

- 若 `typings/official` 没有 `Usage` 类型，就用 `(text string, err error)` 并在 commit message 里记明理由；**不要**为此新造 usage 结构。
- 接口只加声明 + 注释，不得修改 `Provider` 接口本身（不能强制所有 provider 实现）。

### 2.2 DeepSeek 实现（`internal/provider/deepseek_chat.go`）

- 复用 `chatCompletions` 的非流式分支：`deepseekChatFlow` + `flow.run()` → `deepseekTurnOutput.text`。
- **保持 tools 剥离硬规则**：不得把 `req.Tools` 传给上游；`CollectText` 与 `chatCompletions` 唯一差别是「不写 gin.Context、把结果返回」。
- 会话/token/搜索/识图逻辑与 `chatCompletions` 完全一致（`session.ResolveClientKey`、`client.TokenFor`、`uploadImagesFromMessages`、`searchEnabled`、`thinkingEnabledAPI`）。若出现复制粘贴，抽一个 `buildChatFlow(c, m, req) (*deepseekChatFlow, int, error)` 之类的内部函数供两者共用，但**不要**顺手重构 `chatCompletions` 的对外行为。
- 推荐实现路径：在 `provider.go` 里加编译期断言 `var _ TextCollector = (*DeepSeek)(nil)`。

### 2.3 工具历史翻译（`deepseekTurnsFromItems` 所在处）

现状：`deepseekTurnsFromItems` 对 `function_call` / `function_call_output` 直接 `continue`——**工具结果内容被整条丢弃**，第二轮模型会不知道为什么工具返回了什么。改成：

| 输入 item | 翻译成上游可读文本 |
|---|---|
| `function_call`（assistant 发起） | `一轮助手输出里的文本：调用 <name>(<arguments>)`（保留 arguments 原文，避免模型复述 JSON 时被截断到无法理解） |
| `function_call_output`（tool 结果） | `工具 <name> 的执行结果：<content>`；content 超过 4096 字节时截断并追加 `…（已截断）` |
| 普通文本 | 原样 |

- 两个表面（`/v1/responses` 的 `responsesInputItems`、`/v1/chat/completions` 的 `apiMessagesToItems`）都汇入 `deepseekTurnsFromItems`，所以翻译只实现一次。
- `role=tool` 消息在 chat 表面必须能拿到内容：先确认 `apiMessagesToItems` 是否把 `role=tool` 的 content 保留下来；若丢了，在本 ticket 内修好（这是 spec §3.4 的关键坑）。
- 只对**已有工具调用痕迹**的输入生效；无工具历史的普通对话，输出必须与现状逐字一致（这是 L4 回归判据）。

**文件归属：** `internal/provider/provider.go`、`internal/provider/deepseek_chat.go`、必要时 `internal/provider/` 下的翻译辅助文件与测试。

**验收：**
- 新增表驱动单测（放 `internal/provider/` 下）：
  1. `function_call` / `function_call_output` → 两段文本的**精确**字符串（含截断用例：4097 字节 → 4096 + 后缀）；
  2. 无工具历史的普通文本路径输出与改动前一致（把改动前的期望值写死）；
  3. `DeepSeek` 满足 `TextCollector`（编译期断言即可）。
- `go build ./... && go test ./internal/provider/...` 绿。
- 不触网：单测不得调用真实上游。

**Blocked by:** None
