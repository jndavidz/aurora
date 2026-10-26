# Spec: 语音链路降延迟——选定 deepseek-v4-flash,bridge 切 aurora 并开流式

Status: ready-for-agent

来源：2026-10-26 面向主要消费者 open-xiaoai（小爱语音）的通道测速与降延迟方案，用户拍板「模型选 deepseek flash」。测速数据（NAS 生产 aurora，每通道单发×2 轮、真人节奏）：deepseek-v4-flash TTFT 1.8–1.9s、总时长 2.8–3.1s（最快完成、无思考直接出正文、~43 字/s 吐字）；对照 minimax-m3 TTFT 1.4s 但吐字 ~17 字/s、总时长 8s+，kimi/glm 1.5–1.8s 但带思考拖长。deepseek-v4-pro 有思考文本泄漏进正文（TTS 会念出，不可用于语音）；mimo-v2.5-pro 两发皆空回复（通道故障）；qwen 3.9s。

## Problem Statement

小爱语音链路的首音延迟过高：bridge 侧 preflight 实测「LLM 占首音 67%」，而当前生产配置把这块放到了最大——openai 后端非流式（`stream` 未配置，默认关，等全文生成完才返回）且 base_url 直连 DeepSeek 官方 API（`deepseek-chat`），既没有消费 aurora 网关（项目定位 aurora 的主要消费者就是 open-xiaoai），也没有享受 aurora 侧已落地的会话复用（DeepSeek 续轮实测 TTFT 4.8→2.4s）与两期架构修复（Responses 表面 404 修复、流式失败熔断、CDP 后缀归一等）——NAS 上仍在跑旧镜像。同时 aurora 的 coding 封存代码经审计确认对对话路径零运行时影响（O(1) 判定、chat 变体无限频、coding 注册被开关挡），不构成延迟项。

**2026-10-26 复查（上游吸收后）**：主干 `local-toolfix` 已 fast-forward 吸收上游 11 个提交（end_turn/finish_reason、SSE 新格式与 citation、工具 prompt 角色块、`previous_response_id` 续接、tls-client 1.16.0、Chrome 150 指纹画像）。其中**会影响语音链路的是一条共享依赖变更**：所有网页逆向通道共用的 TLS 客户端从 tls-client 1.15.1 + `Chrome_144` 升到 1.16.0 + `Chrome_150`，而各 `internal/*web` 客户端 UA 常量仍是 `Chrome/144`（minimax/mimo 为 146）——UA 与 TLS 版本不一致（此前 DeepSeek 是 144↔144 一致的）。**处置已定：先做一行 UA 对齐**（`internal/deepseekweb` 的 `webUserAgent` → `Chrome/150`，见 WP1），再构建镜像；其余 `*web` 通道的 UA 收口另立项。吸收项其余部分落在 ChatGPT 路径，不影响 DeepSeek provider 的延迟与行为。

**2026-10-26 复审（按主要消费者=open-xiaoai 重新核对 bridge 代码）**：
- **池命中时 aurora 只发本轮增量的末条消息**（`deepseekChatFlow.buildRequest`：`resume` 时 `prompt = messages[len-1].Text`）——所以「历史窗口」只影响会话未命中轮（首轮/新开/TTL 过期），不影响续轮延迟。
- **bridge 的首音不是首 token**：流式路径先经 `SentenceSegmenter` 切句再逐段 TTS——强标点（。！？；\n）累计 ≥4 字切、弱标点（，、：,;）≥12 字、无标点 ≥20 字强制切（`bridge/core/utils/sentence_split.py`）。首段就绪 = LLM TTFT + 首句生成时间。
- **TTS 是并行段的最后一环**：`xiaoai` 原生 TTS 是「合成落盘 → play_url」阻塞式（`bridge/core/services/speaker.py:_play_text_blocking`），豆包 TTS 才有 `stream: true` 边合成边播。若生产用原生 TTS，L3 首音有固定合成开销，LLM 优化会被部分掩盖。
- **可观测性现成**：bridge `turn_trace` 记录 `llm_start → tts_start（首次段，mark 首次胜出）→ play_url`（`bridge/core/utils/turn_trace.py`），可直接拆出 L1/L2/L3，无需新增埋点。

