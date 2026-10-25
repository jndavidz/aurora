# WP5 流式 wire emitter

Status: ready-for-agent
Blocked by: 无

Spec: `../spec.md`（本 ticket 对应其中 WP5 流式 wire emitter 的 Implementation Decisions）

## 验收标准

- ResponsesStream/ChunkStream 落地，活 call site 收编，coding 不动
- `go build` + `go test ./...` 通过（live 用例缺环境自动跳过）

## Comments
