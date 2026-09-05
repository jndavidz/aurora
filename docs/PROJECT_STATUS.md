# Aurora 项目状态总览（ ROADMAP · 可靠性 · 进展 三合一）

> 本文档整合取代以下五份文档（已归档至 `docs/archive/`）：
> - `INTEGRATED_ROADMAP_2026-08-31.md`（A–G 整合路线）
> - `RELIABILITY_PLAN_2026-08-31.md`（可靠性设计方案 P0–P6）
> - `PROGRESS_AUDIT_2026-09-01.md`（9/1 源码实证核对）
> - `WORKLOG_2026-08-31.md`（8/31 全天汇总 + 9/1 增量）
> - `REMAINING_OPT_2026-09-04.md`（未完成优化清单）
>
> 归档的是**已被取代的规划/核对类**文档（结论已浓缩进本文，部分状态已过时）。
> MCP 方案文档已移回 [MCP_INTEGRATION_2026-09-01.md](MCP_INTEGRATION_2026-09-01.md)（暂不执行，独立保留）。
> 仍保留在 docs/ 的：各通道 `*.md`（活协议文档）、`NUC_RESOURCE_ANALYSIS_2026-08-31.md`
>（音频隔离实测数据基线，长期有效）、`PI_AGENT_DEBUG.md`（pi 路由适配活运维知识，
> router.go:115 引用）、`ARCHITECTURE_AUDIT_2026-08-31.md`（1712 行全量审计，查阅用）。
>
> 最后更新：2026-09-05（下午）｜ 基线：`local-toolfix`（5416b19 之后新增 E2 收口 + 遗留缺陷清零 + C2/C3/N3）｜ 31 包测试全绿

---

## 一、定位与战略约束（用户拍板，持续有效）

| # | 约束 | 影响 |
|---|---|---|
| 定位 | **纯对话网页版大模型反代**（2026-09-04 拍板） | 不与 workbuddy 项目融合；全部融合类规划终止 |
| S1 | 编程/agent 重点投入 ChatGPT | `<tool_call>` 协议可靠性优先；不接 MCP（核实与方案见 [MCP_INTEGRATION_2026-09-01.md](MCP_INTEGRATION_2026-09-01.md)，**暂不执行**：本地 MCP 不可行/agent mode 不调用/Plus 只读；默认路线 A=MCP 留客户端） |
| S2 | 国内模型重点保 chat | `-coding` 变体封存不加码；C1 灰度验收看 chat 通过率 |
| 拟真人边界 | **仅限测试动作，禁止写进生产代码** | 正常使用即真人节奏；代码限频/抖动=加延迟+机器特征 |
| 音频红线 | NUC 首要职责是 LMS 音频 | 需浏览器的动作排 22:00–24:00；资源隔离已实测生效 |

---

## 二、已完成（全部经源码/部署实证）

### 基础设施（原阶段 A/B/D + P0–P3）

| 项 | 提交 | 说明 |
|---|---|---|
| A1 续期短路修复 | `bcb69eb` | GLM/Kimi `exp-now<余量` 才续期；失败清票（8 调用点包装） |
| A2 凭证卷拆分 | `95aecce` | `poolfile` 原子回写 + `tokens-state/` rw 卷；错误上抛 |
| A3 健康端点 | `44c8e23` | `GET /v1/health/credentials`（jwtutil 解析 exp；分档 expired/critical/warn/ok） |
| A4 死代码删除 | `f774f15` | -1254 行（apierrors 保留） |
| B 工厂 | `1dc63a2` | `httpclient/factory` 9 家接入；`AURORA_LEGACY_IDENTITY=1` 回滚开关 |
| D1–D5 NUC 常驻 | `59cbf9e`/`6ac1343`/`c855b38` | Chrome+桥 systemd 常驻（资源隔离 cgroup 实测生效）；keeper probe/alert；NUC 桥池置首 |
| D3 三测 | — | 机器侧 ✅（压力下延迟 106μs、零 underrun）；主观听音**用户拍板取消** |

### 可靠性与性能（09-04/05）

