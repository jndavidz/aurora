# Claude(claude.ai)网页逆向接入 —— CDP 真浏览器执行通道

> 完成时间: 2026-08-14。与 Gemini 走同一套 CDP 桥架构
> (`scripts/cdp/bridge.mjs` + `keeper.mjs`),复用全部唤醒/熔断/限频/自愈机制。
> 详细架构说明见 `docs/GEMINI.md` §八;本文只记 Claude 特有部分。

## 一、对外模型

| id | 变体 | 能力 |
|---|---|---|
| `claude-sonnet-5-chat` | chat | 纯真人对话(网页实测模型 `claude-sonnet-5`,模型自动用原生 web_search/artifacts 等) |
| `claude-sonnet-5-coding` | coding | 客户端工具调用(围栏 JSON + FenceParser,与 Gemini coding 同机制,实测可用) |

仅当 `CLAUDE_CDP_URL` 配置时注册(默认复用 `GEMINI_CDP_URL`,同一桥服务)。

## 二、协议要点(2026-08-14 CDP 抓包 + 页内 fetch 实测)

- **发消息**:`POST https://claude.ai/api/organizations/{orgId}/chat_conversations/{convId}/completion`
  - **认证:纯 cookie**,无 Authorization、无会话令牌(比 Gemini 简单得多)
  - 请求体模板含 26 个前端内置工具(`read_me`/`show_widget`/`web_search`/`artifacts`/
    `repl` 等,原样保留,桥每轮只替换 `prompt` + `turn_message_uuids`)
  - `convId` 每轮新生成(uuid):多轮上下文靠全量拍平 prompt,不依赖服务端会话历史
  - 关键字段:`model: claude-sonnet-5`、`effort: medium`、`thinking_mode: auto`、
    `timezone`、`locale`
- **客户端头**(桥缓存并随请求自动更新):
  `anthropic-client-platform: web_claude_ai`、`anthropic-client-version: 1.0.0`、
  `anthropic-device-id`、`anthropic-anonymous-id`、`anthropic-client-sha`、`anthropic-client-build`
- **响应**:标准 Anthropic SSE —— `message_start` → `content_block_start/delta`
  (thinking 块 index 0、text 块 index 1)→ `message_stop`。
  正文增量 = `content_block_delta` 且 `delta.type == "text_delta"` 的 `delta.text`

## 三、组件

| 文件 | 职责 |
|---|---|
| `scripts/cdp/bridge.mjs` | claude 适配器(模板请求构造、SSE 解析、请求自捕获刷新模板) |
| `scripts/cdp/capture-claude.mjs` | 引导:抓 claude.ai 的 /api/ 请求,提取模板 + 客户端头 |
| `internal/provider/claude_cdp.go` | aurora 侧 `ClaudeCDP`(嵌入 `GeminiCDP` 复用转发/唤醒/熔断/限频) |
| 状态缓存 | `.runtime/bridge/claude_session.json`(gitignore 已排除) |

## 四、使用

1. 桌面快捷方式「Gemini小号浏览器」打开独立 profile 浏览器,登录 claude.ai(小号)
2. 首次引导:`node scripts/cdp/capture-claude.mjs 300` → 页面发一条消息 → 抓模板
3. 之后与 Gemini 同体验:NAS 单入口,`claude-sonnet-5-chat`/`-coding` 直接可用;
   空闲自动休眠、请求自动唤醒(见 GEMINI.md §八)

## 五、限频与限额

- **限频**:与全局策略一致 —— chat 不限(真人使用);coding 限频(2s + rand(0~1s),`claude_cdp.go`)。
- **限额(双窗口)**:免费账号 5 小时窗口 + 7 天窗口双限额,5h 约 40~45 条(动态计)。
  桥解析响应流里的 `message_limit` 事件实时监控:
  - `GET /health` 的 `providers.claude.limits` 显示 `{fiveHUtil, fiveHResetsAt, fiveHPercent}`
  - 5h 用量 >= 阈值(环境变量 `CLAUDE_LIMIT_WARN`,默认 0.8)时回复末尾附加
    `⚠️ Claude 5小时限额已用 X%,重置于 HH:MM`;设 0 = 每条回复都显示
- **原生工具可用性**:天气等原生工具(如 `weather_fetch`)偶发区域/临时故障
  (实测 2026-08-14 东京天气查询失败,页面端同样),与桥/链路无关。
