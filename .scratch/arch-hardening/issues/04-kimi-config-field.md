# WP4 KIMI_WEB_SEARCH 入 Config

Status: done (8dcda74)
Blocked by: 无

Spec: `../spec.md`（本 ticket 对应其中 WP4 KIMI_WEB_SEARCH 入 Config 的 Implementation Decisions）

## 验收标准

- 包级 os.Getenv 移除，默认值不变
- `go build` + `go test ./...` 通过（live 用例缺环境自动跳过）

## Comments

- 2026-10-26 完成:KimiWebSearch 入 Config(getBoolEnv 默认 false,与旧 =="1" 默认一致),包级 os.Getenv 移除,env.template 补条目。