| 项 | 提交 | 说明 |
|---|---|---|
| E1 错误分类状态机 | `3003f86` | `ClassifyFailure`（纯状态码）+ `Pool.ReportResult`（冷却/回池）+ Acquire 跳过冷却 |
| E2 apierrors 双写修复 | `3003f86` | `MissingParam` 单次写入 + 回归测试；**handler 60 处收口仍待做** |
| G2 provider 熔断 | `2fbe50d` | `ProviderBreaker`（3 次≥500 摘除 60s 半开恢复）；Resolve 跳过熔断；handler defer 记录 |
| N1 测试修复 | `2c91bb3` | provider 包 3 个失败（97181ee 格式变更遗留） |
| N4 GLM 搜索开关 | `bab60f0` | `GLM_WEB_SEARCH`（默认关） |
| deepseek 搜索开关 | `6acf0c7` | `DEEPSEEK_WEB_SEARCH`（默认关，**实测 -700ms/-23%**） |
| C1 灰度 | `5204d31`/`6a2e83c` | 4 家 TLSFaked 全量部署（deepseek/glm/kimi/doubao） |
| doubao-hook | `db120b4`/`710d533` | Network 主通道（前端改 XHR 后 fetch hook 失效的对策）；a_bogus 非空校验 |
| 豆包 CDP 桥 | `6b2eb84` | bridge doubao adapter（页内 fetch）+ aurora doubao_cdp.go |

### 模型命名统一（09-04/05，`31b8df8`+`5416b19`）

**15 个模型 id 全部小写 `-` 形式**（NAS live 已验证）：

```
auto  gpt-5.6  gpt-5.6-mini  gemini-3-flash  claude-sonnet-5
minimax-m3  mimo-v2.5-pro  mimo-v2.5-asr  deepseek-v4-flash  deepseek-v4-pro
glm-flash  kimi  qwen-3.8-max  doubao  grok-3
```

- 去 `-chat` 后缀（deepseek/glm/kimi/doubao/gemini/claude/minimax/mimo/grok）
- **glm-flash = GLM-5.3-FLASH**（用户拍板升级，上游暂沿用 5.2 入口）；`glm-5.2-chat-thinking` 删除
- qwen 透传上游需 slug 映射：`upstreamSlug()`（provider.go，唯一映射点，`qwen-3.8-max → Qwen3.8-Max`）
- glm thinking 挡删除；kimi 暴露快速档
- `/v1/models` 新增 `friendly_name` 字段（非标准，OpenAI 客户端自动忽略）
- 修复改名的连锁回归：`newCdpBase` 对 `gpt-5.6-chat` 的解析跳过（-coding 前置判定）；桥侧 `aliases: ["auto","doubao"]`
- **破坏性变更**：旧 `-chat` id 已 404；面板自建 preset 需同步

### 反检测加固 + E2 收口 + 遗留缺陷清零（09-05 下午）

