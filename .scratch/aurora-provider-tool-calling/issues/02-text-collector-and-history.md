# 02 — Provider 侧接缝：TextCollector + CollectText + 工具历史翻译

**Type:** task

**What to build:** 让 provider 能以「非流式、取回全文」的方式跑一轮，并让 DeepSeek 的请求翻译层认识 `role=tool` / `assistant.tool_calls` 历史。**不改 `internal/toolcall`，不改 handler。**

> 先读共享笔记 `D:\_work\dev\tmp\aurora-tool-calling-notes\notes.md` §1/§2/§3/§4。参考同型实现 `internal/provider/deepseek_coding.go`（已封存，别修改它）。

### 2.1 新增可选接口（`internal/provider/provider.go`）

```go
// TextCollector 由需要接入工具模拟的 provider 实现：以非流式方式取回
// 一轮全文（不含 reasoning），供 handler 侧解析 <tool_call>。
type TextCollector interface {
    CollectText(c *gin.Context, req *official.APIRequest) (text string, err error)
}
```

- `typings/official` **没有** `Usage` 类型（已核实：`usage` 结构未导出）→ 签名就用 `(string, error)`，不要新造 usage 类型。
- 只加接口声明 + 注释，**不改 `Provider` 接口**；加编译期断言 `var _ TextCollector = (*DeepSeek)(nil)`。

### 2.2 DeepSeek 实现 `CollectText`（`internal/provider/deepseek_chat.go`）

- 复用 `chatCompletions` 的非流式分支（`deepseekChatFlow` + `flow.run()` → `deepseekTurnOutput.text`），**不写 gin.Context、把文本返回**。
- 保持 tools 剥离硬规则：不得把 `req.Tools` 传给上游。
- 会话/搜索/识图逻辑与 `chatCompletions` 一致；重复代码抽内部 helper（如 `buildChatFlow`），但不得改变 `chatCompletions` 的对外行为。
- **会话必须每轮新建**：`buildRequest` 续轮只发最后一条 message（`deepseek_chat_session.go:291-308`），注入的 instructions 只在「新会话」路径生效。→ `CollectText` 里 `clientKey` 置空（不进池、一轮一建一删）。单测必须断言「送给上游的 prompt 含注入片段」，不能只测返回值。

### 2.3 工具历史翻译（`deepseekTurnsFromItems`）

现状：`function_call` / `function_call_output` 被整条 `continue` 丢掉内容。改成按 **ChatGPT 通道已验证的格式**翻译（`internal/toolcall/prompt.go:279` 的系统提示依赖 `"Tool (...)"` 块）：

| item | 翻译成 turns 里的文本 |
|---|---|
| `function_call` | `toolcall.SerializeForHistory([]official.ToolCallRef{...})` 的输出（即 `<tool_call>{"name":...,"arguments":...}</tool_call>`） |
| `function_call_output` | `"Tool (Resultado da ferramenta <name>): " + content`；content > 4096 字节时截断并追加 `…（已截断）`；name 解析不到时用 `"tool"` |
| 普通文本 | 原样，行为不变 |

问题：`responsesInputItem`（`input.go:16`）现在只有 `Type/Role/Text/ImageURLs`，`apiMessagesToItems` 丢掉了 `APIMessage.Name` 与 `ToolCallID`。因此本 ticket 内：
- 给 `responsesInputItem` 增 `Name`、`CallID` 字段，并在 `apiMessagesToItems` 填充；
- `function_call_output` 的 name 优先用 item 自带 name，其次用 `CallID` 反查**最近一次** `function_call` 的 name（call_id 不匹配就回落 `"tool"`）；
- `function_call` 的 name/arguments 从 item.Text 里那段 `{"type":"tool_calls","tool_calls":{...}}` JSON 解出；解不出就跳过（不要 panic）。
- 两个表面都汇入 `deepseekTurnsFromItems`，翻译只写一处；**无工具历史的输入输出必须与改动前逐字一致**（L4 回归判据）。

**文件归属：** `internal/provider/provider.go`、`internal/provider/deepseek_chat.go`、`internal/provider/input.go`、`internal/provider/deepseek_chat_session.go`（仅在确需暴露 token 数时）、新增测试文件。

**验收：**
- 表驱动单测（`internal/provider/` 下）：
  1. `function_call` → 精确的 `<tool_call>` 文本；`function_call_output` → 精确的 `Tool (…)` 文本；
  2. 4097 字节结果 → 4096 + 截断后缀；
  3. name 反查：有 name / 只有 call_id 能反查 / 完全没有 → `"tool"`；
  4. 无工具历史的普通文本 → 与改动前一致（写死期望值）；
  5. `CollectText` 走假上游（`httptest`）断言：**送出的 prompt 含注入的 instructions 片段**、不含 `tools` 相关上游参数、返回文本正确。
- `go build ./... && go test ./internal/provider/...` 绿；不触真实网络。
- `git diff --stat` 证明 `internal/toolcall` 零改动。

**Blocked by:** None
