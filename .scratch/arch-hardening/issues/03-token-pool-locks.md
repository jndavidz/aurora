# WP3 token 池补锁

Status: ready-for-agent
Blocked by: 无

Spec: `../spec.md`（本 ticket 对应其中 WP3 token 池补锁 的 Implementation Decisions）

## 验收标准

- 三家 NextToken + GLM 换发加锁，-race 通过
- `go build` + `go test ./...` 通过（live 用例缺环境自动跳过）

## Comments
