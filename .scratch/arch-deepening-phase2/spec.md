# Spec: 架构收敛二期——DeepSeek 流式降级收口 + ChatGPT 兜底 turnLoop 合并

Status: ready-for-agent

来源：2026-10-26 架构审查候选 4/5，一期（`arch-hardening`，2026-10-26 完成）的 Out of Scope 续章。一期已落地的 wire emitter（`ResponsesEmitter`/`ChunkStream`）是本 spec 两项工作的直接地基：收口后的循环只调 emitter 发输出，不再关心 wire 格式。

## Problem Statement

aurora 的维护者面对两组「同一逻辑手抄三份」的维护负担，且其中一组已经抄出了行为漂移：

**DeepSeek 上游会话复用的降级状态机有三份**：「取会话 → 发请求 → 零增量判据 → 丢弃租约换新会话重试一次 → 归还」这套池交互循环，在非流式两表面共用路径与两条流式路径里各写了一遍，「零增量才降级（B 项拍板）」的注释原文抄了三份。每份的降级判据细节靠人肉保持一致；上游会话复用二期接入 GLM/Grok/Minimax 时（ARCHITECTURE §二既定方向），要么再抄两份 90 行的流式函数，要么先把这三份收成一份。修一个降级语义 bug 必须同步三处，wire 测试也被迫逐副本覆盖。

**ChatGPT 兜底的 continue 轮询循环有三份且已漂移**：「发一轮 → 解析 → 累积 → 记录会话状态 → 有 continue 指令就重排再发」的核心状态机在 chat/completions 与 Responses 两表面（流式+非流式）逐字出现三次。漂移已发生：sentinel 保活 ping（session_observer）只在 chat/completions 的副本里发送，两个 Responses 副本不发送——Responses 会话是否因此缺保活无人能答。另外两个表面的入口前奏（解析账号 / 复用客户端状态 / 转换请求 / 缓存记账，约 60 行）也各自复制。这三处是当前全项目唯一没有自动化测试覆盖的请求处理路径：循环逻辑埋在入口函数里，无法用假上游驱动。

## Solution

两块收口，均为「纯移动 + 行为保全」，不改任何对外协议：

**DeepSeek 侧**：给会话复用的 flow 增加流式运行入口（`runStreaming`），把池交互状态机（acquire / 零增量降级 / 换新重试 / finish）收进 flow 一份；非流式与两条流式路径都退化为「调 flow + 用 wire emitter 发输出」的薄壳。降级判据（B 项拍板）成为 flow 内的单点。二期其他 provider 接入会话复用时，flow 可整体复用。

**ChatGPT 侧**：抽出共享的轮询函数（`chatgptTurnLoop`），返回一轮对话的完整累积结果（正文 / 思维链 / 会话 id / continue 链 / sentinel 状态 / TTFT）；三个调用点只保留各自的输出协议（chat completions 的 JSON 或 chunk 流、Responses 的事件流或 JSON）。sentinel ping 参数化——行为保全（现状哪路发哪路不发保持不变），是否全开留作观察项而非本次拍板。两个表面的入口前奏合并为共享助手。循环成为可测函数后，兜底路径获得第一个自动化测试 seam。

## User Stories

1. As a aurora 维护者, I want DeepSeek 会话复用的降级状态机只存在一份, so that 改降级语义只改一处、三路自动一致。
2. As a aurora 维护者, I want 流式与非流式路径共用同一 flow 循环, so that 现有的 fake-pool 降级测试自动覆盖流式路径。
3. As a 二期开发者（GLM/Grok/Minimax 接入会话复用）, I want flow 循环可整体复用, so that 接入新 provider 不再复制 90 行流式函数。
4. As a aurora 维护者, I want ChatGPT 兜底的轮询循环合并为一份, so that continue 语义、重试上限、错误处理一处修改三路生效。
5. As a aurora 维护者, I want sentinel ping 策略显式参数化（而非靠哪份副本说了算）, so that 保活行为可被审查、可被统一决策。
6. As a aurora 维护者, I want 兜底路径可以用假上游做自动化测试, so that 该路径（当前唯一无测试覆盖的请求路径）的回归在合并前被拦截。
7. As a chat/completions 用户, I want 合并后兜底行为与现在完全一致, so that 重构对我不可见。
8. As a /v1/responses 用户（pi 等客户端）, I want 合并后 Responses 兜底行为与现在完全一致, so that 重构对我不可见。
9. As a DeepSeek 会话复用用户, I want 流式收口后「降级重试对客户端不可见」与「干净 502」语义保留, so that 客户端看到的失败形态不变。
10. As a CI, I want 两块收口都有 fake 驱动的测试（假上游 / 假池）, so that 验证不消耗真实凭证、不触发风控。
11. As a aurora 维护者, I want 入口前奏（账号/状态/转换/缓存）共享一份, so that 新增表面级逻辑不用再抄 60 行。
12. As a 观测 aurora 的运维者, I want sentinel ping 的调用路径不变（不新增对上游的请求种类）, so that 收口不增加风控暴露面。
13. As a 未来读代码的贡献者, I want「B 项拍板」这类流级错误取舍的决策只在一处出现, so that 理解会话复用语义不需要比对三份副本。

## Implementation Decisions

分两个 Work Package，相互独立、可并行实施。

