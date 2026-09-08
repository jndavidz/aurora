# ChatGPT 自定义 MCP 说法核实与 aurora 接入方案

> 日期：2026-09-01
> **状态：暂不执行**（2026-09-05 用户拍板）。本文是核实与方案设计，落地清单见 §3.2——
> 默认走路线 A（MCP 留在客户端，aurora 只保证 tools 透传）；路线 B（NUC sidecar）待有
> "不支持 MCP 的客户端"需求时再启动；路线 C 不建议。
> 起因：用户转述三句社区说法，要求核实并讨论如何在 aurora 体系内落地。
> 核实方式：联网检索 OpenAI 官方帮助中心 / 发布说明 + 第三方实测（4sysops、Boomi、Coworker、MCP Playground）。
> 结论性质：**三句话里，一句成立、一句部分成立、一句不成立**。

---

## 一、三句话逐句核实

### 说法 ①「ChatGPT 支持自定义 MCP，做 agent 巨简单，你让 ds 给你随便写一个本地 mcp 接上 gpt 就能用了」

**拆分判定：**

| 分句 | 判定 | 依据 |
|---|---|---|
| ChatGPT 支持自定义 MCP | ✅ **成立** | 2025-10-17 起随 Developer Mode 推出完整 MCP 支持（OpenAI Enterprise/Edu Release Notes） |
| 做 agent 巨简单 | ⚠️ **打折** | OpenAI 官方 FAQ 原文：**「智能代理模式（agent mode）不会使用自定义 app」**；Deep Research 只能用**只读**自定义 app |
| 写一个**本地** MCP 接上就能用 | ❌ **不成立** | OpenAI 官方 FAQ 原文：**「Can I connect to a local MCP server? Not currently. Only remote servers are supported.」** |

**本地 MCP 为什么不行 —— 这是最关键的认知修正：**

| 能力 | Claude Desktop | ChatGPT |
|---|---|---|
| 本地 stdio MCP | ✅ 支持 | ❌ **不支持** |
| 远程 HTTP/SSE MCP | ✅ 支持 | ✅ 支持（**且必须 HTTPS 公网可达**） |
| 需要自建基础设施 | 否 | **是** |

ChatGPT 连的是**远程 MCP 服务器**，走 SSE 或 Streamable HTTP，端点必须公网 HTTPS。
跑在笔记本上的 stdio 进程**永远连不上**——要么部署到 VPS / 云函数，要么走隧道
（ngrok / Cloudflare Tunnel / OpenAI 官方的 Secure MCP Tunnel）。

> 所以「随便写一个本地 mcp 接上」这一步是错的，中间隔着一个**公网部署**。

**「做 agent 巨简单」为什么打折 —— 三条实测限制：**

1. **Agent mode 不用自定义 app**（OpenAI 官方明确）。这是最伤的一条——
   你接了 MCP，但最能体现 agent 能力的那个模式**根本不调用它**。
2. **每次新开对话都要重新开 Developer Mode**（4sysops 实测吐槽：
   "you must go through the entire mouse odyssey to enable Developer mode whenever you start a new chat"）。
3. **模型遵从度差**：4sysops 实测 GPT-5 **忽略了 prompt 里"用 Tavily MCP 搜索"的显式指令**，
   被追问后才调用，且**不为不遵循指令道歉**。

### 说法 ②「可以用 sol 搜东西，然后 luna 写代码，luna 速度超级快水平和 v4f 差不多（略差一点）」

**Sol / Luna 确有其物 —— 是 GPT-5.6 家族的三档模型，不是 MCP 能力：**

| 档位 | API model id | 定位 | 输入/输出（每 1M token） |
|---|---|---|---|
| **Sol** | `gpt-5.6-sol`（别名 `gpt-5.6`） | 旗舰，长程编码、多文件、agent 编排 | $5 / $30 |
| **Terra** | `gpt-5.6-terra` | 均衡，日常 agent、首轮实现 | $2 / $12 |
| **Luna** | `gpt-5.6-luna` | 高吞吐、低推理，分类/抽取/草稿 | $0.20 / $1.20 |

