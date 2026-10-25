# WP1 拒绝清洗收 seam

Status: done (4ffb16d)
Blocked by: 无

Spec: `../spec.md`（本 ticket 对应其中 WP1 拒绝清洗收 seam 的 Implementation Decisions）

## 验收标准

- chat 变体历史原样发上游；工具路径行为不变
- `go build` + `go test ./...` 通过（live 用例缺环境自动跳过）

## Comments

- 2026-10-26 完成:两表面入口外层清洗改 toolsEnabled 条件;工具路径行为不变(Nightmare 外层+responsesToolCalling 内层双防线保持)。既有纯函数单测 5 用例全绿。