因此本 spec 的验收口径从「HTTP TTFT + 体感」升级为**三段延迟预算**（见 WP3），并把首句长度作为可调延迟杠杆。

## Solution

三步配置与部署级收口（两个仓库都不改代码逻辑；唯一例外是 WP1 的 UA 字面量对齐）：

1. aurora 升级：在主干 `local-toolfix`（镜像 tag 仍为 `aurora:local-toolfix`）构建镜像并经既有部署脚本推 NAS，替换旧镜像——语音链路获得会话复用、两期架构修复与 2026-10-26 上游吸收。
2. bridge 切换：NAS 生产 bridge 配置的 openai 段从「DeepSeek 官方 API 直连」改为「aurora 网关 + deepseek-v4-flash」，开启流式分段（`stream: true`，P1 ticket 03 的分段管线代码已具备只是未启用），并把会话头对齐 aurora 认识的 `X-Session-Key`（连续对话复用上游 session）。
3. 参数与验收：回复长度、首句长度与历史条数下调（分别作用于尾部、首段、会话未命中轮）；以三段延迟口径 + 真机人工听感验收首音。

预期效果：L1（HTTP 首 delta）≈2s；L2（bridge 首段就绪）= L1 + 首句生成，靠「首句 ≤15 字」约束压到 ≤2.6s；L3（真机首音）= L2 + TTS 起播。相对当前「官方 API 非流式」的体感（全文生成 + 合成，~5s 级）有量级改善；连续对话第二轮起再享会话复用增益。

## User Stories

1. As a 小爱语音用户, I want 问话后音箱尽快开口, so that 对话不卡顿、不像在等机器。
2. As a 小爱语音用户, I want 回复以流式逐段播报, so that 首音之后内容连续不断流。
3. As a open-xiaoai 维护者, I want bridge 统一走 aurora 网关, so that 上游凭证与通道治理单点管理（aurora 的项目定位）。
4. As a open-xiaoai 维护者, I want 默认模型是测速选定的 deepseek-v4-flash, so that 在 TTFT 与吐字速率间取得已验证的均衡。
5. As a open-xiaoai 维护者, I want 连续对话复用上游会话, so that 第二轮起 TTFT 进一步下降。
6. As a open-xiaoai 维护者, I want NAS 上跑的是含两期架构修复与上游修复的 aurora 镜像, so that 语音链路获得 404 修复、流式失败熔断、end_turn/SSE 收尾等质量收益。
7. As a open-xiaoai 维护者, I want 回复长度与历史窗口按语音场景收紧, so that 上游处理时间与播报尾部双降。
8. As a 运维者, I want 切换过程有回退路径（配置改回即回官方 API）, so that 网页通道异常时可快速恢复。
9. As a 运维者, I want 验收数据与本次测速同口径可比, so that 改善可量化确认。
10. As a CI/验证流程, I want aurora 新镜像部署后既有测试与单发探活全过, so that 升级无回归。
11. As a 小爱语音用户, I want 思考过程永远不会被 TTS 念出来, so that 播报内容干净（选 deepseek-v4-flash 的理由之一：无思考直接出正文）。
12. As a 小爱语音用户, I want 助手先给一句不超过 15 字的结论再展开, so that 我最早听到的是答案而不是铺垫（同时把 bridge 首段切分时点提前）。

## Implementation Decisions

分三个 Work Package；除**一处 UA 字面量对齐**（非逻辑改动）外，全部为配置/部署变更。

