# Spec: 架构收敛一期——bug 止血 + 流式 wire emitter 地基

Status: closed（2026-10-26：全部 ticket done，两分支已并入主干 local-toolfix 并删除）

来源：2026-10-26 架构审查（深读源码 + 对照 one-api / Bifrost / LiteLLM 业界实践），9 个候选中按「bug 修复 < deepening；改动面小 < 大；被依赖地基 < 上层」排序后的一期范围。所有 bug 断言均已经过二次代码复核实锤，非推演。

## Problem Statement

aurora 的调用方和维护者当前面对三层问题：

**已实锤的行为 bug（用户可感知）**：纯 chat 请求的对话历史在发往 ChatGPT 兜底前被静默改写——coding 域的「拒绝历史清洗」策略泄漏到 chat 变体的 seam 之外，「我会继续分析」这类通用中文短语会被替换成占位符，真人中文对话质量受损；CDP 桥的 Responses 表面对 ChatGPT 系模型（如 gpt-5.6）返回 404 model_not_found——模型能路由进来（Handles 命中）、随后查表未命中；三家网页客户端（GLM/Kimi/千问）的 token 池轮换存在 data race（无锁 cursor 读写），并发请求下有脏读与越界风险，且同型竞态在 DeepSeek 侧已修、其余三家漏修。

**结构性维护痛点（维护者可感知）**：同一套流式 wire 协议（Responses 事件骨架、chat.completion chunk 序列、SSE 头四件套）在 20+ 处手搓、四套平行词表并存；UTF-8 截断修复只落在 handler 一侧、未回灌各 provider 的手搓副本，即是「修一处漏 N 处」的实证。熔断器对流式失败失明：流式失败恒以 200 + failed 事件收场，熔断器读 HTTP status 永远记成功，流式 100% 失败的通道也永不熔断。

**认知税**：每加一家 provider 或每修一个 wire 格式 bug，都要重新学习一遍完整 SSE 协议知识并同步多处——interface 与 implementation 一样宽，depth 为零。

## Solution

两阶段闭环：先止血，再落地基。

**止血（行为修复）**：拒绝历史清洗收回 coding 路径内（工具路径内部已有等价调用，chat 路径恢复「历史原样发上游」不变量）；CDP 桥基类转正为独立基座、`-chat` 内部后缀约定归一到单一 resolve 点（Responses 404 随之修复）；三家 token 池照已验证模式补锁；Kimi 搜索开关迁入 Config 单一事实来源。

**地基（deepening）**：落两个流式 emitter module——`ResponsesStream`（Responses 事件骨架）与 `ChunkStream`（chat.completion chunk 序列），把事件序列、rune 安全切片、SSE 头延迟写出等 wire 知识收进单一 interface 之后的 implementation；全部活 call site（chat 变体 + ChatGPT 兜底）改走 emitter；失败发射点统一后接通熔断器，流式失败不再失明。顺手项：「拍平」函数家族迁回共享输入助手 module，使 GLOSSARY 一级词汇在代码里有家。

coding 变体封存不动。二期候选（DeepSeek 流式降级状态机收口、ChatGPT 兜底 turnLoop 合并）明确排除在本期之外。

## User Stories

1. As a chat 用户（无 tools 纯对话）, I want 我的对话历史原样发往上游, so that 中文日常短语不被静默改写、回复不被防锚定占位符污染。
2. As a Responses API 客户端（pi 等）, I want 经 CDP 桥请求 ChatGPT 系模型不再 404, so that `/v1/responses` 表面与 `/v1/chat/completions` 同样可用。
3. As a 高并发调用方, I want token 池轮换在并发下无 data race, so that 多账号负载均衡不产生脏读或崩溃。
4. As a aurora 维护者, I want 流式 wire 格式的修改只发生在单一 emitter module, so that 修一处即全 provider 生效（locality）。
5. As a aurora 维护者, I want 新 provider 接入时只需提供 delta 回调而不必学习整套 SSE 协议（leverage）, so that 加一家上游的接触面收敛。
6. As a aurora 维护者, I want golden SSE 序列测试在 emitter 的单一 seam 上可写, so that wire 格式回归不再依赖 live 验证真实账号。
7. As a aurora 运维者, I want 熔断器能感知流式失败, so that 已故障通道被真实摘除、`/health` 观测可信。
8. As a 配置管理者, I want Kimi 搜索开关与 GLM/DeepSeek 一样走 Config, so that 配置可枚举、健康端点可见、无双读漂移。
9. As a 新 provider 贡献者, I want 「拍平」等共享概念住在共享输入助手 module, so that 不误判自家实现依赖别家 provider 内部。
10. As a CI, I want 全部修复有非 live 测试覆盖（httptest 假上游 / -race 并发单测）, so that 回归在合并前被拦截且不消耗真实凭证。
11. As a chat 用户, I want 工具调用场景的防锚定清洗照旧生效, so that coding agent 的重试行为不受止血修复影响。
12. As a `/v1/models` 消费端, I want CDP 基类转正后模型目录与能力标注不变, so that 客户端选型不受重构影响。
13. As a 二期开发者（GLM/Grok/Minimax 接入上游会话复用）, I want 本期 emitter 与 404 修复先行落地, so that 二期不在旧地基上叠加新副本。

