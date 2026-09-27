# Spec: 上游会话复用（Upstream Session Reuse）

Status: ready-for-agent

## Problem Statement

aurora 当前把客户端的全量对话历史拍平进 prompt、每请求新建上游会话。实测连续对话每轮比官网慢约 2.4 秒（均值 4.8s vs 2.4s），且随轮数增长；更关键的是流量形态偏离真人：真实网页端从不重发全量历史，全是「服务端会话 + 只发本轮增量」，aurora 的全量重发是显著的非真人特征，增加风控暴露面。

同时，主调用方 open-xiaoai-bridge（小爱语音对话）的产品形态是「默认连续对话」：用户期望跨轮记忆、说「换个话题」时开新对话，而当前链路无法表达这两个语义。

## Solution

aurora 对显式会话型上游启用会话复用：连续对话期间同一客户端的每一轮复用已建立的上游 session，只发送本轮新内容；对话停止超过 30 分钟、或客户端发出明确的新会话指令时，才新开 session。

定位澄清（用户拍板）：**aurora 只做网页反代**。aurora 不维护对话记忆、不校验客户端历史一致性、不猜客户端意图——何时新开、历史是否连贯，完全由客户端侧（open-xiaoai-bridge 的记忆文件与暗号机制）决定。跨 TTL 的记忆断档由 bridge 侧记忆文件解决，不在本 spec 范围。

## User Stories

1. As a 小爱语音用户, I want 连续对话时每轮响应明显变快（复用上游 session，省去建会话与全量历史重发）, so that 对话体验接近直接使用厂商网页。
2. As a 小爱语音用户, I want 说「我们换个话题」后模型忘记之前的对话内容, so that 新话题不被旧上下文污染。
3. As a 小爱语音用户, I want 对话停止超过 30 分钟后再开口时自动开启新会话, so that 长时间间隔后的新问题不被当作上一段对话的延续。
4. As an API 客户端（未来接入方）, I want 通过标准 HTTP 头 `X-Session-Key` 标识我的对话流, so that 我的连续对话能享受会话复用而不依赖特定客户端约定。
5. As an API 客户端（未来接入方）, I want 通过 `X-Session-Action: new` 头或 `user` 字段 `#new#` 前缀强制开新会话, so that 程序化地控制话题切换而无需理解各上游协议。
6. As an API 客户端, I want 不带 `X-Session-Key` 头的请求完全不进会话池（每次独立处理）, so that 一次性调用与连续对话互不干扰、行为可预期。
7. As a bridge 开发者, I want aurora 信任我发送的内容并原样转发（含只发最新一条）, so that bridge 侧的记忆管理（记忆文件、20 条窗口、话题切换）是唯一的上下文权威。
8. As a 运维者, I want 会话复用失败（session 被上游回收/风控失效）时自动降级为新开会话重试一次, so that 用户体验不到中断。
9. As a 运维者, I want 会话池有容量上限与惰性过期, so that 长期运行不泄漏内存、不悬挂死 session。
10. As a aurora 维护者, I want 会话策略集中在单一 module 而非散落各 provider, so GLM/Grok/Minimax 等协议同型的上游未来低成本跟进。
11. As a aurora 维护者, I want 拍平逻辑收编为「新会话引导」专用路径而非散在各 provider 的主路径, so that 代码结构反映真实流量形态（增量为主、全量为例外）。

## Implementation Decisions

### 定位与职责边界（用户拍板）

- aurora 是**网页反代**：不维护对话记忆、不校验客户端历史、不猜意图。上下文权威在客户端。
- 跨 TTL 记忆断档：由 open-xiaoai-bridge 的记忆文件解决（bridge 侧独立工作项）。
- 拍平路径**保留但降级**为「新会话引导」：新开会话时把客户端当轮发来的内容作为首条 prompt 发出。不再有「TTL 过期后全量重放历史」的 bootstrap 路径——TTL 过期即接受记忆断档。

### 会话策略（策略表，最终版）

- 命中条件：池内存在该 key 的活 entry（未过 TTL、model/mode 匹配、非 vision）→ 续轮：`parent_message_id = 上轮 response_message_id`，prompt 只含本轮新内容。
- 未命中 / TTL 过期 / 显式作废 / 失败降级 → 新开 session：prompt 为客户端本轮发来的内容（对 bridge 即最新一条；单发请求即其唯一一条）。
- TTL = 30 分钟，惰性过期（每次命中时检查）+ 后台清理循环（参照 handler 层既有 SessionManager 模式）。
- 显式作废信令（双通道）：HTTP 头 `X-Session-Action: new`；chat completions `user` 字段前缀 `#new#`。
- vision 请求不进池（与搜索互斥、`model_type` 特殊，直接走现行独立会话路径）。

### 会话键（clientKey）

- 取值优先级：`X-Session-Key` 请求头 → chat completions `user` 字段（剥除 `#new#` 前缀后）→ 无（不进池）。
- 最终缓存键 = `clientKey + "|" + exposed model id`。
- 前缀一致性校验：**不做**（用户拍板：aurora 只做反代，信任客户端）。

