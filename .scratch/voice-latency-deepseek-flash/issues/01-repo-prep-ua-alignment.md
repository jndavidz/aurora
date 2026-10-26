# 01 — 仓库侧准备：UA 对齐 + 构建/测试门禁（已完成）

**What to build:** 把吸收后遗留的 TLS/UA 版本不一致收口，并确认镜像构建的代码基线可构建、测试全绿。

- `internal/deepseekweb/client.go` 的 `webUserAgent`：`Chrome/144` → `Chrome/150`（与共享 TLS profile `Chrome_150` 同版本）。
- spec 复审结论落地：三段延迟预算、首句约束、TTS/工具两处生产事实。

**验收（已完成）：**
- `go build ./...` 通过；`go test ./...` 37 包全绿（16GB 机器需 `-p 1`）。
- `go test ./internal/deepseekweb/... ./internal/provider/...` 绿。
- 提交：`98688bc`（UA）、`0f90701`/`5166f7e`（spec 复审）。

**Status:** done

**Blocked by:** None
