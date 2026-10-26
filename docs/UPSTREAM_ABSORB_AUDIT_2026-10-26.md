# Aurora 上游吸收复核（2026-10-26）

> 基线：`local-toolfix` = `f711de9`（由 `origin/voice-latency-deepseek-flash` fast-forward 并入，21 commits）
> 复核方式：逐提交读 diff + 现测 build/test；结论区分**代码实证**与**推断**。
> 相关：[ARCHITECTURE.md](ARCHITECTURE.md)（接线点与历史教训）、[PROJECT_STATUS.md](PROJECT_STATUS.md)（状态总览，其「31 包测试全绿」基线已过时，见 §三 P1）

## 一、这次并入的是什么

分支关系（现测，非记忆）：`origin/voice-latency-deepseek-flash` 领先原主干 21 commits、原主干对其 0 领先 → 可纯 ff；`origin/main` 领先 `upstream/main` 0/落后 8。

12 个代码提交 + 9 个 docs 提交，净 `+2490 / −330`。上游 `aurora-develop/aurora` 的 8 个提交里 7 个以 cherry-pick 形式吸收，映射如下：

| 提交 | 性质 | 上游对应 | 影响面 |
|---|---|---|---|
| `ef45e4a` | fix(bootstrap) | b73a576 | session_token 换发改走 std client + 轮换后写回文件 |
| `087fd58` | feat(chatgpt) | 8fcdbeb | 思考类模型走 `system_hints=["reason"]`，payload 字段统一 |
| `2243dd7` | feat(chatgpt) | 77cb249 | 2026-08 新 SSE 格式解析 + cite 替换（`sseparser` +266 行） |
| `fb37de8` | fix(chatgpt) | 8b18bd9 | cite 流硬化、消息重置与 `.gitignore` 补规则 |
| `17011ad` | feat(toolcall) | 867322b | 工具 prompt 前置 TOOL CALL TRANSLATOR 角色块 |
| `a14947e` | fix | 无（自有） | chat completion 的 end_turn 与 finish_reason 归一 |
| `aabc72f` | feat(responses) | ff5dd6c | `previous_response_id` 会话续接 + 工具结果命名补齐 |
| `ec3892d` | refactor | 10ca511 | TLS 指纹升级 Chrome 150 联合画像（20 文件） |
| `8021676` | chore(deps) | 15983ce（部分） | tls-client 1.15.1 → 1.16.0 |
| `98688bc` | fix(deepseek) | 无（自有） | `webUserAgent` 对齐 Chrome 150，与吸收后 TLS 同版本 |
| `37e2fd5` | chore(fmt) | — | gofmt 收口 |
| — | docs ×9 | — | 语音降延迟 spec 拆票与三轮真机验收记录 |

## 二、验证结果（现测）

```
go version                      → go1.26.5 windows/amd64
go build ./...                  → 通过（exit 0）
go test ./...                   → 仅 internal/toolcall FAIL，其余全绿
```

`git status -sb` → `## local-toolfix...origin/local-toolfix [ahead 21]`（复核当时未推；现已推送，见 §五 #1）。

## 三、缺陷清单