**WP1 aurora NAS 镜像升级**：
- 先落 UA 对齐：`internal/deepseekweb/client.go` 的 `webUserAgent` 由 `Chrome/144` 改为 `Chrome/150`，与共享 TLS profile（tls-client 1.16.0 / `Chrome_150`）同版本；仅此一行，`go build` + `./internal/deepseekweb` 与 `./internal/provider` 测试已绿。
- 在主干 `local-toolfix` 构建镜像（镜像 tag 仍 `aurora:local-toolfix`），经既有 NAS 部署脚本推送并 `compose up`（脚本已含探活）。
- 构建环境约束（本次实测）：本机 16GB 内存 + 系统 commit 接近上限时 `go build ./...` 会 OOM；用 `-p 1`（必要时 `GOMEMLIMIT=768MiB`）。Docker 构建在 WSL 侧发起（双端纪律）。
- 新镜像相对 NAS 现跑镜像的差异：会话复用（DeepSeek 双表面）、两期架构修复（Responses 404 / 流式熔断 / CDP 后缀归一）、上游吸收（end_turn 与 finish_reason、SSE 2026-08 新格式与 citation、工具 prompt 角色块、`previous_response_id`、tls-client 1.16.0、Chrome 150 指纹与 DeepSeek UA 对齐）。
- 部署后验证：`/v1/models` 列表含 deepseek-v4-flash；以与测速同口径的单发确认通道健康，并明确确认无 TLS 握手失败、无 403 / 风控拒绝。
- 兜底（不改代码、不重建）：若新 TLS 栈触发拦截，给 aurora 容器加 `AURORA_LEGACY_IDENTITY=1` —— DeepSeek 通道标记 `Upgradable`，回退 Go 原生传输（`httpclient/factory`），代价是丢 TLS 伪装。
- NAS aurora 环境变量除上述可选项外不动（`DEEPSEEK_WEB_SEARCH` 维持现状默认开——「智能搜索恒开」是既有拍板；是否关闭见 WP3 决策门）。

**WP2 bridge 切换到 aurora + 流式**（改 NAS 上挂载进容器的生产 bridge 配置文件，bridge 代码零改动）：
- openai 段：`base_url` 指向 aurora（bridge 为 host 网络，走 NAS 本机地址即可）；`model` 改 `deepseek-v4-flash`；`api_key` 对齐 aurora 的鉴权值（部署时从 aurora 侧环境读取，不落入任何文档或记录）；`stream: true`（代码键名 `openai.stream`，默认 false）启用句读分段管线。
- `session_header` 改为 aurora 认识的 `X-Session-Key`（默认 `X-Hermes-Session-Key` aurora 不认）。注意 bridge 的 `session_key` 默认恒定（`agent:default:open-xiaoai-bridge`）→ 所有对话共用同一个上游 session，30 分钟 TTL 后自然新开；「换话题立即新开」需要 `X-Session-Action: new`，bridge 当前未接（follow-up，不在本 spec）。
- **首句约束（新增，直接压 L2）**：在 `system_prompt`（或按 `session_system_prompts`）追加「先用一句不超过 15 字的话给出结论，再展开」。首段切分门槛是 4 字强标点 / 12 字弱标点，首句越短首段越早；该项零代码、可回退。
- `max_tokens` 300 → 220、`history_max_messages` 20 → 10：
  - `max_tokens` 只作用于尾部时长，且必须与 `rule_prompt` 的「300 字以内」自洽——200 tokens 对中文可能中途截断（播报断流的体验问题），故取 220–260；若验收出现截断，先调 `rule_prompt` 到 150 字，不要先动 stream（保持单变量）。
  - `history_max_messages` 只影响会话未命中轮（池命中时 aurora 只发末条，已核实）：10 条 = 5 轮，用于 TTL 过期/新话题后重建上下文。
- **TTS 已确认（2026-10-26 用户拍板）**：生产为 `xiaoai` 原生 TTS（阻塞式「合成落盘 → play_url」），L3 首音含固定合成开销 —— 验收以 L2 为主指标，L3 只记录基线；豆包/流式 TTS 以后另议，不在本 spec。
- **工具能力边界（用户拍板 2026-10-26：aurora 只做对话；语音控制走命令表，不依赖 LLM 工具）**：bridge 的工具注册表**永远非空**（内置 hass / weather / music / device_status / ir_send 五个模块自动注册），且 `tool_calls_enabled` 默认 true —— 即每轮都会把 `tools` 带给后端；而 aurora DeepSeek **chat** 路径不消费 tools（`deepseekSenderReq` 无 tools 字段，工具协议只在已封存的 `-coding` 变体里）。处置：
  - 本期显式配置 `tool_calls_enabled: false`：消除无效 tools 载荷，也避免 bridge 走「探测式流式」（首帧 tool_calls 会放弃请求整轮转非流式）。
  - **无用户可见功能损失**：设备控制由命令表命中（KWS `direct_table`）完成、不进对话，本来就不走 LLM 工具；只有「自然语言随口控制」这类未使用的形态会失去，不构成回退理由。