### 架构形态（对应 architecture review Candidate 1）

- 新增 Session Strategy deep module 于 provider 层：小 interface（acquire/release），吸收池化、TTL、信令处理、失效降级全部 implementation。
- Provider 侧通过可选接口（SessionAware 能力）接入：一期仅 DeepSeek 实现；GLM（conversation_id）、Grok（parent_response_id）、Minimax（session_id）协议同型，列为后续跟进项，不在本期。
- 无可用会话通道的上游（qianwen/doubao/mimo/kimi-web）维持现状，不在本期改动。
- token 轮换语义：session 与建立时的 token 绑定；池内 token 热加载或轮换后旧 entry 视为失效（降级新开）。

### 失败降级

- 续轮 completion 失败（上游 4xx/session 失效/风控）：丢弃池内 entry → 新开 session 重试一次 → 仍失败则返回错误。对客户端透明。

### 池容量与安全

- 池上限 16 entries，LRU 淘汰。
- 复用使 aurora 流量形态回归真人网页行为（服务端会话 + 增量），对风控正面。

### open-xiaoai-bridge 侧（独立工作项，此处仅记录接口约定）

- 每请求带 `X-Session-Key: <session_key>`（复用现有 `session_header` 机制，改头名或并存）。
- 暗号方案 B（用户拍板）：整句精确「我们换个话题」+ 模糊词表（≤8 字、编辑距离 ≤2，如「换个话题」「聊点别的」）；触发后 TTS 直接播过渡语（不发 LLM），清 bridge 本地历史，下一轮请求带 `X-Session-Action: new`。
- reset 本地历史与发信令原子执行（同一临界区），避免半态。
- bridge 记忆文件：bridge 侧项目自行设计，不在本 spec。

## Testing Decisions

- **测试 seam（单一）**：Session Strategy module 的 interface——acquire/release。池化、TTL、信令、降级、LRU 全部穿过该 seam 用内存 fake 上游测试，无需网络。
- DeepSeek adapter 层：续轮请求体断言（`parent_message_id` 非空、prompt 为增量）、信令解析（`#new#` 前缀剥除）、vision 不进池。参照既有 provider 单测风格（`kimi_test.go` 的 flatten 断言模式）。
- 不测：真实上游续轮行为（`_live` 守卫，需 `DS_TEST_TOKEN`，手工单次验证：quick 模式续轮 + expert 模式续轮各一条，验证 `parent_message_id` 行为与 thinking 语义）；bridge 端到端语音链路（bridge 侧工作项验收）。
- 既有测试回归：deepseek chat 现有单测与 handler 测试全绿（拍平函数保留，主路径改走 strategy）。

## Out of Scope

- bridge 记忆文件的设计与实现（bridge 侧独立工作项）。
- GLM / Grok / Minimax 的会话复用接入（协议同型，二期）。
- qianwen / doubao / mimo / kimi-web 的行为变更。
- aurora 侧任何对话记忆、历史摘要、记忆文件功能。
- coding 变体（已封存，`CODING_ENABLED=false`）。
- vision 识图路径的会话化。

### 明确排除的关联重构（architecture review C2/C3，用户拍板不并入本期）

以下两项与 session 复用同期提出，但**不并入本 spec**——一次变更只动一个 seam，避免「改行为的重构」与「搬代码的重构」耦合在同一批次，出问题无法二分定位：

- **C2 双表面输出管线去重**（~30 个 Stream/NonStream 方法中的重复 delta→SSE 循环收敇为 2 个 sink adapter）：本 spec 触碰的 provider 方法（deepseek chat 4 个）只做最小侵入改动（改 prompt 构造与 session 获取），不做管线塌缩。
- **C3 归一化器下沉**（stripTools/system 降级等 5 类转换关注点收敛）：拍平函数在本 spec 中保留原样，仅调用点收窄；归一化重构等 C1 落地稳定后单独评估。

两项均为 Worth exploring / Speculative，不废弃——后续若立项，另开 spec（建议形态：`.scratch/output-pipeline-dedup/`、`.scratch/normalizer-sink/`）。

## Further Notes

- 延迟收益预估：连续对话第 2 轮起每轮省 ~1.4–2.4s（session create + 全量历史重发），轮数越多收益越大；首字延迟亦随 prompt 缩短而下降。
- 风控收益：流量形态回归真人（增量发送），呼应 coding 限频的既有拍板逻辑。
- 上游会话有效期未实测（可能短于 30min TTL）：失败降级路径兜底，最坏退回每轮新开。
- expert（thinking）模式续轮行为未实测：live 验证项；若上游异常，expert 档退回每轮新开（配置开关），quick 档先行。
- 实现落地时同步改写 `docs/ARCHITECTURE.md`「多轮 = 客户端无状态」一节（该表述将不再成立）与 `docs/DEEPSEEK.md` 多轮说明；CONTEXT.md「拍平」词条补注「仅新会话引导路径使用」。
