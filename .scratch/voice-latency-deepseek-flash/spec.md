# Spec: 语音链路降延迟——选定 deepseek-v4-flash,bridge 切 aurora 并开流式

Status: ready-for-agent

来源：2026-10-26 面向主要消费者 open-xiaoai（小爱语音）的通道测速与降延迟方案，用户拍板「模型选 deepseek flash」。测速数据（NAS 生产 aurora，每通道单发×2 轮、真人节奏）：deepseek-v4-flash TTFT 1.8–1.9s、总时长 2.8–3.1s（最快完成、无思考直接出正文、~43 字/s 吐字）；对照 minimax-m3 TTFT 1.4s 但吐字 ~17 字/s、总时长 8s+，kimi/glm 1.5–1.8s 但带思考拖长。deepseek-v4-pro 有思考文本泄漏进正文（TTS 会念出，不可用于语音）；mimo-v2.5-pro 两发皆空回复（通道故障）；qwen 3.9s。

## Problem Statement

小爱语音链路的首音延迟过高：bridge 侧 preflight 实测「LLM 占首音 67%」，而当前生产配置把这块放到了最大——openai 后端非流式（`stream` 未配置，默认关，等全文生成完才返回）且 base_url 直连 DeepSeek 官方 API（`deepseek-chat`），既没有消费 aurora 网关（项目定位 aurora 的主要消费者就是 open-xiaoai），也没有享受 aurora 侧已落地的会话复用（DeepSeek 续轮实测 TTFT 4.8→2.4s）与两期架构修复（Responses 表面 404 修复、流式失败熔断、CDP 后缀归一等）——NAS 上仍在跑旧镜像。同时 aurora 的 coding 封存代码经审计确认对对话路径零运行时影响（O(1) 判定、chat 变体无限频、coding 注册被开关挡），不构成延迟项。

**2026-10-26 复查（上游吸收后）**：主干 `local-toolfix` 已 fast-forward 吸收上游 11 个提交（end_turn/finish_reason、SSE 新格式与 citation、工具 prompt 角色块、`previous_response_id` 续接、tls-client 1.16.0、Chrome 150 指纹画像）。其中**只会影响语音链路的是一条共享依赖变更**：所有网页逆向通道共用的 TLS 客户端从 tls-client 1.15.1 + `Chrome_144` 升到 1.16.0 + `Chrome_150`，而各 `internal/*web` 客户端 UA 常量仍是 `Chrome/144`（minimax/mimo 为 146）——即 UA 与 TLS 版本从此不一致（此前 DeepSeek 是 144↔144 一致的）。其余吸收项落在 ChatGPT 路径，不影响 DeepSeek provider 的延迟与行为。

## Solution

三步配置与部署级收口（两个仓库都不改代码逻辑）：

1. aurora 升级：在主干 `local-toolfix`（镜像 tag 仍为 `aurora:local-toolfix`）构建镜像并经既有部署脚本推 NAS，替换旧镜像——语音链路获得会话复用、两期架构修复与 2026-10-26 上游吸收。
2. bridge 切换：NAS 生产 bridge 配置的 openai 段从「DeepSeek 官方 API 直连」改为「aurora 网关 + deepseek-v4-flash」，开启流式分段（`stream: true`，P1 ticket 03 的分段管线代码已具备只是未启用），并把会话头对齐 aurora 认识的 `X-Session-Key`（连续对话复用上游 session）。
3. 参数与验收：回复长度与历史条数下调（直接砍 prompt 体积与尾部时长）；以与本次测速同口径的单发复测 + 真机人工听感验收首音。

预期效果：首音从「TTFT + 全文生成」（当前官方 API 非流式，体感 ~5s 级）降至「TTFT + 首段」（~2s 级）；连续对话第二轮起再享会话复用增益。

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

## Implementation Decisions

分三个 Work Package；全部为配置/部署变更，无代码逻辑改动。