- 变更后重启 bridge 容器；回退 = 配置改回官方 API 三项（base_url/model/stream），其余参数可保留。

**WP3 验收**（三段预算；目标值是设计目标，不是实测值）：
- **L1 HTTP 首 delta**（aurora chat/completions 流式端点，与测速同口径同 prompt）：首轮目标 ≤2.0s（吸收前实测 1.8–1.9s）；**会话续轮目标 ≤1.5s**（复用上游 session，省掉建会话与全量重发，这是本 spec 的核心增量）。
- **L2 bridge 首段就绪**（`turn_trace` 首条 `tts_start` − `llm_start`）：目标 ≤2.6s，= L1 + 首句生成；首句约束是这里的主要杠杆。
- **L3 真机首音**（`turn_trace` 首条 `play_url` − `llm_start`）：生产为 xiaoai 原生 TTS，**L3 只记录基线、不设硬门槛**（含固定合成开销，主指标看 L2）。
- 通道健康：单发确认新 TLS 栈（tls-client 1.16.0 / Chrome_150 + UA 150）下无 403、无握手失败。
- 人工：真机语音一轮连续对话，验收首音体感与流式播报连续性（分段不断流、不念思考）。
- **决策门（每次只动一个变量）**：L1 > 2.0s → 先关 `DEEPSEEK_WEB_SEARCH` 复测；L1 达标但 L2 不达标 → 收紧首句约束（prompt），不动 bridge 分段门槛（不可配置，属代码变更）；L2 达标但 L3 不达标 → 查 TTS provider / `stream` 设置。
- aurora 侧回归：新镜像部署前本地全量测试绿——2026-10-26 吸收后在 `local-toolfix` 实测 `go test ./...` 37 包全绿（`-p 1`），UA 对齐后 `internal/deepseekweb`、`internal/provider` 复测绿；WSL 侧 `-race` 关键包尚未在吸收后补跑，部署前需补。

**全局约束**：一切 live 请求遵守防风控纪律（单发、真人节奏、不复跑不重放）；凭证（aurora 鉴权值、上游 token）不进 git、不进文档、不进对话记录——此前一次探查已意外回显 NAS 侧若干服务 token，本次所有远程命令必须先过滤键值再输出；NAS 侧变更执行后回读验证。

## Testing Decisions

- 本 spec 无新增代码 seam（除 WP1 的 UA 字面量）：验收全部走生产实例外部行为。
- 量化 seam 分三段，全部来自现成观测面：L1 = NAS aurora chat/completions 流式端点首个 delta 时间（与 2026-10-26 测速同口径、同 prompt、同计时法）；L2/L3 = bridge `turn_trace` 的 `llm_start` / 首条 `tts_start` / 首条 `play_url`（首次胜出语义，无需新增埋点）。
- 人工验收 = 真机语音一轮（免唤醒或唤醒对话各一条），听感判据：首音 ≤3s 体感、播报无思考文本、不断流、首句先给结论。
- aurora 镜像回归 = 既有测试全量：Windows 全仓 `go test ./...`（37 包，2026-10-26 吸收后实测绿，需 `-p 1` 规避 16GB 机器的编译器 OOM）+ WSL `-race` 关键包（吸收后待补）；构建产物部署后 `/ping`、`/v1/models` 探活，并单发确认 DeepSeek 新 TLS 栈健康。
- bridge 侧不新增自动化测试（纯配置变更；流式分段管线自身属 bridge 仓库的测试范围）。

## Out of Scope