## Implementation Decisions

分七个 Work Package，前四个为行为修复，后三个为收口重构；每个 WP 独立可验收。

**WP1 拒绝清洗收 seam**：两个表面入口（ChatGPT 兜底路径）对历史的无条件清洗改为仅工具路径生效。工具路径内部的既有调用保持不变，因此 coding 行为零变化；chat 路径恢复不变量。判据与 `toolsEnabled` 判定对齐。

**WP2 CDP 基类转正 + 后缀归一**：
- 基类与五个桥 provider（ChatGPT/Claude/Hunyuan/Doubao/Gemini）的关系从 embed 改为持有字段（组合），消除「Go 无虚方法、基类内部回调子类不可达」的已两次踩坑陷阱。
- `-chat` 内部后缀的补/剥逻辑从各子类手工补丁收进单一归一点（Handles 与模型 resolve 处），两个表面行为一致——Responses 404 是后缀不对称的直接后果，归一即修复。
- 基类更名为中性的桥基座命名，不再以五个 adapter 之一的 provider 名命名。
- `auto` 特判、Models() 去后缀语义、桥池轮询/熔断/唤醒行为全部保持。

**WP3 token 池补锁**：三家网页客户端的 NextToken 照 DeepSeek 侧已验证模式加互斥（含 cursor 与 tokens 的读写保护）；GLM provider 的 token 换发路径补互斥（Kimi 侧已有同型锁）。**不**抽公共 tokenpool module（F1 教训：有具体重复痛点再抽）；refresh 流程本体各家形状不同（JWT claims 副作用、文件回写），不合并不共享。

**WP4 搜索开关入 Config**：`KIMI_WEB_SEARCH` 迁入 Config 字段，语义与默认值（默认关）不变；包级 env 读取移除，消除与 GLM/DeepSeek 的三轨制漂移。

**WP5 流式 wire emitter**：
- 新增 `ResponsesStream`（created → added → delta* → done/completed/failed 事件骨架，含 reasoning item 可选路径）与 `ChunkStream`（role → delta → tool_call delta → stop → [DONE]，含 SSE 头延迟到首增量技巧）两个 emitter。
- 吸收现存四套平行词表（provider 侧 map 版事件构造族、handler 侧 string 版、typed struct、手搓四事件序列）为单一 implementation。
- **只收活代码**：chat 变体与 ChatGPT 兜底；`*_coding.go` 封存文件一律不动。
- 各家差异（reasoning item 有无、output_index 起点、sentinel 元数据、TTFT 头延迟）经构造选项参数化；若参数表膨胀说明切分线画错，止步于「骨架 emitter + 各家填 delta 回调」。
- rune 安全切片（防 UTF-8 跨帧截断）进 emitter 单点，回灌全部 call site。

**WP6 熔断接通流式失败**：失败发射点（failed 事件 / 错误帧）收进 emitter；emitter 与熔断器同包，失败发射处直接上报 provider 名。HTTP status 旁敲保留为非流式路径兜底，流式路径以 emitter 上报为准。

**WP7 拍平归家**：「拍平」函数家族从 DeepSeek 的 chat 变体文件迁入共享输入助手 module；coding 指令家族（围栏 prompt 等）封存不动，仅迁移 chat 变体在用的共享函数。纯移动零逻辑变更。

**全局约束**：WP1–4 为行为修复或等价重构，行为差异即验收对象；WP5–7 纯移动+收口，验收基准是前后 wire 输出字节级一致（时间戳等非确定字段除外）。git 操作一律 WSL 侧执行；每 WP 一次独立 commit，message 引用本 spec。