**WP1 DeepSeek 流式降级收口**：
- 给会话复用的 flow 增加流式运行入口：`runStreaming(onDelta)`（回调收增量），返回与非流式入口同型的轮次结果。池交互循环（acquire、零增量降级、Discard+AcquireNew 重试一次、finish 归还）收进该入口一份；非流式 `run` 与之共享同一套降级判据实现。
- 降级判据单点化：三份副本现有语义一致（正文与思维链均为零增量才降级重试；已吐增量后上游报错不降级不报错，透明收尾）——「B 项拍板」注释随之收敛为 flow 内一处。
- 两个流式表面函数退化为薄壳：调 `runStreaming` + 用一期 wire emitter 发输出。completions 面的「SSE 头延迟到首增量、未开流失败走干净 JSON 502」语义保留——延迟头状态由薄壳侧的 emitter（deferred 模式）承担，flow 不感知 wire。
- acquire 失败（建会话失败）仍发生在开流之前，由薄壳处理为干净 JSON 502（现状行为）；flow 内的失败在已开流后由薄壳经 emitter 失败出口收尾（现状行为）。
- 不做跨 provider 通用化：flow 仍以 DeepSeek 前缀命名、不引泛型接口；GLM/Grok/Minimax 接入时的抽象提届时再做（F1 教训：有第二家真痛点再抽）。

**WP2 ChatGPT 兜底 turnLoop 合并**：
- 新增共享轮询函数（handler 包）：输入客户端、账号、已转换请求、客户端状态与配置开关；输出一轮对话的完整累积结果——正文、思维链、会话 id、最终 parent id、continue 是否继续、sentinel 状态、TTFT、停止标记。
- continue 重排（清空 messages、Action=continue、换 conversation/parent id）与「会话状态记账 + 注册」收进循环内一份。
- sentinel ping 参数化为布尔开关：chat/completions 路径传 true、两个 Responses 路径传 false——**行为与现状完全一致**（现状即只有 chat/completions 发）；ping 时机（首个会话 id 出现后异步一次）保留现状。是否对 Responses 全开 ping 不在本 spec 拍板，作为观察项见 Further Notes。
- 三个入口只保留输出协议差异：chat/completions 的流式 chunk / 非流式 JSON（含 usage 与 sentinel 元数据）、Responses 的 emitter 事件流 / 非流式 JSON（含 timing 字段）。
- 两表面的入口前奏（账号解析、客户端状态复用、请求转换、缓存记账）合并为共享助手；两表面特有的部分（工具调用提前分支等）留在各自入口。
- 三副本的细微差异（TTFT 记录位置、状态码早退、wsConn 生命周期、ArtifactDelivery 透传）逐差异做行为保全，按一期 G4「纯移动零逻辑变更」先例分步提交。

**全局约束**：每 WP 独立 commit 并引用本 spec；git 一律 WSL 侧；不触碰 coding 封存文件；不新增对上游的请求种类或调用路径（风控纪律）。

## Testing Decisions

**好测试的定义**：只测 external behavior——穿过公开 interface（DeepSeek 两方法 / 兜底两表面入口）对假上游断言完整输出序列与副作用（池状态、会话注册），不测 implementation 内部。

**Seam 选择（尽量复用现有 seam；本 spec 只新增一个）**：
- WP1 seam（零新增）：现有 DeepSeek 双 wire 回环（真 provider + httptest 假上游 + 假 token 文件，覆盖两表面的会话键/信令/续轮）+ session 包 fake-pool 降级测试基建（11 个 TestLoop 形态）。收口后：把降级/重试/零增量用例平移覆盖流式路径（改造现有 wire 测试的断言到流式分支），并保持非流式既有断言不回归——同一批用例两形态跑，即三副本收敛为一份的直接证据。prior art：deepseek 两个 wire 测试、session 包 pool/session 测试。
- WP2 seam（本 spec 唯一新增，最高层）：`chatgptTurnLoop` 的「发一轮上游」抽象为可注入依赖（与现有 conversationClientOrder 同签名的函数变量或窄接口），测试注入假实现返回预置轮次序列，驱动三入口断言：continue 链累积、ping 参数化行为（chat/completions 发、Responses 不发）、输出协议正确、会话注册副作用。这是兜底路径的第一个自动化 seam（一期 spec 声明随二期补齐）。prior art：一期 emitter golden、deepseek fake sender/pool 注入手法。
- 并发安全：收口后跑 -race（Windows 侧无 CGO，走 WSL go）。
- 禁止：批量脚本压测、循环连发；live 验证至多单条（见 Further Notes）。

## Out of Scope

- GLM/Grok/Minimax 实际接入会话复用（本 spec 只收口 DeepSeek 侧；接入时的 flow 泛化届时评估）。
- sentinel ping 是否对 Responses 路径全开（观察项，需 live 实测保活收益 vs 风控暴露，另行拍板）。
- chatgpt 包级 env 双读清理（一期遗留 Speculative 项）。
- Config 结构化拆分、Provider 接口去 gin 化、tokenpool 公共池抽取（维持一期拍板不做）。
- coding 变体任何改动（封存）。
- 会话复用降级语义本身的变更（本 spec 只收口实现，不改语义——B 项拍板、token 绑定、TTL 等维持现状）。

## Further Notes

- 依赖关系：两项都建立在一期 wire emitter 之上（流式出口已是 emitter 调用）；WP1/WP2 相互独立，无 blocking，可并行或任一先行。
- 实施顺序建议：WP1 先（改动面集中在 provider 包一家、有现成测试基建、风险低），WP2 后（三副本差异多、行为保全工作量大）。
- 观察项（WP2 完成后择机）：用一次 live 单条对比确认 Responses 路径是否需要 ping——若上游 session 因缺 ping 被提前回收，则全开并记入决策；否则维持参数化现状。此验证遵守防风控纪律（单发、复用登录态、不复跑）。
- 验收基线：Windows 侧全仓 build + `go test ./...` 绿；WSL 侧 -race 关键包绿；WP1 的 wire 断言在流式/非流式两形态下等价；WP2 三入口行为与合并前的输出逐字节一致（时间戳等非确定字段除外）。