| 级别 | 问题 | 证据 | 建议 |
|---|---|---|---|
| P1 | `TestGolden` 在任何全新 clone 必红：测试 glob `testdata/*.input.txt`，而输入样本被 `.gitignore:6` 的 `*.txt` 排除、从未入库，只跟踪了 7 个 `.golden` | `git check-ignore -v internal/toolcall/testdata/deepseek-real.input.txt` → `.gitignore:6:*.txt`；`git ls-files internal/toolcall/testdata` 只出 `.golden`；合并前的 `cd45a3b` worktree 复现同样 FAIL（**非本次并入引入**，引入于 `a457925`） | 加 `!internal/toolcall/testdata/*.input.txt` 反向规则并重建样本；`.golden` 只存输出快照，输入**不可反推**，需重写 7 条 |
| P2 | `previous_response_id` 的 `call_id→name` 补齐是死代码：两处注册都传 `nil` callNames，取回恒空 | [chat_handler.go:333](../internal/handler/chat_handler.go#L333)、[:369](../internal/handler/chat_handler.go#L369) 均 `RegisterResponse(..., nil)`；[chat_handler.go:242](../internal/handler/chat_handler.go#L242) 调 `ApplyFunctionCallNames`（[request.go:463](../typings/official/request.go#L463)）；全仓无其它 `RegisterResponse` 调用点 | 会话续接（conversation/parent）那半已生效；要补齐需在工具轮里收集 call_id→name 并传入。上游 `ff5dd6c` 原样如此 |
| P2 | `end_turn` 判定收紧为「JSON bool 且为 true」：上游若发 `"true"`/`1`，`isEnd` 不再触发、收尾丢失 | [handler_response.go:538](../internal/chatgpt/handler_response.go#L538) `if endTurn, ok := ...EndTurn.(bool); ok && endTurn`（旧为 `!= nil`）；测试仅覆盖 true/false 字面量 | 真机日志里确认字段实际类型再定；推断项，非实测 |
| P3 | 字符串事件条件放宽：`{"v":"…","o":…}` 无 `p` 时也当正文追加，有重复计入风险 | [handler_response.go:80](../internal/chatgpt/handler_response.go#L80)（旧要求 `p==nil && o==nil`）——代码实证；是否真会出现该形态属推断 | 观察上游实帧后再收 |
| P3 | `previous_response_id` 无归属校验：任何调用方引用他人 responseID 即可续接其 ChatGPT 会话 | [session_manager.go:87](../internal/handler/session_manager.go#L87) `GetResponse` 只按 ID 查，不带 apikey/account 维度 | 本地单用户无影响；若经 bridge 暴露多用户面必须加隔离 |
| P3 | 凭据文件写入的两处边角 | [bootstrap.go:301](../internal/bootstrap/bootstrap.go#L301)、[:320](../internal/bootstrap/bootstrap.go#L320) rename 失败兜底用 `0644`（比临时文件 0600 宽）；Windows 分支 remove→rename 之间存在理论空窗 | 可把兜底权限改为沿用原文件 mode |
| P3 | `responseSessions` 无容量上限，仅靠 30 分钟 TTL 回收 | [session_manager.go](../internal/handler/session_manager.go) `cleanupLoop` | 高流量下关注内存 |

`git status` 复核期间无本地脏改；`chunk.Choices[0]`（[handler_response.go:606](../internal/chatgpt/handler_response.go#L606)）无长度守卫，但该串只出自 `NewChatCompletionChunk`（恒 1 个 choice），判为低风险、不列整改。

## 四、结构性知识（值得留存的三条）

1. **OpenAI 会话令牌是一次性轮换**：用指纹 TLS client 请求 `/api/auth/session`，即使响应被 Cloudflare 拦截解析不出，令牌也已在服务端被消费，任何重试都只会拿到匿名空会话。故 `exchangeSessionToken` 必须全程 std client，且轮换出的新令牌要立刻写回 `session_tokens.txt`——否则重启加载与 10 分钟健康续期都拿已消费的旧令牌。全仓仅此一处写该文件，`sessionTokenFileMu` 够用。
2. **新版 SSE 的 cite 标记必须 hold-back**：标记被切在两帧里、`alt` 链接晚于正文 1~2 帧。逐帧直接替换会（a）把半截标记当私有区乱码透传，（b）把「完整但 alt 未到」的标记误删、链接永久丢失。`CiteStreamPipeline` 因此暂存未解析尾部，`MaxCiteHoldBytes = 4096`（[parser.go:673](../internal/sseparser/parser.go#L673)）兜底强制放行，宁丢链接不扣正文。
3. **TLS 指纹与 UA 必须同版本**：吸收上游 Chrome 150 联合画像后，`webUserAgent` 若仍停在旧版本就是自证不一致，`98688bc` 专门收这个口。

## 五、待办（均需单独授权）

| # | 事项 | 状态 |
|---|---|---|
| 1 | `git push origin local-toolfix` | **已完成**（2026-10-26，`cd45a3b..34f0574`） |
| 2 | 删已并入的 `voice-latency-deepseek-flash`（本地 + 远端） | 待授权；分支纪律要求并入即删（[AGENTS.md:14](../AGENTS.md#L14)） |
| 3 | `main` ff 到 `upstream/main`（落后 8、自有 0），再 `git merge main` 回主干 | 待授权 |
| 4 | P1 golden 样本重建 | 未开始 |
| 5 | P2 `RegisterResponse` callNames 补齐 | 未开始 |
| 6 | `PROJECT_STATUS.md` 的「31 包测试全绿」基线（2026-09-05）已过时 | 待更新 |
