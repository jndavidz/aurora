# WP1 DeepSeek 流式降级收口（runStreaming）

Status: done (88ef55d)
Blocked by: 无

Spec: `../spec.md` WP1。三份池交互循环（run / chatStreamTurn / chatStreamCompletion）收敛为 flow 单份 runCore；两流式表面退化为薄壳。B 项拍板注释三合一。

## 验收标准

- 既有 11 个 fake-pool TestLoop 与双 wire 回环全绿（行为保全）
- 新增流式降级用例（零增量重试/已吐增量不降级）经 onDelta 断言
- go build + go test ./... 绿

## Comments

- 2026-10-26 完成:acquireLease(建会话失败=干净502前置)+runStreaming(l,onDelta) 吸收三份池交互循环;B 项拍板注释三合一;两流式薄壳化(emitter eager/deferred 头语义保留)。既有 11 TestLoop+双 wire 回环全绿=行为保全;新增 scriptedChatSender 流式用例 3 个(零增量重试/已吐增量不降级/双失败)。