| 项 | 说明 |
|---|---|
| E2 handler 收口 | chat/audio/image_handler 共 50+ 处手拼 `gin.H{"error":gin.H{...}}` → `apierrors.JSONError`/`NotFoundAccount`；apierrors 新增 `Param()` 辅助；`Not Account Found.` 统一走 `NotFoundAccount`（含 Abort 语义）；简单字符串 403/500 保持原格式不强行改形 |
| C2 sec-ch-ua 动态化 | `util.SecChUaForUA()` 从 UA 提取 Chrome 主版本派生 `Sec-Ch-Ua`；`WithUserAgent`/`NewBaseHeaderWithState` 覆盖 UA 时自动重对齐（版本不一致=经典 bot 信号） |
| C3 device id 账号级固化 | `NewChatClientStateForAccount()`：优先账号指纹 OaiDeviceID → token 派生确定性 v4 型 UUID → 兜底随机；6 个调用点全部接入；SessionID 保持每请求新生成（对齐浏览器每标签页语义） |
| N3 指纹外置 | `SENTINEL_SDK_VERSION`（默认 20260423af3c，6 处引用点：browserfp/sentinel/prooftoken/so/turnstile）与 `OAI_BUILD_NUMBER`（默认 7823760）环境变量化，上游轮换改 compose 环境变量即可，无需重编译 |
| P0-13 修复 | kimiweb 帧 `make([]byte, length)` 加 8MB 上界（对齐 glmweb 8MB/deepseekweb 4MB），防上游异常大帧内存耗尽 |
| P1-4 修复 | grokweb 主读循环每帧刷新 120s ReadDeadline，半开 TCP 不再永久挂起 goroutine/fd |
| P1-7 修复 | qianwenweb 差值输出校验 HasPrefix（同 P1-6 glmweb 模式），上游重排引用时整体替换而非重复输出 |
| P1-35 修复 | doubaoweb/geminweb `nextAccount` unlock→sleep→relock 竞态：改为锁内预标记 `lastUsed=now+wait`（wait>0）或 now（wait≤0），并发 goroutine 无法再绕过限频 |
| E3 凭证热加载 | deepseekweb `NextToken` 每次先 stat token 文件，mtime 变化即整池重读（keep-last-good：读失败/空文件沿用旧池）；顺带修掉 cursor 无锁竞态（`sync.Mutex`）。minimax/mimo 同日补齐：provider 层 `webClient` 记录 tokenMod，mtime 变更置空重建。keeper scp 重推文件后进程内即时生效，无需重启。测试：deepseekweb `hotreload_test.go`（含 -race）+ provider `hotreload_test.go` |
| F2 收窄版 | `chatRequestState` struct 替代 toolCalling 系列 6 指针输出参数（`fba5b63`）。澄清语义：仅 clientState 是 in-out，其余纯输入。零业务逻辑变更，S1 核心路径后续改动的复利点。G4 的"拆 handler"以此为起点自然推进 |
| G1 终态:NUC 统一凭证提取器 | 架构事实（用户拍板 09-05）:**各模型登录态权威在 NUC Chrome**。`scripts/nuc/token-harvester.{mjs,service,timer}`（已部署 NUC,timer 每日 **08:15**+rand30min,对齐 NUC 开机窗口 08:00–00:30,Persistent 补跑）：CDP 读各站 localStorage/cookie → md5 幂等推 NAS 部署区 → E3 热加载生效。站点 minimax/mimo/qianwen/grok/deepseek（qianwen 因新鲜 x5sec **通道复活**;deepseek 经 JSON 解包修复已纳入）;排除豆包(冻结)/GLM·Kimi(自愈)/桥体系。**MiniMax 每日签到同步迁 NUC**（`minimax-checkin.timer` 09:00+rand30min,读 harvester state 新票,实测 day4 +1000 积分）;Windows keepalive 任务计划已由用户删除,PC 侧 `.runtime/tokens` 过时副本已清理。教训链一并入库:首批 NAS 侧脚本方向错误（Drive 排除 `.runtime`,同步区死水）覆盖部署区 5 文件→PC 权威源恢复→终态 NUC 统一提取（详见 CREDENTIALS.md）。教训链一并入库:首批 NAS 侧脚本方向错误（Drive 排除 `.runtime`,同步区死水）覆盖部署区 5 文件→PC 权威源恢复→PC 侧脚本也被 NUC 架构取代撤销。**PC `.runtime/tokens/` 已降级为历史快照可清理**（详见 CREDENTIALS.md §NUC 统一凭证提取器） |
| ~~G1 push 修正版~~ | superseded:中间态 `push-tokens-from-pc.sh`（PC→NAS）随"登录态统一 NUC"架构撤销;事故与教训保留在 git 历史（`7080e2d`→撤销→`49db7ea` 终态）。群晖 sshd SFTP 不兼容需 `scp -O` |
| G3 前置 golden | so/turnstile VM 零测试 → `golden_test.go` ×2（09-05）：微型字节码锁定核心 opcode 语义（赋值/拷贝/加法拼接/xor/base64/JSON/删元素/子队列）+ 端到端 success 字节级快照 + P0-4 无限循环终止哨兵。首轮失败即澄清关键语义：**操作类 opcode 参数均为寄存器引用，字面值须先 op2 入寄存器**。真实 dx 样本待 live 抓取后可叠加全流程快照 |
| G4 第一步 | `internal/handler/tool_calling.go`（09-05）：toolCalling 全集（chatRequestState + 双入口 + 共享收集器 + looksLike* 分类器 + 输出辅助）自 chat_handler.go/shared.go 原样迁出，纯移动零逻辑变更。handler 内chat 路径文件收敛为：chat_handler（入口编排）/ tool_calling（工具调用）/ shared（请求基础设施） |

