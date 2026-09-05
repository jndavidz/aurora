# Aurora 项目状态总览（ ROADMAP · 可靠性 · 进展 三合一）

> 本文档整合取代以下五份文档（已归档至 `docs/archive/`）：
> - `INTEGRATED_ROADMAP_2026-08-31.md`（A–G 整合路线）
> - `RELIABILITY_PLAN_2026-08-31.md`（可靠性设计方案 P0–P6）
> - `PROGRESS_AUDIT_2026-09-01.md`（9/1 源码实证核对）
> - `WORKLOG_2026-08-31.md`（8/31 全天汇总 + 9/1 增量）
> - `REMAINING_OPT_2026-09-04.md`（未完成优化清单）
>
> 归档的是**已被取代的规划/核对类**文档（结论已浓缩进本文，部分状态已过时）。
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
| S1 | 编程/agent 重点投入 ChatGPT | `<tool_call>` 协议可靠性优先；不接 MCP |
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
- **坑清单**（8/31 实踩，沿用）：CDP 键盘需 bringToFront；SSH 远端 `$var` 要转义；`wc -l` 判空用 `[ -s ]`；chown 借 alpine 容器；ps RSS 虚高看 cgroup

---

## 六、关键决策记录（时间序）

| 日期 | 决策 |
|---|---|
| 08-31 | 音频硬约束确立（22:00–24:00 窗口 + 资源隔离 + 三项实测） |
| 09-01 | S1/S2 战略约束；apierrors 保留+修复；MCP 不接 |
| 09-02 | coding 变体全量封存（`CODING_ENABLED=false`，4e0e8c8）；workbuddy 通道转验证期 |
| 09-04 | workbuddy 融合取消（定位收敛纯对话反代）；拟真人仅限测试；C1 四家 TLS 部署；E1/E2/G2 落地；豆包走桥（路线1） |
| 09-05 | 模型 id 全量小写 `-` 化部署；豆包滑块→登出→限频，**冷冻** |