## Testing Decisions

**好测试的定义**：只测 external behavior——穿过公开 interface（Provider 两方法 / 两表面 handler 入口）对着假上游断言完整 wire 序列；不测 implementation 内部函数、不依赖真实凭证。

**Seam 选择（沿用现有最高层 seam，不新增低层 seam）**：
- 主 seam：wire 回环——真 provider + httptest 假上游 + 公开入口 + golden 序列断言。prior art：DeepSeek 侧两个 wire 测试（chat completions 表面接入会话复用时的回环模式，含 token 文件注入手法）。emitter 改造后每家 provider 至少保留/新增一条 wire 回环防接线回归；emitter 自身的 golden 测试写在其 interface 上（单一 seam 覆盖全部事件序列知识）。
- 404 修复（WP2）：假桥 server 的 wire 回环，断言 Responses 表面对无后缀模型 id 返回正常事件流而非 404；ChatCompletions 表面对照断言不回归。
- 锁修复（WP3）：并发单测——多 goroutine 并发取 token 断言无丢失无越界，`go test -race` 通过；prior art：coding_limit 单测的表驱动风格。
- WP4：Config 加载单测（默认值 + 显式开启两分支）。
- WP1：清洗函数纯单测（marker 命中/不命中/中文短语边界）保留；调用条件的验证以「工具路径既有测试不回归 + chat 路径不再触发清洗」的等价断言表达。ChatGPT 兜底路径当前无 httptest seam，**不在本期补齐**（该 seam 的补齐依赖二期 turnLoop 合并，见 Out of Scope）；live 验证遵守仓库防风控纪律：单条、复用登录态、不复跑。
- WP5 等价性：迁移前后对同一假上游会话各跑一轮 wire 回环，golden 对比（忽略 created_at 等非确定字段）。

**禁止事项**：批量脚本压测、循环连发、并发轰炸一律禁止；非必要不打上游——本地可验证的逻辑不碰真实凭证。

## Out of Scope

- DeepSeek 流式降级状态机三副本收口（`runStreaming`）——依赖本期 emitter 地基，二期候选。
- ChatGPT 兜底 continue 轮询循环三副本合并（`chatgptTurnLoop`）——独立大块，二期候选；兜底路径 httptest seam 的补齐随它走。
- 公共 tokenpool module 抽取（F1 教训：等第二家真痛点）。
- Config 结构化拆分（ProviderConfig 子结构）——加家税大头在 wire format 与入口，config 拆分收益边际。
- Provider 接口去 gin 化（业界已识别的反模式，但改动面大，仅观察）。
- coding 变体任何改动（S2 封存拍板）。
- webclient 工厂统一收口（F1 已降级暂不做）。
- Registry.Resolve 三层匹配、官方 SDK 替换、DI 框架——已拍板不动。

## Further Notes

- 本 spec 的候选全集（9 项）与排序依据见 2026-10-26 架构审查报告（`%TEMP%/architecture-review-20261026-0240.html`，临时文件可能已清理；结论已固化于本 spec 与候选 4/5 的 Out of Scope 记录）。二期启动时应以候选 4（deepseek flow 流式收口）与候选 5（turnLoop 合并）为起点。
- 三个 bug 的复核证据链（2026-10-26 二次实锤，置信度高）：清洗泄漏——两个表面入口在工具判定之后、分支之前无条件调用，且工具路径内部有第二次调用（外层对工具请求冗余）；404——Handles 按带后缀 id 命中后，Responses 委托基类按不带后缀 id 查表未命中，ChatCompletions 则有补后缀补丁（不对称即 bug）；竞态——三家 NextToken 无锁自增对照 DeepSeek 侧带锁实现与自述修复注释。
- 外部对照要点：流式管道四段分层（byte → SSE 行解析 → 规范形 → 出口序列化器）中 aurora 独缺出口序列化器，WP5 即补此段；Bifrost FailureClass 印证「status 不足以分类错误」——WP6 是其最小实现；凭证池被动记账（只按真实请求结果更新健康度）与仓库「非必要不打上游」纪律一致，WP6 不引入主动探活。
- 词汇表对齐：本 spec 使用 GLOSSARY.md 既有术语（表面 / Provider / 兜底 / chat 变体 / 拍平 / token 池 / coding 限频）；「wire emitter / 出口序列化器」为新增概念，落地时如需进 GLOSSARY 由实施 ticket 顺手补。