---

## 三、未完成（按优先级）

| 优先 | 项 | 工作量 | 说明 |
|---|---|---|---|
| ~~P1~~ | ~~E2 handler 收口~~ | — | **已完成**（09-05 下午，50+ 处机械化替换，见 §二） |
| ~~P2~~ | ~~C2 sec-ch-ua 动态化~~ | — | **已完成**（09-05 下午，`util.SecChUaForUA`） |
| ~~P2~~ | ~~C3 device id 账号级固化~~ | — | **已完成**（09-05 下午，`NewChatClientStateForAccount`） |
| ~~P2~~ | ~~N3 硬编码指纹外置~~ | — | **已完成**（09-05 下午，`SENTINEL_SDK_VERSION`/`OAI_BUILD_NUMBER`） |
| ~~P3~~ | ~~E3 凭证热加载~~ | — | **已完成**(09-05 下午,deepseekweb mtime 重读;minimax/mimo 同类问题见 §3.3 注记) |
| ~~P4~~ | ~~G3 VM 合并~~ | **前置已备** | so/turnstile golden 测试已就位（09-05）——合并的测试保护网完成；合并本身仍**暂缓**（上游改版时抽象层先碎，等稳定期+真实 dx 样本再评估） |
| ~~P4~~ | ~~G4 拆 handler / F1 webclient / F2 context~~ | — | **F2 收窄版已完成**（chatRequestState，09-05）；**G4 第一步已完成**（09-05：toolCalling 全集 ~660 行迁出至 `tool_calling.go`，chat_handler 1248→780、shared 518→325，纯移动零逻辑变更，ARCHITECTURE §5.2 已注记）；F1 降级"暂不做"（收益已被工厂化蚕食，有具体重复痛点再抽） |

**明确不做**：G2 chat 限频/Pool 信号量（与「速度快」及拟真人拍板冲突）；workbuddy 融合；双活多副本；turnstile/so 抽象层；启用元宝。
（补充自 ARCHITECTURE_AUDIT §9）：不把 ChatGPT 塞进 Provider 接口；不写 turnstile/so 优雅抽象层；不引依赖注入框架；typings 不换官方 SDK；不一次性重写 handler；不强求 10 家客户端 100% 统一（Gemini RPC/Grok WS/Mimo ASR/Doubao 模板是结构性异类）；暂不做指标/追踪体系；测试覆盖率分层设定（toolcall 85%、客户端靠 live 测试）。

### 3.1 遗留缺陷清单（ARCHITECTURE_AUDIT §5，Phase 0 已修 11 处 + 09-05 下午清零 4 处）

> 来源：`archive/ARCHITECTURE_AUDIT_2026-08-31.md` §5（13 P0 + 35 P1，全带文件:行号）。
> **注意**：同文件多处问题可能被同一次改动顺带修掉，动手前先 grep 现状。
> **09-05 下午**：审计清单内所有"未修"项已全部修复（P0-13 / P1-4 / P1-7 / P1-35），见 §二。

| 级别 | 位置 | 问题 | 现状 |
|---|---|---|---|
| P0-13 | `kimiweb/stream.go:175` | `make([]byte, length)`，length 上游可控 uint32 无上限 | **已修**（8MB 上界，09-05 下午） |
| P1-4 | `grokweb/client.go:163` | 主读循环无 ReadDeadline → 半开 TCP 泄漏 goroutine/fd | **已修**（120s 每帧刷新，09-05 下午） |
| P1-6 | `glmweb/stream.go:130,135` | TrimPrefix 差值不校验 HasPrefix → 整段重复输出 | 已修（`f10eb1a`，glm-flash 偶发重复回复根因） |
| P1-7 | `qianwenweb/stream.go:88` | 同 P1-6 | **已修**（09-05 下午，HasPrefix 校验+整体替换） |
| P1-35 | `doubaoweb/client.go:126`/`geminweb/client.go:124` | nextAccount 解锁后 sleep 再重加锁 → 限频被绕过 | **已修**（锁内预标记 lastUsed=now+wait，09-05 下午） |
| 其余 P0-2/3/4/5/6/7/12 | so.go 递归清寄存器、并发写 map、runQueue 无上限、browserfp 未判空、api/router init 双重初始化 | **Phase 0 已修**（52 文件 +521/-316，见 audit §7 注记）——动手前先核对 | 已修 |