**WP1 aurora NAS 镜像升级**：
- 在主干 `local-toolfix`（2026-10-26 @ `cd20bca`）构建镜像，经既有 NAS 部署脚本推送并 `compose up`（脚本已含探活）。
- 构建环境约束（本次实测）：本机 16GB 内存 + 系统 commit 接近上限时，`go build ./...` 会 OOM；用 `-p 1`（必要时 `GOMEMLIMIT=768MiB`）跑构建与测试。Docker 构建在 WSL 侧发起（双端纪律）。
- 新镜像相对 NAS 现跑镜像的差异：会话复用（DeepSeek 双表面）、两期架构修复（Responses 404 / 流式熔断 / CDP 后缀归一）、上游吸收（end_turn 与 finish_reason、SSE 2026-08 新格式与 citation、工具 prompt 角色块、`previous_response_id`、tls-client 1.16.0、Chrome 150 指纹）。
- 部署后验证：`/v1/models` 列表含 deepseek-v4-flash；以与测速同口径的单发确认通道健康。
- **新增验证项（上游吸收引入）**：所有网页逆向通道共用的 TLS 客户端已升到 tls-client 1.16.0 + `Chrome_150`，而 `internal/deepseekweb` 的 UA 仍是 `Chrome/144`（UA↔TLS 版本不一致，此前是 144↔144 一致）。DeepSeek 单发必须明确确认无 TLS 握手失败、无 403 / 风控拒绝。
  - 回退一（不改代码、不重建）：给 aurora 容器加 `AURORA_LEGACY_IDENTITY=1` —— DeepSeek 通道标记 `Upgradable`，会回退 Go 原生传输（`httpclient/factory`）。代价是丢掉 TLS 伪装。
  - 回退二（一行代码、需重建）：把 `internal/deepseekweb` 的 `webUserAgent` 对齐 `Chrome/150`；同类 UA 常量还有 glm/doubao/grok/kimi/qianwen/yuanbao（144）与 minimax/mimo（146），属跨通道一致性收口，超出本 spec「零代码改动」前提，单独立项。
- NAS aurora 环境变量除上述可选项外不动（`DEEPSEEK_WEB_SEARCH` 维持现状默认开——「智能搜索恒开」是既有拍板；是否为闲聊关闭留作验收后按需决策，见 Further Notes）。

**WP2 bridge 切换到 aurora + 流式**（改 NAS 上挂载进容器的生产 bridge 配置文件，bridge 代码零改动）：
- openai 段：`base_url` 指向 aurora（bridge 为 host 网络，走 NAS 本机地址即可）；`model` 改 `deepseek-v4-flash`；`api_key` 对齐 aurora 的鉴权值（部署时从 aurora 侧环境读取，不落入任何文档或记录）；`stream` 置真启用流式分段管线。
- `session_header` 改为 aurora 认识的 `X-Session-Key`（bridge 默认发 Hermes 头，aurora 不认；对齐后连续对话按客户端键复用上游 session，只发本轮增量）。
- `max_tokens` 300 → 200、`history_max_messages` 20 → 10（语音闲聊场景：短回复 + 小历史窗口）。
- 变更后重启 bridge 容器；回退 = 配置改回官方 API 三项（base_url/model/stream）。

**WP3 验收**：
- 量化：以本次测速同口径（每通道单发、间隔 ≥2.5s、真人节奏）经 bridge→aurora 链路复测 deepseek-v4-flash 的 TTFT 与总时长；预期 TTFT ≤2s。
- 通道健康：单发明确确认 DeepSeek 在新 TLS 栈（tls-client 1.16.0 / Chrome_150）下不报 403、不被风控拦截；异常时按 WP1 两条回退处置。
- 人工：真机语音一轮连续对话，验收首音体感与流式播报连续性（分段不断流、不念思考）。
- aurora 侧回归：新镜像部署前本地全量测试绿——2026-10-26 吸收后在 `local-toolfix @ cd20bca` 实测 `go test ./...` 37 包全绿（`-p 1`）；WSL 侧 `-race` 关键包尚未在吸收后补跑，部署前需补。