- bridge C3 流式管线的深化开发（bridge 仓库 `bridge-architecture-optimization-plan.md` C3 主线的后续,本 spec 只启用既有分段能力）。
- mimo-v2.5-pro 通道故障修复（凭证/协议待查,修复后可另行评估）。
- deepseek-v4-pro 思考泄漏问题（语音场景已排除该模型,低优先级另查）。
- `DEEPSEEK_WEB_SEARCH` 默认值变更（维持「智能搜索恒开」拍板；仅在 WP3 决策门触发时作为单变量复测项）。
- aurora 其他通道（GLM/Kimi/MiniMax 等）的接入与调优;CDP 桥类通道一律不用于语音。
- open-xiaoai 仓库模板配置与 NAS 生产副本的一致性同步（该仓库自身纪律）。
- **自然语言设备控制（LLM 工具）**：不是需求——用户确认语音控制走命令表命中（KWS `direct_table`），不依赖 LLM 工具；本 spec 显式关闭 `tool_calls_enabled` 即可。
- TTS 换豆包/流式合成（用户 2026-10-26 拍板：以后再说）。

## Further Notes

- 风险两面（用户已拍板方向,如实记录）：网页逆向通道相比官方 API 有结构性风控/封号风险与更高的协议失效频率（上游改版即断）,收益是零 API 费用 + 统一网关治理 + 会话复用;回退路径保留（WP2 三项配置改回即恢复官方 API）。
- 模型 id 依据：NAS aurora `/v1/models` 现出 `deepseek-v4-flash [deepseek]`（token 池非空自动注册）,无需新增 NAS 侧环境变量。
- 会话复用的运维语义（沿用上游会话复用 spec 的拍板）：增减 token 文件行会改变哈希分配,旧会话条目失效一轮（降级新开、自愈）;带会话键请求按客户端键确定性绑定同一账号。
- 测速原始数据（两轮单发）已在本会话记录,验收时同口径复测对比即可。
- **上游吸收的影响面（2026-10-26 复查）**：11 个提交里 9 个有吸收（`8fcdbeb`/`15983ce`/`10ca511` 为部分吸收或带本仓适配），2 个跳过（CI tag 发版、README 徽章）。吸收项绝大多数落在 ChatGPT 路径（`internal/chatgpt`、`internal/handler`、typings、`internal/toolcall`）以及依赖与指纹（tls-client、browserfp）；DeepSeek provider 的请求构造、解析、会话策略**零改动**。因此会话复用/流式的延迟收益判断不变，但 4.8→2.4s 是**吸收前**镜像的实测值，验收必须在新镜像上复测，不得直接引用。
- **UA↔TLS 一致性**：DeepSeek 已在 WP1 对齐为 `Chrome/150`（唯一一行代码改动）；其余 `internal/*web` 通道（glm/doubao/grok/kimi/qianwen/yuanbao 144、minimax/mimo 146）仍未对齐，属共享 TLS profile 升级的遗留，另立项收口为「UA 与 TLS profile 同源生成」（如由 `util.FixedUserAgent` 推导），避免每次指纹升级后逐文件追改。
- **已否决的加速手段（如实记录）**：
  - **预热会话**（唤醒时预建上游 session）：能消掉首轮建会话成本，但等于每唤醒多打一次上游请求，与防风控纪律（减少机器化痕迹）冲突，且首轮收益无法复用到「用户换话题」场景——不做。
  - **bridge 分段门槛参数化**（如首段强制 4 字切）：`SentenceSegmenter` 门槛目前是构造参数、未暴露配置，改它属 bridge 代码变更，超出本 spec「bridge 零改动」；改用 prompt 首句约束达到同等效果。
  - **切换豆包流式 TTS**：能压 L3，但引入外部 TTS 依赖与音色变更；用户 2026-10-26 拍板「以后再说」，本期不做。
- **首轮 vs 续轮的验收分工**：桥接连续对话默认开启，第二轮起才触发会话复用；L1 续轮目标（≤1.5s）必须单独测一组「同 session_key 的第二轮」，不能拿首轮数据代替。