- Luna 在 **2026-07-30 降价 80%**（从 $1/$6 降到 $0.20/$1.20），是三档里最便宜的。
- 三档**上下文都是 1.05M / 最大输出 128K**，差异在推理深度与价格，**不是上下文**。
- aurora 的 `models_handler.go:52-56` 已广告 `auto` / `gpt-5.6` / `gpt-coding`，
  其中 `gpt-5-6` 正是 Sol 的 slug（OpenAI 已把 Free/Go 默认模型切到 GPT-5.6 Luna）。

**「用 Sol 搜、用 Luna 写」这套分工是成立的**，本质是**按推理深度路由**：
规划/检索交给 Sol，批量实现交给 Luna。有一个开源项目 `sol-luna-orchestrator`
就是这么做的（MCP server，让 Sol 当 supervisor 把有界任务派给 Luna worker）——
但实测结论有点打脸：**6 次自由选择里 Sol 每次都拒绝派工，强制派工反而更慢**。

> 对 aurora 的直接含义：**模型档位选择是客户端的事**，
> aurora 侧只需保证 `model` 字段原样透传（现状已做到）。

### 说法 ③「ChatGPT 自带插件可以接 MCP，相当于无限额度」

⚠️ **部分成立，但"无限"是错觉。**

**成立的那一半**：相对 API 按 token 计费，订阅制确实没有"余额"概念，
同样的用量不会烧钱。这是"网页反代"整个玩法的基础，也适用于 aurora。

**不成立的那一半 —— 四个天花板：**

| 天花板 | 具体限制 |
|---|---|
| **权限分级** | **Plus / Pro 只能接只读（read/fetch）MCP**；写操作需 **Business / Enterprise / Edu** |
| **官方口径自相矛盾** | OpenAI 开发者指南说 Developer Mode 给 Pro/Plus/Business/Enterprise/Edu「full read and write」；<br>帮助中心说 full MCP「only available to Business and Enterprise/Edu」。<br>→ **两套文档打架，必须以自己账号实测为准** |
| **订阅速率限制** | Plus 有消息数/时间窗限制（尤其高级模型），不是真无限 |
| **Agent mode 绕开** | 最能"烧额度"的模式不用自定义 MCP，等于把最大用量场景堵死了 |

---

## 二、这三句话对 aurora 意味着什么

### 2.1 一个必须先说清的分层事实

**MCP 是客户端侧协议，aurora 是网关。** 二者不在同一层：

```
┌──────────────────────────────────────────────────────┐
│ 上层 agent（ZCode / pi / codebuddy / Claude Code）    │
│   └─ 自带 MCP client ── 连 N 个 MCP server           │  ← MCP 在这一层
└───────────────────────┬──────────────────────────────┘
                        │ OpenAI tools（function calling）
┌───────────────────────▼──────────────────────────────┐
│ aurora 网关（NAS 单副本）                             │  ← aurora 在这一层
│   · /v1/chat/completions   ← 默认规格                 │
│   · /v1/responses          ← 兼容层                   │
│   · ChatGPT 兜底：<tool_call> 文本协议模拟            │
└───────────────────────┬──────────────────────────────┘
                        │
       ┌────────────────┼────────────────┐
       ▼                ▼                ▼
  ChatGPT 网页    国内网页模型      Gemini/Claude
  （重点：编程）   （重点：chat）    （CDP 桥，NUC）
```

**结论：aurora 不需要实现 MCP 也能让上层 agent 用上 MCP。**
上层 agent 自己把 MCP tools 转成 OpenAI `tools` JSON schema 传进来即可。

### 2.2 三条候选路线对比

