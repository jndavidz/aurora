# WP7 拍平迁共享输入助手

Status: done (b77dc0e)
Blocked by: 无

Spec: `../spec.md`（本 ticket 对应其中 WP7 拍平迁共享输入助手 的 Implementation Decisions）

## 验收标准

- flatten 家族迁 input.go，纯移动
- `go build` + `go test ./...` 通过（live 用例缺环境自动跳过）

## Comments

- 2026-10-26 完成:flattenChatInput/flattenChatInputAPI/flattenChatItems 纯迁移至 input.go,零调用点变更。