**全局约束**：一切 live 请求遵守防风控纪律（单发、真人节奏、不复跑不重放）；凭证（aurora 鉴权值、上游 token）不进 git、不进文档、不进对话记录——此前一次探查已意外回显 NAS 侧若干服务 token，本次所有远程命令必须先过滤键值再输出；NAS 侧变更执行后回读验证。

## Testing Decisions

- 本 spec 无新增代码 seam：验收全部走生产实例外部行为。
- 量化验收 seam = NAS 生产 aurora 的 chat/completions 流式端点（与 2026-10-26 测速同口径、同 prompt、同计时法：首内容 delta 时间为 TTFT）——同口径才可比。
- 人工验收 = 真机语音一轮（免唤醒或唤醒对话各一条），听感判据：首音 ≤3s 体感、播报无思考文本、不断流。
- aurora 镜像回归 = 既有测试全量：Windows 全仓 `go test ./...`（37 包，2026-10-26 吸收后实测绿，需 `-p 1` 规避 16GB 机器的编译器 OOM）+ WSL `-race` 关键包（吸收后待补）；构建产物部署后 `/ping`、`/v1/models` 探活，并单发确认 DeepSeek 新 TLS 栈健康。
- bridge 侧不新增自动化测试（纯配置变更；流式分段管线自身属 bridge 仓库的测试范围）。

## Out of Scope

- bridge C3 流式管线的深化开发（bridge 仓库 `bridge-architecture-optimization-plan.md` C3 主线的后续,本 spec 只启用既有分段能力）。
- mimo-v2.5-pro 通道故障修复（凭证/协议待查,修复后可另行评估）。
- deepseek-v4-pro 思考泄漏问题（语音场景已排除该模型,低优先级另查）。
- `DEEPSEEK_WEB_SEARCH` 默认值变更（维持「智能搜索恒开」拍板;若验收后 TTFT 不达标,先试关搜索再测,单变量决策）。
- aurora 其他通道（GLM/Kimi/MiniMax 等）的接入与调优;CDP 桥类通道一律不用于语音。
- open-xiaoai 仓库模板配置与 NAS 生产副本的一致性同步（该仓库自身纪律）。

## Further Notes

- 风险两面（用户已拍板方向,如实记录）：网页逆向通道相比官方 API 有结构性风控/封号风险与更高的协议失效频率（上游改版即断）,收益是零 API 费用 + 统一网关治理 + 会话复用;回退路径保留（WP2 三项配置改回即恢复官方 API）。
- 模型 id 依据：NAS aurora `/v1/models` 现出 `deepseek-v4-flash [deepseek]`（token 池非空自动注册）,无需新增 NAS 侧环境变量。
- 会话复用的运维语义（沿用上游会话复用 spec 的拍板）：增减 token 文件行会改变哈希分配,旧会话条目失效一轮（降级新开、自愈）;带会话键请求按客户端键确定性绑定同一账号。
- 测速原始数据（两轮单发）已在本会话记录,验收时同口径复测对比即可。
- **上游吸收的影响面（2026-10-26 复查）**：11 个提交里 9 个有吸收（`8fcdbeb`/`15983ce`/`10ca511` 为部分吸收或带本仓适配），2 个跳过（CI tag 发版、README 徽章）。吸收项绝大多数落在 ChatGPT 路径（`internal/chatgpt`、`internal/handler`、typings、`internal/toolcall`）以及依赖与指纹（tls-client、browserfp）；DeepSeek provider 的请求构造、解析、会话策略**零改动**。因此会话复用/流式的延迟收益判断不变，但 4.8→2.4s 是**吸收前**镜像的实测值，验收必须在新镜像上复测，不得直接引用。
- **UA↔TLS 一致性（待跟进，不在本 spec）**：各 `internal/*web` UA 常量（Chrome/144、minimax/mimo 146）与共享 TLS profile（Chrome_150）不一致。若验收中出现拦截迹象，两条回退见 WP1；长期应收口成「UA 与 TLS profile 同源生成」（如由 `util.FixedUserAgent` 单一来源推导），避免每次指纹升级后逐文件追改。