| 路线 | 做法 | 工作量 | 收益 | 风险 / 代价 | 建议 |
|---|---|---|---|---|---|
| **A · 不动 aurora，MCP 留在客户端** | 上层 agent 自带 MCP client；aurora 只保证 `tools` 透传 + `<tool_call>` 解析可靠 | **0** | 已可用，无新增复杂度 | 不支持 MCP 的客户端用不了 | ✅ **基线，默认走这个** |
| **B · aurora 侧挂 MCP→tools 桥（sidecar）** | 独立进程连 N 个 MCP server，`tools/list` → OpenAI function schema 注入请求；拦截 tool_call → 调 MCP → 结果回灌 | 中（1–2 周） | **任何 OpenAI 兼容客户端都能间接用 MCP** | 引入有状态编排循环 | ⚠️ 可选，**必须做 sidecar，禁止塞进主进程** |
| **C · aurora 暴露 MCP server（反向）** | aurora 对外提供 `/mcp`（Streamable HTTP），让 ChatGPT 网页把 aurora 当工具接 | 中 | ChatGPT 网页可反向调用国内模型 | 需公网 HTTPS 隧道；Plus 只读；agent mode 不支持 | ❌ **暂不做** |

### 2.3 路线 B 的关键设计约束（若要做的唯一正确形态）

**绝对不能塞进 aurora 主进程。** 理由直接来自可靠性计划的原则 2：
「主服务越简单越可靠；凭证生命周期管理外置到 NUC」。

| 维度 | 塞进主进程 | 独立 sidecar |
|---|---|---|
| 主服务复杂度 | ❌ 从"无状态转发"变成"有状态编排" | ✅ 主服务零改动 |
| 故障域 | ❌ MCP server 挂 → aurora 挂 | ✅ 隔离，MCP 挂了只是工具不可用 |
| 落点 | NAS（Celeron N3060，仅 2 核） | ✅ **NUC**（i3-4010U，已有 Node 20 + 常驻基础设施） |
| 音频影响 | ❌ 与 squeezelite 争抢核 0 | ✅ 可套用既定隔离（CPUAffinity=1 3 + Nice=+10 + CPUQuota=80%） |
| 本地 stdio MCP | 容器内不可用 | ✅ NUC 上**可以跑本地 stdio MCP**（sidecar 是 client，不是 ChatGPT 在连） |

> **这一点很重要**：本地 stdio MCP **在路线 B 下完全可用**——
> 因为连它的是 NUC 上的 sidecar 进程，不是 ChatGPT。
> 「不支持本地 MCP」这条限制**只约束 ChatGPT 直连，不约束 aurora 体系**。

### 2.4 路线 C 为什么不值得现在做

| 硬约束 | 影响 |
|---|---|
| 需公网 HTTPS | aurora 在内网 10.10.10.2:65432，要接 ChatGPT 必须穿透（ngrok/CF Tunnel）→ **把内网网关暴露到公网，与"凭证不外泄"红线直接冲突** |
| Plus 只读 | 用户若是 Plus，接进来也只能读，写操作不可用 |
| Agent mode 不支持 | 最有价值的场景被官方堵死 |
| 收益模糊 | ChatGPT 网页调 aurora → 再调国内模型，链路绕两跳，延迟与失败率都不划算 |

**除非**用户有 Business/Enterprise 工作区且愿意做隧道，否则不建议。

---

## 三、建议

### 3.1 按用户 2026-09-01 拍板的战略约束（S1/S2）

> **S1：编程 / agent 用途重点投入 ChatGPT**
> **S2：国内大模型重点保证 chat**

**S1 的真正着力点不是接 MCP，而是加固 `<tool_call>` 文本协议。**

理由：真 API 的 function calling 是**结构化保证**的；aurora 靠
「system prompt 注入调用约定 + 解析模型输出的 `<tool_call>` 块」来**模拟**，
这条链路在 agent 场景下最容易出问题：

| 风险点 | 表现 | 现状 |
|---|---|---|
| 多轮工具调用 | 上下文里堆满历史 tool_call 块，模型开始模仿格式乱输出 | 已有 `sanitizeRefusalHistory`（chat_handler.go:130 / 380） |
| 并行工具调用 | 模型一次吐多个 `<tool_call>`，解析顺序与 id 绑定易错 | 未见专项测试 |
| 参数含特殊字符 | JSON 内出现 `</tool_call>`、引号、换行 → 解析断裂 | `internal/toolcall/` 有 fence/recover，覆盖未知 |
| 模型拒绝协议 | 直接以自然语言回答而不吐 `<tool_call>` | 有 `RefusalRetries`（config.go:120，默认 3） |

