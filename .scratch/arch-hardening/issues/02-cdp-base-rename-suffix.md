# WP2 CDP 基类转正 + 后缀归一

Status: done (4822741)
Blocked by: 无

Spec: `../spec.md`（本 ticket 对应其中 WP2 CDP 基类转正 + 后缀归一 的 Implementation Decisions）

## 验收标准

- /v1/responses + gpt-5.6 不再 404；5 桥 provider 组合化
- `go build` + `go test ./...` 通过（live 用例缺环境自动跳过）

## Comments

- 2026-10-26 完成:cdpBase 转正(5 桥 embed→组合);-chat 归一收进 internalID 单点;Responses 404 修复由假桥 wire 测试 TestChatgptCDPResponsesNoSuffix404 验证,ChatCompletions 发桥内部名断言不回归。