Phase 0 已修 11 处清单（防重复修）：prooftoken diffLen 上界、api/router 显式 Initialize+sync.Once、minimaxweb instanceUUID 加锁、qianwenweb/yuanbaoweb onDelta nil 守卫、sseparser Parts 长度保护 ×2、browserfp RWMutex+惰性初始化（连带修 turnstile/prooftoken nil panic）、toolcall/fence（只剥工具围栏+尾部反引号保留 2）、toolcall/recover（工具名白名单+大小写归一）、so.go（opcode0 独立 solver/50000 步上限/Snapshot 60s 超时）。

### 3.2 对外接口规格（活知识）

| 路径 | Handler | 规格 |
|---|---|---|
| `POST /v1/chat/completions` | `Nightmare` | OpenAI Chat Completions |
| `POST /v1/responses` | `Responses` | OpenAI Responses API |
| `POST /v1/models/responses` | `Responses` | **pi 适配器专用别名**（来源：PI_AGENT_DEBUG.md §3，router.go:115） |

### 3.3 保活触发阈值与 keeper 设计（RELIABILITY §3.3/§5.2/§5.3）

- 剩余 >30% 不动作；30%~10% 触发保活（refresh 优先）；≤10% 保活+告警；已失效告警+摘除转兜底
- credential-keeper 四件套：**probe**（每 6h，随时）/ **act**（refresh 随时、CDP 型 22:00–24:00、人工型只告警）/ **publish**（scp 推 NAS）/ **alert**（日志+webhook，去重）
  - **push 修正版（09-05，事故后重做）**：`scripts/keeper/push-tokens-from-pc.sh` 在 **PC（WSL）**运行，本地 `.runtime/tokens/`（keepalive 代取 + 人工重抓落点）→ NAS 部署区直推（幂等/scp -O）。⚠ 首版"NAS 同步区→部署区"方向错误已撤销——Drive 排除 `.runtime` 隐藏目录，同步区是死水；**doubao 明确排除**（权威在 NUC doubao-hook）。DSM 定时任务无需注册；DeepSeek/Grok/千问重抓后手动跑一次即可
- 回写通路选型：scp + 热加载 ✅（推荐）；POST /admin ⚠️；NFS 谨慎（群晖 ACL 前科）
- **热加载现状（09-05 E3 全量完成）**：deepseekweb（`NextToken` 内 mtime 检查+整池重读）、minimax/mimo（provider 层 `webClient` mtime 变更重建 client）均已支持；
  GLM/Kimi 靠 refresh 续期自愈。keeper publish 实装后对所有通道即时生效，无需重启
- 失败退避 1min→5min→30min；需浏览器的重试排次日窗口，不当夜反复

### 3.4 toolcall 双协议活知识（AUDIT §6.5）

| 维度 | 标签协议（tags.go+parser.go） | 围栏协议（fence.go） |
|---|---|---|
| 变体容忍 | 强：6 条正则归一化 | 零 |
| 半分隔符保护 | keepPartialTagTail 保留最长前缀 ✅ | 只保留 1 个字符 ❌（Phase 0 改为 2） |
| 解析失败 | Flush 回吐原文 | 静默丢弃 |
| 适配 | ChatGPT/DeepSeek/Doubao/Yuanbao | GLM/Gemini/Grok/MiniMax/Mimo |

> GLM 网页版会忽略 `<tool_call>` 标签（fence.go:11-19 实证），故 GLM 必须走围栏协议——这是协议选择的历史依据。

---

## 四、通道现状（NAS live，09-05）

