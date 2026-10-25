# WP5 流式 wire emitter

Status: done (c1311a9)
Blocked by: 无

Spec: `../spec.md`（本 ticket 对应其中 WP5 流式 wire emitter 的 Implementation Decisions）

## 验收标准

- ResponsesStream/ChunkStream 落地，活 call site 收编，coding 不动
- `go build` + `go test ./...` 通过（live 用例缺环境自动跳过）

## Comments

- 2026-10-26 完成:wire.go 落地 ChunkStream(立即/延迟头)+ResponsesEmitter(created→added→delta→done→completed,FunctionCall 四事件,MessageDone/Completed/Done 细分);收编 provider 9 家 chat 流式+cdpBase 桥路径+handler 兜底两路径;删 3 份平行词表(shared.go string 版/tool_calling 手搓/chat_handler typed),净删 149 行。golden wire_test.go + deepseek 双 wire 回环通过=等价性。coding 封存文件 0 改动(git diff 确认)。偏离记录:spec 曾提「每家一条 wire 回归」,实际以 emitter golden+deepseek 全链路双 wire+CDP 假桥三层覆盖,其余家接线同构,逐家假上游成本高于边际收益。