**优先级排序（按 S1）：**

1. **`internal/toolcall/` 补 golden 测试**（现状：路线图 G3 已标注"零测试"是硬前置）
   —— 这是**所有后续动作的前置**，也是当前最高 ROI 的一项。
2. 多轮 / 并行 / 特殊字符三组对抗用例 → 暴露真实断点。
3. 路线 B（MCP sidecar）—— 只有在 1、2 做完且确实需要给"不支持 MCP 的客户端"供能时才启动。

> **注意与路线图 G3 的衔接**：G3 是「合并 `so.go`/`turnstile.go` 双份 VM」，
> 硬前置同样是"先补 golden 测试"。两项可合并排期。

### 3.2 落地清单

| # | 动作 | 前置 | 工作量 | 状态 |
|---|---|---|---|---|
| 1 | `internal/toolcall/` golden 测试（多轮 / 并行 / 特殊字符） | 无 | 2–3 天 | ⏸ 待排期 |
| 2 | 对抗用例暴露的断点修复 | 1 | 视结果 | ⏸ |
| 3 | 路线 B：NUC 上 MCP→tools sidecar | 1、2 + 确认有"不支持 MCP 的客户端"需求 | 1–2 周 | ⏸ 待定 |
| 4 | 路线 C：aurora 暴露 /mcp | 需 Business 工作区 + 隧道方案 | — | ❌ 不建议 |

### 3.3 一句话总结

> **社区说法里"接 MCP 做 agent"的门槛被低估了（本地 MCP 连不上、agent mode 不调用、
> Plus 只能只读）；但"订阅制≈无限额度"这个前提对 aurora 依然成立——
> 因为 aurora 是网关，MCP 应该留在上层 agent，aurora 该加固的是
> `<tool_call>` 文本协议这条它独有的、也是最容易断的链路。**

---

## 附：核实来源

| 来源 | 性质 | 关键结论 |
|---|---|---|
| [OpenAI Help Center — Developer mode & MCP apps (beta)](https://help.openai.com/zh-hant-hk/articles/12584461-developer-mode-and-mcp-apps-in-chatgpt-beta) | **官方** | 仅远程服务器；agent mode 不用自定义 app；Deep Research 只读；Business/Enterprise 才有 full MCP |
| [OpenAI Enterprise & Edu Release Notes](https://help.openai.com/en/articles/10128477-chatgpt-enterprise-%E8%88%B7-edu-%E7%99%BC%E8%A1%8C%E8%AA%AA%E6%98%8E) | **官方** | 2025-10-17 推出 full MCP + Developer Mode |
| [Boomi — OpenAI ChatGPT setup](https://help.boomi.com/docs/Atomsphere/Platform/Connect_chatgpt_setup) | 第三方 | 需公网 HTTPS；SSE + Streamable HTTP；隧道方案（ngrok/CF Tunnel） |
| [4sysops — Add ChatGPT MCP server](https://4sysops.com/archives/add-chatgpt-mcp-serveranother-setback-for-openai/) | 第三方**实测** | 每次新对话要重开 Developer Mode；GPT-5 忽略显式 MCP 指令 |
| [Coworker — ChatGPT MCP 2026](https://plg.coworker.ai/blog/chatgpt-mcp) | 第三方 | 方案分级表；**指出 OpenAI 两份文档自相矛盾** |
| [MCP Playground](https://mcpplaygroundonline.com/blog/test-mcp-server-with-chatgpt-and-openai) | 第三方 | Plus/Pro 只读；写操作需 Business/Enterprise/Edu；仅远程 HTTPS 无 stdio |
| [OpenAI — GPT-5.6](https://openai.com/index/gpt-5-6/) | **官方** | Sol / Terra / Luna 三档定位与基准 |
| [developersdigest — GPT-5.6 开发者指南](https://www.developersdigest.tech/blog/gpt-5-6-sol-terra-luna-developer-guide) | 第三方 | 2026-07-30 降价 80%；三档价格与上下文 1.05M/128K |