| 通道 | 模型 | 状态 | 延迟参考 |
|---|---|---|---|
| deepseek | deepseek-v4-flash / -pro | ✅ | 2.2–3.9s |
| glm | glm-flash（5.3） | ✅ | 1.5–2.3s（偶发重复帧，观察） |
| kimi | kimi | ✅ | 3.6–7.8s |
| qwen | qwen-3.8-max | ✅ | 0.7–2.9s（偶发上游抖动空流，自愈） |
| chatgpt 桥 | gpt-5.6 / gpt-5.6-mini / auto | ✅ | 20–48s（UI 驱动特性） |
| gemini 桥 | gemini-3-flash | ✅ | ~48s（UI 驱动） |
| claude 桥 | claude-sonnet-5 | ✅ | 4.4–9.8s |
| minimax / mimo / grok | minimax-m3 / mimo-v2.5-pro / grok-3 | ✅ | 4.3–11.7s（grok 偶发 ws 403，上游问题） |
| doubao | doubao | ⏸ **冷冻** | 滑块→登出→重登→限频（710022002）；解冻 SOP 见下 |

**doubao 解冻 SOP**（用户下令后执行）：重启 doubao-hook → 用户 VNC 页面发一条消息（真人节奏）→ hook 捕获推 NAS → 同步桥 session → 重启桥 → 单轮低频验证。

---

## 五、运行拓扑与运维

```
NAS(10.10.10.2) aurora 容器 :65432 ──┬── NUC(10.10.10.3) Chrome CDP:9222 + aurora-bridge:8799【首位】
   tokens/ ro                        │      doubao-hook.service（冷冻中）
   tokens-state/ rw                  └── PC(10.10.10.6 / 10.20.0.2) 桥【备份】
   credential-keeper 每日 06:30 probe/alert
```

- **回滚开关**：`AURORA_LEGACY_IDENTITY=1`（C1 四家 TLS → Go 原生）
- **豆包凭据链**：页面 UI 对话 → doubao-hook（Network 事件）捕获 → 推 NAS tokens → 桥 capture 同步会话状态
- **坑清单**（8/31 实踩，沿用）：CDP 键盘需 bringToFront（失焦时 Enter 不进编辑器）；SSH 远端 `$var` 要转义（`\$f`）；`wc -l` 判空用 `[ -s ]`；chown 借 alpine 容器（`65532:65532`，tokens-state 属主）；ps RSS 虚高看 cgroup `memory.current`；LMS playerid 是 `00:00:00:00:00:00` 非 MAC
- **渲染定稿**：Xvfb 1280×720×24 + Chrome `--disable-gpu` + `--num-raster-threads=4`（软件渲染下"鼠标顺网页慢"的修复）
- **续期余量**：GLM refreshSkew=10min（access ~2h）/ Kimi=3min（access ~15min）；GLM 池 162 天 / Kimi 池 72 天（A3 实测）

---

## 六、关键决策记录（时间序）

| 日期 | 决策 |
|---|---|
| 08-31 | 音频硬约束确立（22:00–24:00 窗口 + 资源隔离 + 三项实测） |
| 09-01 | S1/S2 战略约束；apierrors 保留+修复；MCP 不接 |
| 09-02 | coding 变体全量封存（`CODING_ENABLED=false`，4e0e8c8）；workbuddy 通道转验证期 |
| 09-04 | workbuddy 融合取消（定位收敛纯对话反代）；拟真人仅限测试；C1 四家 TLS 部署；E1/E2/G2 落地；豆包走桥（路线1） |
| 09-05 | 模型 id 全量小写 `-` 化部署；豆包滑块→登出→限频，**冷冻** |
| 09-05（下午） | E2 收口 + 遗留缺陷清零（P0-13/P1-4/P1-7/P1-35）+ C2/C3/N3 反检测加固；指纹参数外置为 `SENTINEL_SDK_VERSION`/`OAI_BUILD_NUMBER` |
| 09-05（晚） | F2 收窄版（chatRequestState）；G1 publish 方向事故→撤销→修正为 PC 侧 push（Drive 排除 `.runtime` 的教训入库）；G3 前置 golden（so/turnstile VM 语义快照）；G4 第一步（toolCalling 拆分） |
