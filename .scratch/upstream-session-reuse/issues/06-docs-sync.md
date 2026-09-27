# 06 — 文档同步

**What to build:** 会话复用行为全部落定后，同步既有文档中已不再成立的断言。

- `docs/ARCHITECTURE.md`：「多轮 = 客户端无状态」一节改写（该表述将不再成立）。
- `docs/DEEPSEEK.md`：多轮说明更新（增量续轮为主、全量拍平降级为新会话引导例外）。
- `CONTEXT.md`：「拍平」词条补注「仅新会话引导路径使用」。

**Blocked by:** 04 — chat completions 表面接入 + `#new#` 信令

**Status:** done

- [x] `docs/ARCHITECTURE.md` 多轮表述改写
- [x] `docs/DEEPSEEK.md` 多轮说明更新
- [x] `CONTEXT.md` 拍平词条补注
- [x] 跑 `bash /mnt/d/_work/dev/scripts/check/doc-consistency-check.wsl.sh` 排查其他文档过时断言

## Comments

### 实现记录 (2026-09-28)

- 同步面比 ticket 列的三个文件更大：对外契约 `API.md` 新增「上游会话复用」章节
  （双头语义、curl 两轮示例、TTL/池上限/降级开关）。spec user story 4/5/6 是对外承诺，
  不加这节等于 API 文档缺失一项已上线行为。
- **历史快照不改写，只加勘误注记**（AGENTS.md §5）：
  - `docs/YUANBAO.md`「与 DeepSeek 网页通道一致」→ 删线 + 注记（该通道已关停，行为是终态快照）。
  - `docs/deepseek网页协议整理.md`「服务端无历史记忆」→ 加勘误块指向 `docs/DEEPSEEK.md` §一·9。
- `docs/ARCHITECTURE.md` §八 接线点表补 `internal/provider/session/` 与
  `deepseek_chat_session.go` 两行（Standards review 提过的「接线点未登记」）。
- 一致性脚本通过；它只查过时关键词/断链，语义引用（如上述两处）靠人工判读，已逐条处理。

### 同期代码修复（Standards/Spec review 产出，随本次提交）

- `webPool` 惰性构造无锁 → `sync.Once` 守卫（并发首访会各建一池，旧池与其清理 goroutine 泄漏）。
- `PoolConfig.CleanupInterval` 注释称「0 = 默认 10min」而实现是「0 = 禁用后台循环」——
  改为实现向注释对齐（新增 `defaultCleanupInterval`）。
- `deepseekSenderReq.ThinkingEnabled/SearchEnabled` 死字段删除（开关由 `deepseekWebSender`
  构造时确定，flow 层不感知）。
- 上游未给 `response_message_id` 时不再 `Release` 入池：无锚点入池会让下轮在同一 session 上
  全量重发历史，既非续轮也非新会话引导。新增 `flow.finish` 统一收尾 + 回归测试
  `TestLoopEmptyAnchorDoesNotPool`（去掉修复即 FAIL）。
