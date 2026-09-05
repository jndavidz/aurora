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
> 最后更新：2026-09-05 ｜ 基线：`local-toolfix` @ `5416b19`（已推送，NAS/NUC 已部署）｜ 31 包测试全绿

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

---

## 三、未完成（按优先级）

| 优先 | 项 | 工作量 | 说明 |
|---|---|---|---|
| **P1** | E2 handler 收口 | 0.5 天 | 60 处手拼 `gin.H{"error":…}` → apierrors（机械化替换） |
| **P2** | C2 sec-ch-ua 动态化 | 0.5–1 天 | `builder.go:40` 硬编码 Chromium 头；随 C1 灰度观察 |
| **P2** | C3 device id 账号级固化 | 0.5–1 天 | `client_state.go:24-25` 每请求新 UUID |
| **P2** | N3 硬编码指纹外置 | 1 天 | sentinel SDK `20260423af3c` / Build-Number `7823760` 配置化（上游轮换即求解失败） |
| **P3** | E3 凭证热加载 | 1 天 | deepseekweb 补 mtime 重读（GLM/Kimi 已部分自愈） |
| **P4** | G3 VM 合并 | 1–3 周 | so(1107)+turnstile(1640) 仅 7 同名函数；**暂缓**（上游改版时抽象层先碎） |
| **P4** | G4 拆 handler / F1 webclient / F2 context | 各 1–3 周 | 大重构，按路线图节奏 |

**明确不做**：G2 chat 限频/Pool 信号量（与「速度快」及拟真人拍板冲突）；workbuddy 融合；双活多副本；turnstile/so 抽象层；启用元宝。
（补充自 ARCHITECTURE_AUDIT §9）：不把 ChatGPT 塞进 Provider 接口；不写 turnstile/so 优雅抽象层；不引依赖注入框架；typings 不换官方 SDK；不一次性重写 handler；不强求 10 家客户端 100% 统一（Gemini RPC/Grok WS/Mimo ASR/Doubao 模板是结构性异类）；暂不做指标/追踪体系；测试覆盖率分层设定（toolcall 85%、客户端靠 live 测试）。

### 3.1 遗留缺陷清单（ARCHITECTURE_AUDIT §5，Phase 0 已修 11 处，以下仍未修）

> 来源：`archive/ARCHITECTURE_AUDIT_2026-08-31.md` §5（13 P0 + 35 P1，全带文件:行号）。
> **注意**：同文件多处问题可能被同一次改动顺带修掉，动手前先 grep 现状。

| 级别 | 位置 | 问题 | 现状 |
|---|---|---|---|
| P0-13 | `kimiweb/stream.go:175` | `make([]byte, length)`，length 上游可控 uint32 无上限（deepseekweb 4MB / glmweb 8MB 有界） | **未修**（09-05 复核） |
| P1-6 | `glmweb/stream.go:130,135` | 全量重发协议 `TrimPrefix` 差值不校验 `HasPrefix` → 上游重排引用时**整段重复输出**（glm-flash 偶发重复回复的根因） | **未修** |
| P1-7 | `qianwenweb/stream.go:88` | 同上（geminweb:205 有防御） | 未修 |
| P1-35 | `doubaoweb/client.go:126`/`geminweb/client.go:124` | nextAccount 解锁后 sleep 再重加锁 → 限频被绕过且时间戳失真 | 未修 |
| P1-4 | `grokweb/client.go:163` | 主读循环无 ReadDeadline → 半开 TCP 时 goroutine/fd 泄漏（grok 偶发挂起与此相关） | 未修 |
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
- 回写通路选型：scp + 热加载 ✅（推荐）；POST /admin ⚠️；NFS 谨慎（群晖 ACL 前科）
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
