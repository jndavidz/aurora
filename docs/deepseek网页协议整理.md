# chat.deepseek.com 网页接口协议整理

> 逆向来源:本地 `D:\dev\src\z_ref` 下 10 个参考仓库(详见 §1)。本文档为**实施前资料**,结论需经官网实测验证后才进入编码(验证清单见 §9)。
>
> 主参考实现(按可信度): **zyj-deepseek-2api**(TS, V4 时代最新) · **ds-free-api**(Rust, 功能最全) · **masterzerno/deepseek-2api**(Go, PoW 参考)。
> 整理日期: 2026-08-09

---

## 目录

1. [仓库谱系与可信度](#1-仓库谱系与可信度)
2. [总体架构与固定请求头](#2-总体架构与固定请求头)
3. [会话生命周期](#3-会话生命周期)
4. [PoW(DeepSeekHashV1)](#4-powdeepseekhashv1)
5. [Chat Completion](#5-chat-completion)
6. [响应流 SSE 解析(双格式)](#6-响应流-sse-解析)
7. [工具调用与提示词注入](#7-工具调用与提示词注入)
8. [模型名映射](#8-模型名映射)
9. [★ 官网验证清单(编码前置)](#9--官网验证清单编码前置)
10. [Aurora 接入设计方向(token 文件注入池)](#10-aurora-接入设计方向token-文件注入池)
11. [封号风险与对策(社区实测)](#11-封号风险与对策社区实测)

---

## 1. 仓库谱系与可信度

| 仓库 | 语言 | 协议版本 | 定位 | 可信度/价值 |
|---|---|---|---|---|
| **zyj-deepseek-2api** (`zyj-deepseek-2api/`) | TypeScript (Cloudflare Worker) | **V4 时代最新** | 登录/换 token、会话、PoW、双格式 SSE 解析 | ⭐ 主参考 |
| **ds-free-api** (`ds-free-api/`) | Rust | 最新 | 账号池/健康检查/文件上传/超限回退/工具注入实践 | ⭐ 主参考 |
| **masterzerno/deepseek-2api** (`deepseek-2api/`) | Go | V3 主流 | 纯 Go PoW(DeepSeekHashV1)+ 极简代理 | ⭐ PoW 可直接移植 |
| **lza6-deepseek-2api** (`lza6-Deepseek-2api/`) | Python (FastAPI) | 较旧 | 头部/载荷参照(cookie + Bearer + x-app-version) | ◐ 仅参照 |
| **HelloDeepseek** (`HelloDeepseek/`) | TS | 与 zyj 同源, V3 | = zyj 早期版, 无 V4 解析 | ◐ 对比 |
| **zxy1994-deepseek-2api** (`zxy1994-deepseek-2api/`) | TS | V3 前身 | zyj 的 V3 旧版 | ◐ 对比 |
| deepseek-fr-2api (`deepseek-fr-2api/`) | Python | 逆向 **deepseek.de/.es/.fr** 站点 | 欧洲全角站点(含 Turnstile), 非官网 | ✗ 旁支, 仅参考模型命名 |
| one-api / inference-gateway / cursor-deepseek | Go | 官方 API 通道 | 走官方 deepseek API(需付费 key/企业账户) | ✗ 与本需求无关 |

> 结论: 三家"网页逆向"实现之间协议细节有出入(见 §9), 以 chat.deepseek.com 实测为准。

---

## 2. 总体架构与固定请求头

```
浏览器(登录) ──→ user_token/账号密码
                    │
Aurora/代理 ── POST /api/v0/chat_session/create ─────→ chat.deepseek.com
            │ POST /api/v0/chat/create_pow_challenge ─→ {algorithm,challenge,salt,expire_at,difficulty,signature,target_path}
            │ POST /api/v0/chat/completion (SSE) ─────→ p/o/v JSON-Patch 流(带回 x-ds-pow-response)
            │ POST /api/v0/chat/stop_stream / chat_session/delete
```

### 2.1 固定请求头(多仓库一致)

```http
Host: chat.deepseek.com
User-Agent: DeepSeek/<ver> Android/<api>     # 各仓库: 1.8.0 / 2.0.0 / 2.1.1
Accept: application/json
Content-Type: application/json
Authorization: Bearer <user_token>           # 浏览器 cookie "user_token" 的值, 非 JWT
x-client-platform: android                   # 或 web
x-client-version: 2.0.0                      # 2.0.0 才解锁 expert 模型等
x-client-locale: zh_CN                       # 或 en_US
accept-charset: UTF-8
```

### 2.2 响应信封与错误

```
{ "code": 0, "msg": "", "data": { "biz_code": 0, "biz_msg": "", "biz_data": {...} } }
```

- `code != 0` → 会话/认证错误
- `biz_code != 0` → 业务错误(必须同时检查)
- 已知业务错误码: `1001`/`1201` = 限流/过载; `40301` = `INVALID_POW_RESPONSE`

### 2.3 WAF(重要)

- CloudFront 拦截时返回 **202** + `x-amzn-waf-action` 头
- **美区出口 IP 必中 WAF** → 服务端必须支持非美区代理(ds-free-api 实测结论)
- 排查顺序: 若一直拿 403/202, 先看代理出口地区

---

## 3. 会话生命周期

| 端点 | Body | 返回路径 |
|---|---|---|
| `POST /api/v0/chat_session/create` | `{}`(zyj 有 `{character_id: null}` 变体) | 新:`data.biz_data.chat_session.id`; 老: `data.biz_data.id` |
| `POST /api/v0/chat_session/delete` | `{chat_session_id}` | 代理负责兜底清理 |
| `POST /api/v0/chat/stop_stream` | `{chat_session_id}` | 中断生成 |
| 其他 | `update_title`、`edit_message`(需 messfee, ds-free+) | — |

> 代理侧策略: **一条对话 = 一个 session + 一次 completion 请求**; 续轮 = 同一 session 换 `parent_message_id` 串链。会话结束后务必 delete(防账号后台堆积会话)。

---

## 4. PoW(DeepSeekHashV1)

来源: masterzerno `pow.go`(纯 Go + 标准库, 可直接搬) + zyj `pow.ts`(WASM) + ds-free 的 wasm URL。

**流程**:
1. `POST /api/v0/chat/create_pow_challenge` body `{target_path: "/api/v0/chat/completion"}` → 得到 `{algorithm, challenge, salt, expire_at, difficulty, signature, target_path}`
2. 前缀 = `salt + "_" + expire_at + "_"`; 在 `[0, difficulty)` 内搜 nonce, 使 `DeepSeekHashV1(challenge + 前缀 + 十进制 nonce)` 的 hash 与 challenge 命中(相等)
3. `difficulty` 为 0 时按 144000 处理(老实现)
4. 算法本质: 对标准 SHA3-256 的 Keccak-f 只做 **round 1..23(跳过第 0 轮)**, rate=136
5. 结果头部: `x-ds-pow-response` = Base64(标准 JSON `{algorithm, challenge, salt, answer, signature, target_path}`)
6. 辅助: WASM 地址 `https://fe-static.deepseek.com/chat/static/sha3_wasm_bg.7b9ca65ddd.wasm`(版本会变, 供对照)

**分歧点(需验证)**: 旧 Python 实现是 `challenge + salt + nonce` 直拼, 与 Go/TS 的 `salt_expireAt_` 前缀不一致 → 以实测为准。另:**部分账号/区域免 PoW**(challenge 为空时直接放行, Go 版支持)。

---

## 5. Chat Completion

### 5.1 请求体(各实现 5 种变体)

```jsonc
{
  "chat_session_id": 123456,          // 会话 id
  "parent_message_id": null,          // 首轮 null; 续轮 = 上轮响应 message id
  "prompt": "...",                    // 拍平后的字符串(多轮全部拼入, 无服务端历史)
  "ref_file_ids": [],
  // 差异项:
  "model_type": "default",            // 仅 ds-free 使用(对应 default/expert/vision)
  "thinking_enabled": true,          // zyj/ds-free
  "search_enabled": true,            // zyj/ds-free
  "preempt": false,                  // 仅 ds-free
  "stream": true,                    // zyj 用
  "client_stream_id": "..."          // 旧 Python 版
}
```

> `prompt` 是**拍平后的单一字符串**: 多轮历史、工具结果全部拼入(服务端无历史记忆, 每轮全量提交)。

### 5.2 SSE 响应流(两种格式并存, 2026 以 V4 为准)

帧格式: `\n\n` 分隔; 每帧含 `event:` 行 + `data:` 行(p/o/v JSON-Patch)。

| 事件 | 含义 |
|---|---|
| `ready` + `{request_message_id, response_message_id}` | 请求被接受, 记录 ID(续轮/停止/统计用) |
| `hint` + `{content, finish_reason: ...}` | 错误/限流(`rate_limit`, `input_exceeds_limit`) |
| `update_session` | 会话状态刷新 |
| `close` | 流关闭 |
| `[DONE]` | 结束(部分实现) |

**p/o/v 补丁三字段**: `p` = JSON 指针路径, `o` = 操作(REPLACE/APPEND/REMOVE...), `v` = 新值。

- **V4 初始快照**: 无 `p`, `v.response.fragments[]` 数组 `{type: THINK|RESPONSE, content}`
- **V4 增量**: `p: "response/fragments"` + `o: "APPEND"` + `v: [[fragment]]` 追加; 然后 `p: "response/fragments/-1/content"`(或空路径)`v` 字符串 → 当前活跃 fragment 的 delta
- **V3 兼容**: `p: "response/thinking_content"` → 思考块; `p: "response/content"` → 正文; `p: "response/status"` 等
- **结束**: `p: "response/status"` + `v: "FINISHED"`(或 `"INCOMPLETE"`); `accumulated_token_usage` → usage
- 搜索模式下跳过 `[citation: ...]` 前缀片段

**状态机实现要点**(ds-free 的 response.rs 是最完整参考):
1. 持久化 `current_path` + `op`, 分配各 fragment 类型
2. 存量 + 增量组装, 避免重复拼接初始快照
3. 分块续写: 若单轮超 token, 用 `parent_message_id` 延续而不丢历史

---

## 6. 响应流(SSE)解析 — 纵览

见上方 5.2, 补充:

- **V4 与 V3 同时解析**: 老前端只发 V3 格式, 新前端发 V4 → 解析器要做**格式自适应**(zyj 的 parse SSE 即同时覆盖两种)
- 帧头 `event:` 与 `data:` 分开解析; `data: [DONE]` 与 `event: close` 都视为流结束
- **异常处理**: 服务端若中途断流(无 FINISHED 也无 close), 探测超时后按半截输出处理并上报

---

## 7. 工具调用与语言注入

### 7.1 模型原生标签(参考官方 added_tokens)

> 以下 token 多属 Unicode 私用区(PUA)字符, Markdown 中无法原样显示; 完整字符可查 `ds-free-api/docs/deepseek-prompt-injection.md` 的 added_tokens 表。按用途描述:

| 标记(描述) | 用途 |
|---|---|
| 思维链 token(think 开启/关闭) | CoT 容器, 推理模型在最终回答前输出内部思考 |
| User 角色 token / Assistant 角色 token | 角色锚点, 替代 User:/Assistant: 文本前缀, 防角色混淆注入 |
| fill-in-the-middle 的 begin / end / hole | 代码中间补全 |
| End-of-Turn token(潜在 `终结符替代`) | 标记本轮结束, 模型停止生成信号之一 |
| 工具调用列表容器 / 单个工具容器 / 工具结果列表容器 / 单个结果容器 / 工具分隔符 | 工具调用与回填的结构化包装 |

### 7.2 网页端实测结论(ds-free-api/docs/deepseek-prompt-injection.md)

- 网页后端会**过滤**大部分原生 PUA 标记: 实测能被后端正常使用的仅有"思维链 token、角色锚点 token"三类
- 系统提示: 用 `<|System|>` 形式的**全角**类标签注入(妥协方案)
- **工具调用主标签**: `<|tool▁calls▁begin|>` / `<|tool▁calls▁end|>`(ASCII `|` 与下划线 `_`), 模型遵循度好、幻觉少
- 全角变体(`｜`U+FF5C、`▁`U+2581)与 ASCII 等价 → 解析端做内置模糊匹配, 覆盖大多数字符级幻觉
- **回退列表**: `<|tool_call_begin|>`、`<tool_calls>`、`<tool_call>` 等作为 format 变体, 增量维护
- **reminder 注入**: 在末尾留一个**未闭合**的思维链标签, 把系统指令/工具说明插进去, 模型先思考再作答, 遵循度更强

### 7.3 注入顺序(ds-free prompt.rs)

```
[System(含 reminder)] [历史 user/assistant 轮次] ... 末尾 未闭合思维链[reminder] + assistant 终结符(供 split 拆分点)
```

- 工具定义/调用指令 → 注入 System 尾部
- 优先级: 格式规范 > 工具定义 > 调用指令
- `response_format` 降级: JSON 约束注入 reminder 块

---

## 8. 模型名映射

| 类型 | 入参 model | 备注 |
|---|---|---|
| V4 时代(zyj) | `deepseek-v4-pro` / `deepseek-v4-flash` | + `-thinking`(≈reasoner)、+ `-search` |
| V3 兼容 | `deepseek-chat` / `deepseek-reasoner` | 别名 `deepseek-v3`、`deepseek-r1`、`*-search` |
| ds-free 特有 | `deepseek-default` / `deepseek-expert` / `deepseek-vision` | 由 config `model_types` 生成 |

- thinking 开关 ≈ `reasoning_effort` 非 `none` → `thinking_enabled`
- search 默认开启(后端注入强系统提示); 显式 `web_search_options` 可覆盖
- **ds-free 模型表默认**: `["default","expert","vision"]`, max_input_token 1048576, max_output 384000, input_char_limit 2621440

---

## 9. ★ 官网验证清单(编码前必须逐项实测)

| # | 验证项 | 现状分歧 | 验证方式 |
|---|---|---|---|
| 1 | `model_type` 字段 | 仅 ds-free 使用, 其余不传 | DevTools 抓网页真实请求对比 |
| 2 | 当前模型表(V4-flash/pro? thinking/search 标志? 免费用户可见?) | zyj 列 v4; ds-free 用 default/expert/vision | 网页 `/models` 或抓包 |
| 3 | ` thinking` 标签是否被后端过滤; 只有 ` thinking`/` response`/` user(assistant)` 能通过? | 注入文档实测 | 发测试消息观察是否过滤 |
| 4 | PoW 前缀 `salt_expireAt_` vs `challenge+salt+nonce` 直拼 | Go/TS 一致, Python 旧版不同 | 用真实 challenge 复算对比 |
| 5 | 客户端版本号(2.0.0? expert 解锁?) | 各仓不同 | 对比不同 header 的响应 |
| 6 | session create 响应 `chat_session.id` vs 老 `id` | 各仓不同 | 实测 |
| 7 | 是否必等 `event: ready` 后才发 body | ds-free 默认等 | 实测 |
| 8 | 中止用 `stop_stream` 还是 delete session | 各仓不同 | 实测 |

---

## 10. Aurora 接入设计方向(验证后细化)

待官网验证通过后, 按 **token 文件注入池** 形态实施:

### 10.1 认证(已确认)

- 形态: **token 文件注入池** + **可丢弃小号池**(2026-08-09 与用户确认)
- `deepseek_tokens.txt`(每行一个 user_token, 风格对齐现有 `access_tokens.txt`)
- **池内只放可牺牲的小号**(新注册/临时邮箱号), 主邮箱/主账号**永不入池**; 被封即丢、可随时扩
- 注册源: gmail `+tag` 子账号、临时邮箱; 邮箱注册需全局代理(见 §11)
- 多 token 轮询/故障转移; 客户端侧可选透传自有 token
- 无需存密码(风险更低)

### 10.2 协议实现模块

| 模块 | 方案 |
|---|---|
| PoW | 移植 masterzerno `pow.go`(标准库, 无外部依赖); challenge 为空直接放行; 加 WAF 代理位 |
| SSE | 按 ds-free 状态机: current_path/op + V3/V4 双解析; FINISHED 收尾 |
| Prompt 拼装 | 拍平 + `角色` 标签 + 工具注入(§7) |
| 工具调用 | 复用现有 `internal/toolcall`, 替换注入标签为 `<|tool▁calls▁begin|>` 系 |
| 模型映射 | 接入现有 conversion 模型表, 追加 `deepseek-v4-*` 等 |
| 会话 | 一对话一 session + delete 兜底 |

### 10.3 配置

- `proxy` 位(非美区, 绕 WAF, §11 证据 #38)
- 超时/重试/限流错误重试(40101/1201)
- **反风控硬性要求**(§11 结论落成配置):
  - 并行度上限约束(参考 账号数/2, 官方口径)
  - 每账号独立出口(IP/代理)绑定, 不共用出口快速轮换
  - 失败熔断 + 自动换号重试; 池账号健康状态 存 `Error` 可弃
  - 会话用完即删(不驻留)+ 单会话短生命周期
  - 账号文件明确标注"可丢弃池", 容量/配额上限可配
- 大历史: 先不做文件上传回退(后续 ds-free 方案)

---

## 附: 落地后的待办(编码后)

1. `internal/` 新增 deepseek provider + 配置文件
2. 真机实测完整链(CREATE → POW → COMPLETION → DELETE)
3. 补 SSE 状态机单测(V4 快照/append/delta)
4. 工具调用端到端(Aurora 工具 → 注入标签 → 结果回填)
5. client_stream_id/ds-free 的高级透传项按需取舍

---

## 11. 封号风险与对策(社区实测)

> 依据: ds-free-api repo issues(#1~#101) 及评论区, 抓取于 2026-08-09。**封号是结构性风险, 不是 bug**: 绕过官方协议用官方网页账号做 API 调用, 任何实现在"完全防封"上都无能为力, 能做的是**降低触发概率 + 把损失隔离在可丢弃账号上**。

### 11.1 风险画像(issue 编号证据)

| 风险 | 现象 / 社区证据 |
|---|---|
| 认证不限号 | 一张封/见光死: #101「老号几分钟就封」#97「一用就封」#83「还没开始就结束」#94「7个号全部暂时封禁, 创几个杀几个」 |
| 解封规律 | #94 用户实测: 多为 **24h 临时风控自动解封**(规则未知) |
| 请求特征触发 | #98 推测: 每次请求新建会话+全量上下文、工具标签过强, 都可能是触发点; "复制一次完整请求发给 ds 就被封" |
| WAF 大规模挂 | #38 全账号 "error decoding response body"(CloudFront 202 拦截)→ 需非美代理 |
| 专家模式 | 网页端已取消专家模式的文件上传 #68; #94 怀疑 0.2.7-pre1 专家兼容被针对 |
| 域名风控 | #12: DeepSeek **封注册域名**, 勿用自己主域名注册多号 |
| 账号级限流 | #4: 高并发下 Session Pool 无限扩张 → 账号级限流 |
| 注册限制 | 国内网络/手机号只能手机注册; 邮箱注册需全局(非国内)代理 #12/#62 |

### 11.2 对策(社区 + 维护者共识)

1. **账号分层(核心)**: 主号永不进池; 池里只放可牺牲小号。维护者自用号长时间不受影响(#71 作者原话"自用账号目前没问题")→ **低频自用是安全区, 高频并发才是雷区**。
2. **注册来源**: gmail `+tag` 子账号(一个邮箱无限子账号) #12、临时邮箱(emailtick 等) #84; 用**非主域名**注册。
3. **行为仿真**:
   - 新号先人工网页登录/简单对话几次, 增加"人味"(#94 用户仅存案例均为"手动网页登录对话过")
   - 避免把能一眼识别为程序调用的完整请求体发回官方(有用户因此被封 #98)
   - 会话用完即删, 不驻留
4. **并发控制**: 并行数 ≈ 账号数/2(官方 README 口径); 限流检测 + 指数退避重试; Session Pool 设上限(#4)
5. **WAF/出口**: 非美区代理; 每账号独立出口, 避免多账号共享 IP 快速轮换
6. **运维动作**: 启动/定期健康检查池账号, 命中 Error 即弃; 面板支持手动重登

### 11.3 对 Aurora 适配器的硬性要求(已写入 §10.3)

- 账号池 = 可丢弃号池, 配容量/配额上限与并行度上限
- 每账号独立出口代理、失败熔断、自动换号重试
- 会话即删不驻留; 不复制完整请求特征
- 池健康状态机(Error 可弃) + 开机自检

**结论: web 逆向通道定位为"有风险的备用通道", 商用/重要流量走 DeepSeek 官方 API(便宜), 不要把宝押在网页通道上。**