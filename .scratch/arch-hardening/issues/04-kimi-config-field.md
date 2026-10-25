# WP4 KIMI_WEB_SEARCH 入 Config

Status: ready-for-agent
Blocked by: 无

Spec: `../spec.md`（本 ticket 对应其中 WP4 KIMI_WEB_SEARCH 入 Config 的 Implementation Decisions）

## 验收标准

- 包级 os.Getenv 移除，默认值不变
- `go build` + `go test ./...` 通过（live 用例缺环境自动跳过）

## Comments
