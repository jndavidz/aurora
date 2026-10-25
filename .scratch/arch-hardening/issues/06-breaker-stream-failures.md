# WP6 熔断接通流式失败

Status: done (4fe0b28)
Blocked by: (05 已完成)

Spec: `../spec.md`（本 ticket 对应其中 WP6 熔断接通流式失败 的 Implementation Decisions）

## 验收标准

- 流式失败上报 breaker，不再恒记成功
- `go build` + `go test ./...` 通过（live 用例缺环境自动跳过）

## Comments

- 2026-10-26 完成:ReportStreamFailure(emitter Fail/deepseek 错误帧直调,provider 名经 gin context 注入,ChatGPT 兜底路径空操作);router 注入 RecordFailure;未开流 JSON 502 仍走 status 旁敲。单测 TestReportStreamFailureOnFail。
