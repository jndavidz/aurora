# aurora 多 Provider 架构(全网页逆向 + Responses 统一表面)

> 更新时间: 2026-08-14
> 关联文档: `docs/DEEPSEEK.md`、`docs/GLM.md`、`docs/GROK.md`、`docs/GEMINI.md`、
> `docs/KIMI.md`、`docs/DOUBAO.md`、`docs/QIANWEN.md`、`docs/NAS_DEPLOYMENT.md`、
> `docs/CDP_BROWSER_DEBUG.md`(抓包方法)。

---

## 一、定位

aurora 是「网页端 → OpenAI 兼容 API」网关。对外提供 **两个 OpenAI 兼容表面**:

- **`/v1/chat/completions`**:主流客户端(测试 HTML、zcode、多数 agent)使用
- **`/v1/responses`**:pi 等 Responses 客户端使用

两个表面都同时服务 **ChatGPT**、**DeepSeek**、**智谱 (GLM)**、**Grok**、**Kimi**、
**Gemini**、**豆包**、**千问 (Qwen)**(都是网页逆向),共享同一套 Provider 逻辑——
Provider 接口含 `Responses()` 与 `ChatCompletions()` 两个方法,内部收敛为
**同一核心**(输入统一成 messages → 拍平 prompt → session/PoW/SSE → delta 流),
差异只在输入解析与输出格式(两个薄适配器)。

```
OpenAI 兼容客户端(pi / zcode / aurora-chat.html / codebuddy)
        │  /v1/chat/completions 或 /v1/responses
        ▼
   ChatHandler(Nightmare / Responses)
        │
        ├─ model ∈ provider 注册表? ── 是 ──▶ Provider.ChatCompletions / .Responses
        │                                            │
        │                                            └─ 共享核心:toMessages → buildPrompt → session/SSE
        │                                               ↓ 两薄适配器输出
        └─ 否(含 "auto"、gpt-*)──▶ ChatGPT 网页逆向路径(默认兜底)
```

## 二、为什么统一到 Responses

1. **联网搜索只在 Responses**:DeepSeek/Qwen/OpenAI 的官方接口都只在 Responses 的
   `web_search` 内置工具里有;chat completions 没有。
2. **DeepSeek 网页逆向 (chat.deepseek.com) 是自研通道**,按 Responses 语义落地。
3. **reasoning / function_call / web_search_call 都是 Responses 的第一类 item**,
   对 coding agent 更干净。

**多轮 = 客户端无状态**:每轮客户端全量重发 `input` 数组(历史 message + function_call +
function_call_output items)。**不依赖 `previous_response_id`/`store`**(DeepSeek 不支持,
aurora 也无需)。网页通道每轮把完整 `input` 拍平成网页 prompt 发上游。`maxHistoryChars`
(100000)预检保留,防长历史膨胀。

## 三、Provider 抽象(`internal/provider`)

### 接口定义

```go
type Capability string // "web_search" | "reasoning" | "vision" | "function_call" | "sandbox_code"
type Model struct { ID, OwnedBy string; Caps []Capability }
type Provider interface {
    Name() string
    Models() []Model                                    // 喂给 /v1/models(含能力标注)
    Handles(model string) bool                          // 精确匹配模型 id
    Responses(c *gin.Context, req *official.ResponsesAPIRequest)
    ChatCompletions(c *gin.Context, req *official.APIRequest)
}
```

### Registry

```go
type Registry struct{ ... }
func (r *Registry) Register(p Provider)   // 后注册的优先级更高
func (r *Registry) Resolve(model string) Provider  // 返回第一个 Handles() 命中的 provider
func (r *Registry) Models() []Model       // 聚合所有 provider 的模型(含能力标注)
```

- **ChatGPT 不实现 Provider 接口**——它是 handler 默认兜底(脆弱、依赖账号池/指纹/sentinel,
  强行抽象风险大收益低)。
- 新 provider 实现该接口,在 `router.go` 条件注册(仅其 token 池文件非空时注册)。
- 每个 Provider 内部再按模型 id 拆 **chat / coding 两个变体**(见 §四)。

### 注册条件

```go
// internal/handler/router.go
registry := provider.NewRegistry()
if cfg.DeepSeekWebTokens != ""  { registry.Register(provider.NewDeepSeek(cfg)) }
if cfg.GlmWebTokens != ""       { registry.Register(provider.NewGlm(cfg)) }
if cfg.KimiWebTokens != ""      { registry.Register(provider.NewKimi(cfg)) }
if cfg.DoubaoAccounts != ""     { registry.Register(provider.NewDoubao(cfg)) }
if cfg.QianwenWebTokens != ""   { registry.Register(provider.NewQianwen(cfg)) }
if cfg.GrokCookies != ""        { registry.Register(provider.NewGrok(cfg)) }
if cfg.GeminiCDPURL != ""       { registry.Register(provider.NewGeminiCDP(cfg)) } // CDP 桥转发
```

注册后不可注销。N/A 的 provider 不注册即可。

### 分发逻辑

`Nightmare`(chat/completions)和 `Responses`(/v1/responses)入口处同一逻辑:

```go
if h.providers != nil {
    if p := h.providers.Resolve(original_request.Model); p != nil {
        p.ChatCompletions(c, &original_request)  // 或 p.Responses(...)
        return
    }
}
// 未命中 → ChatGPT 兜底
```

> 历史 bug:最初 Nightmare 没有 Provider 分发(/v1/chat/completions 的 DeepSeek 请求
> 之前永远回 ChatGPT,只有 Responses 有分派)。已在 commit `edf0025` 修复。

## 四、Chat / Coding 变体

**每个 provider 的每个模型,用后缀路由到两种形态之一:**

| 变体 | 定位 | 行为 |
|---|---|---|
| `-chat` | 对话为主(模仿网页真人) | **绝不注入工具调用信息**:剥离客户端 `tools`/`tool_choice`,只发真人对话形态的请求 + 网页模式开关(快速/专家、智能搜索、深度思考、识图) |
| `-coding` | coding agent 为辅(工具调用) | 文本协议工具调用:把 tools 注入提示词,引导模型输出工具标签块,解析成 Responses 的 `function_call` item |

**ChatGPT 例外**:ChatGPT 不注册为 provider,其 coding 机制在 handler 内实现——**同一模型 id + 请求带 `tools` 即自动进工具通道**(见 §6.1)。此外 ChatGPT 有 `gpt-5-6-coding` 变体(强制工具调用模式,无 tools 报 400)。

**前缀保护**:各 provider 的 `parseXxxModel` 函数检查前缀(如 `glm-`、`kimi-`、`grok-`),
防 `gpt-5-chat` 这类 id 误命中。

**能力标注**:
- `-chat`:标注 `web_search`、`reasoning`、`vision`(按实际)
- `-coding`:标注 `function_call`(文本协议模拟)或 `sandbox_code`(智谱/Grok 云端沙箱)

## 五、工具调用(文本协议模拟)

### 5.1 核心机制

所有网页逆向 provider 的**上游都不认识结构化 `tools` 字段**。工具调用是纯文本协议模拟:

1. **converter 注入**:`conversion/requests/chatgpt/convert.go` 把客户端 `tools` 数组渲染成
   system prompt + `<tool_call>{JSON}</tool_call>` 标签教学 + 末尾 `FinalNudge` 用户消息。
2. **上游视为普通文本**:模型回复中包含 `<tool_call>` 标签块。
3. **解析器**:`internal/toolcall/` 包:
   - `parser.go` `Parser.Feed/Flush`:流式状态机,从 chunk 流切出 `(textDelta, toolCalls)`。
   - `recover.go` `RecoverFromText`:兜底扫无标签的裸 JSON(markdown 围栏)。
   - `tags.go` `DefaultTags`: `<tool_call>`/`</tool_call>`(ChatGPT);`NormalizeTagged`
     归一化 DeepSeek 的 `<|tool▁calls▁begin|>`(U+2581)等变体。
   - `fence.go` `FenceParser`:智谱 `&#8203;````json` 围栏 JSON 工具调用。
   - `prompt.go` `BuildInstructions` / `BuildInstructionsWithTags`:工具渲染。
   - `params.go` `InferToolFromParams`:"参数直给"格式推断工具名。
4. **输出**:`StreamToToolCallDeltas` 切分 name 段 + arguments 段 delta。

### 5.2 ChatGPT 共享收集器 `toolCallingRetry`(commit 8c694e3)

ChatGPT 的 coding 变体不注册为 provider,在 handler 内实现,且与 chat/completions
共用同一套重试/兜底逻辑:

```
handleToolCalling(chat/completions) ──┐
                                       ├── toolCallingRetry(共享收集器)
responsesToolCalling(/v1/responses) ──┘
```

`toolCallingRetry` 包含:
- **历史大小预检** 413(免费模型超大请求静默空回复)
- **REFUSAL_RETRIES 重试循环** + SYSTEM OVERRIDE 逼重试(1→2→4→8s 退避)
- **拒绝/停顿/环境推诿分类器** (`looksLikeSandboxRefusal` / `looksLikeRequestingUserContent` /
  `looksLikePrematureStop` / `looksLikeEnvironmentExcuse`)
- **`RecoverFromText` 兜底** + **空回复检测**(哑火,连续 2 次停手)
- **502 兜底**(`no_valid_reply`)

**历史教训**:responses 工具调用曾无重试(`responsesToolCallingStream` 只跑一轮),
模型偶发绕开工具直接纯文本回答即"偶发不触发"。已删除该实现,统一走 `toolCallingRetry`。

### 5.3 协议输出

- **chat/completions 流式**:`writeToolCallingStream` → 标准 OpenAI SSE(`tool_calls` delta)
- **chat/completions 非流式**:`NewChatCompletionWithToolCalls`
- **responses 流式**:`response.created` → `output_item.added(message)` → `output_text.delta` →
  `output_item.done` → `function_call` 事件序列 → `response.completed` → `[DONE]`
- **responses 非流式**:`NewResponsesResponse` 带 `function_call` items

### 5.4 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `TOOL_CALLING_ENABLED` | true | 工具调用总开关(需登录账号) |
| `REFUSAL_RETRIES` | 3 | 重试次数(pi 场景建议 1,ZCode 建议 5) |
| `DEBUG_TOOL_LOG` | — | 工具调用调试日志路径 |

## 六、各 Provider 详情

### 6.1 ChatGPT(默认兜底)

**不注册为 provider**,是 handler 内默认路径。模型 id 原样透传上游(空→`auto`)。

**模型目录**(`internal/handler/models_handler.go`,2026-08-14 抓包 `/backend-api/models` 更新):
`auto`, `gpt-5-5`, `gpt-5-6`, `gpt-5-5-mini`, `gpt-5-6-mini`, `gpt-5-6-coding`(function_call 标注)。

> ⚠️ **slug 不等于实际运行模型**:`/backend-api/models` 返回的 slug 是 UI 选择器标识,
> 实际运行什么模型取决于账号 tier。**免费账号下所有 slug 实际都运行 GPT-5.5-mini**
> (实测 `gpt-5-6`、`gpt-5-6-mini`、`gpt-5-5` 都自称 GPT-5.5-mini)。
> slug 的 title("GPT-5.6 Luna" 等)是营销显示名,不代表模型真实版本。
> Plus/Pro 账号可能运行更高版本,待实测。

**coding 变体**(`gpt-5-6-coding`):

**coding 变体**(`gpt-5-6-coding`):
- `normalizeCodingModel`:`-coding` 后缀 → 改写 `gpt-5-6` 透传上游(真实 slug),响应回显 `-coding` id
- 无 tools → 400 `missing_tools`;带 tools → 强制工具调用(走 `toolCallingRetry`)
- 白名单 `chatgptCodingBases`:只含 `gpt-5-6`

**认证**:`access_tokens.txt`(每行一个 JWT,浏览器 localStorage 提取)。

**限制**:免费版有周/小时限额,超限返回 "You've hit your limit"(非代码问题)。

### 6.2 DeepSeek(`internal/deepseekweb/` + `internal/provider/deepseek*.go`)

**模型**:`deepseek-v4-flash-chat/pro-chat` + `-flash-coding/-pro-coding`。

**协议要点**:
- **PoW 工作量证明**:`internal/deepseekweb/pow.go`,每次 completion 前需解 challenge。
- **SSE 双格式**:V3(`response/content`、`response/thinking_content`)与
  V4(`fragments[]` type:THINK/RESPONSE),`ConsumeStream` 自动适配。
- **搜索**:`search_enabled:true` 开关,结果含 `SEARCH` fragment + `[citation:N]` 引用标记。
  aurora 使用 `citationStripper`(流式跨帧安全)自动剥离 `[citation:N]` 标记。
- **识图**:上传文件 → `ref_file_ids` → 强制 `model_type:vision`。
- **智能搜索**:chat 变体 `search_enabled:true`;coding 变体关搜索。

**引用标记剥离**(commit a409ed3):`internal/deepseekweb/stream.go` 的 `citationStripper`,
采用正则 `\[\s*citation\s*:\s*[^\]]*\]`,流式跨帧安全(未闭合片段留 pending)。

**工具标签**:`<|tool▁calls▁begin|>`/`<|tool▁calls▁end|>`(U+2581 ▁,`deepseekTagSet`)。

**认证**:`deepseek_tokens.txt` 每行一个 `localStorage["userToken"].value`。

### 6.3 智谱 GLM(`internal/glmweb/` + `internal/provider/glm*.go`)

**模型**:`glm-5.2-chat`(快速挡,默认)/`glm-5.2-chat-thinking`(思考挡)/`glm-5.2-coding`。
`glm-5-chat`/`glm-5-coding` 已下线(parse 白名单只认 `glm-5.2-` 前缀)。

**协议要点**:
- **签名**:`X-Sign` / `X-Nonce` / `X-Timestamp` 头,`internal/glmweb/sign.go`。
- **`chat_mode`**:快速挡 `speed`(默认,无深度思考),思考挡 `thinking`(深度思考 + 联网搜索 + 识图)。
- **联网搜索**:`is_networking:true` 透传,但触发是**概率性**的(同一 query 有时搜索有时拒绝,
  上游模型行为,非代码问题)。
- **coding**:智谱模型无 function calling 训练,图标 `sandbox_code`(云端沙箱),
  客户端自定义工具调用是尽力而为。

**认证**:`glm_tokens.txt` 每行一个 `chatglm_refresh_token`(JWT,~90 天),
代码自动换发 access_token(JWT,~2 小时)。

### 6.4 Grok(`internal/grokweb/` + `internal/provider/grok*.go`)

**模型**:`grok-3-chat` / `grok-3-coding`。

**协议要点**:
- **WebSocket 实时协议**:`wss://grok.com/ws/mgw/?uid=...`,遵循 OpenAI Realtime 事件流
  (`session.create` → `conversation.item.create` → `response.create`)。
- **原生搜索+云端沙盒**:Grok 网页端自带搜索和沙盒工具,`-coding` 变体走原生工具透传,
  不能访问客户端本地文件。
- **`stream_error` 处理**(commit 96fb3b1):`response.grok.output` 事件含 `stream_error`
  (如 `usage_limit_reached`),此前被忽略→200 空正文,已修复为返回明确 502。

**认证**:`grok_cookies.txt` 每行 `uid|cookie`(含 `sso`/`sso-rw` 登录会话)。
账号有用量限制(`usage_limit_reached`),超限需浏览器重新抓 cookie。

### 6.5 Kimi(`internal/kimiweb/` + `internal/provider/kimi*.go`)

**模型**:`kimi-chat` / `kimi-coding`。

**协议要点**:
- **纯 Bearer 认证,无签名**。`access_token`(JWT,15 分钟) + `refresh_token`(JWT,90 天,轮换)。
  `refresh_token` 池模式,代码自动换发(`POST auth.kimi.com/.../RefreshToken`)。
- **Connect RPC**:`POST www.kimi.com/apiv2/.../ChatService/Chat`,unary 帧,
  快速模式 `scenario=SCENARIO_K2D5` + `reasoning_effort=LOW`。
- **协议特征**:单 message 拍平历史(网页只传单条用户消息,历史由服务端会话记忆),
  aurora 把全量 messages 拍平为一条 `user` 消息。
- **coding**:走原生工具透传(ipython),无自定义函数工具(模型 ToolType enum 无 FUNCTION)。

**引用标记剥离**:`internal/kimiweb/stream.go` 的 `citationStripper`(私有区字符标记,
`🛠`/`🎨`/``/``),流式跨帧安全。

**认证**:`kimi_tokens.txt` 每行一个 `refresh_token`(localStorage 提取)。

### 6.6 Gemini(`internal/geminweb/` + `internal/provider/gemini*.go`)

**模型**:`gemini-3-flash-chat`(CDP 桥通道;`-coding` 桥暂未实现)。

**当前通道 = CDP 桥转发**(commit 见 local-toolfix):
- 直连(`geminweb` + `gemini_accounts.json`)因数据中心 IP + 模拟指纹被 Google 风控
  (BardErrorInfo 1096/1157)已停用(commit ff4af80,router 中注释保留)。
- 现由家庭 PC 的 `scripts/cdp/bridge.mjs` 用真实浏览器页内 fetch 执行(零指纹模拟),
  aurora 配 `GEMINI_CDP_URL=http://<PC>:8799` 后注册 `GeminiCDP` provider
  (`internal/provider/gemini_cdp.go`),只做 OpenAI 兼容 HTTP 转发。
- 详情与按需启动/自动停止/令牌自愈见 `docs/GEMINI.md` §八。

**直连协议要点**(历史,若恢复直连需先按 §八"协议更新"修正):
- **StreamGenerate 端点**:`POST https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate`
  (RPC batchexecute)。
- **认证**:`at` 令牌 = `window.WIZ_global_data.SNlM0e`(base64url:时间戳,会话级固定);
  `SNlM6e` 大令牌(~2.6KB,会话级);`f.sid` 会话级。
- **严格限频**:代码内置单账号并发 1、间隔 ≥2s(防封号)。
- **`card_content` 过滤**(commit 3622496):剥离 `http://googleusercontent.com/card_content/` 链接。
- **coding**:走文本协议 `<tool_call>`,与 ChatGPT 同源。

**认证(直连)**:`gemini_accounts.json`(JSON 数组,含 cookie/at/snlM6e/fsid,会话级固定)。

### 6.7 豆包 Doubao(`internal/doubaoweb/` + `internal/provider/doubao*.go`)

**模型**:`doubao-chat`(仅 chat,`-coding` 已注释禁用,commit 84aa115)。

**协议要点**:
- **端点**:`POST https://www.doubao.com/chat/completion?<URL 参数>`。
- **认证**:URL 参数 `aid`/`device_id`/`fp`(风控指纹)/`msToken`/`a_bogus`(签名,分钟级短时效)/
  `web_id`/`tea_uuid`/`web_tab_id`。**无 Authorization/cookie 头**(cookie 自动带)。
- **正文坑**:增量在 `patch_object=111` 的 `tts_content`(`patch_object=1` 的 `text_block`
  只含首字,曾致文本截断)。完成 = `patch_object=50 ext.is_finish:"1"`。
- **`need_create_conversation` 恒 false**:豆包无法新建会话,空 conv 报 `STREAM_ERROR`。
- **多轮**:全量回放 messages 数组(仿 DeepSeek)。
- **`a_bogus` 短时效**:分钟级,需定期浏览器刷新。**定位低频备用通道,保活已被否决**(详见 docs/DOUBAO.md §四)。

**认证**:`doubao_accounts.json`(JSON 数组,含 cookie + URL 签名参数 + 会话 id)。

### 6.8 千问 Qwen(`internal/qianwenweb/` + `internal/provider/qianwen*.go`)

**模型**:`Qwen3.8-Max`(仅 chat,无 coding 变体,网页 API 不支持外部工具)。

**协议要点**:
- **Chrome TLS 指纹**:必须用 `bogdanfinn` `tlsClient` 模拟 Chrome_146(Go 标准库/curl 触发 JA3 风控)。
- **`x5sec` 通关 cookie**:约 20 分钟失效,需浏览器过滑块后签发,CDP 提取。
- **必带头**:`Accept` 必须显含 `text/event-stream`;`Origin`+`Referer` 必须匹配(否则验证码)。
- **WAF 限流**:短时间大量请求触发验证码墙,冷却期数分钟到几十分钟。
- **安全头**(`clt-acs-sign` 等)服务端不校验。
- 纯 chat 形态,无工具调用通道。

**认证**:`qianwen_tokens.txt` 每行一个**完整 cookie header**(含 `tongyi_sso_ticket` + `x5sec` + `x5sectag` + `sm_ruid` + `sm_uuid` + `JSESSIONID`)。

### 6.9 腾讯元宝(已关停)

`internal/yuanbaoweb/` + `internal/provider/yuanbao*.go` 代码保留但**已关停**。
2026-08-14 两个账号一天内先后被封(网页端对非浏览器调用极敏感),关闭原因:
- aurora 无状态会话(网页端是持久会话)
- 缺失浏览器头
- 每次消息自动联网搜索

恢复需:人形化改造或走官方 API。

## 七、网页逆向共同模式

### 7.1 Token 来源

| provider | 文件 | 格式 | 周期性 |
|---|---|---|---|
| ChatGPT | `access_tokens.txt` | 每行一个 JWT(浏览器 localStorage) | 会话级(几小时~天) |
| DeepSeek | `deepseek_tokens.txt` | 每行一个 `userToken` | 会话级 |
| GLM | `glm_tokens.txt` | 每行一个 `chatglm_refresh_token`(JWT,~90天) | 代码自动换发 |
| Kimi | `kimi_tokens.txt` | 每行一个 `refresh_token`(JWT,~90天) | 代码自动换发 |
| Grok | `grok_cookies.txt` | 每行 `uid\|cookie` | 会话级,账号用量限制 |
| Gemini | `gemini_accounts.json` | JSON 数组(cookie/at/SNlM6e/fsid) | 会话级 |
| Doubao | `doubao_accounts.json` | JSON 数组(cookie+URL 签名参数+会话) | `a_bogus` 分钟级 |
| Qianwen | `qianwen_tokens.txt` | 每行完整 cookie header | `x5sec`~20分钟 |

### 7.2 SSE 解析

| provider | 格式 | 解析位置 |
|---|---|---|
| ChatGPT | 标准 SSE(`data: {...}`) | `internal/chatgpt/handler_response.go` |
| DeepSeek | p/o/v JSON-Patch | `internal/deepseekweb/stream.go` |
| GLM | 标准 SSE | `internal/glmweb/stream.go` |
| Grok | WebSocket JSON 帧 | `internal/grokweb/client.go` |
| Kimi | length-prefixed 帧(flags+length+JSON) | `internal/kimiweb/stream.go` |
| Gemini | RPC response 帧 | `internal/geminweb/client.go` |
| Doubao | SSE(`event: STREAM_CHUNK`) | `internal/doubaoweb/client.go` |
| Qianwen | 标准 SSE | `internal/qianwenweb/stream.go` |

### 7.3 引用标记剥离

多 provider 在正文中嵌入引用标记(网页端渲染成引用卡片),aurora 统一剥离:

| provider | 标记格式 | 实现 |
|---|---|---|
| DeepSeek | `[citation:N]` | `deepseekweb/stream.go` `citationStripper`(commit a409ed3) |
| Kimi | 私有区字符 `🛠`/`🎨`/``/`` | `kimiweb/stream.go` `citationStripper` |
| Gemini | `http://googleusercontent.com/card_content/` | `geminweb/client.go` `sanitizeText`(commit 3622496) |

### 7.4 限频策略(chat 不限,coding 限)

用户拍板的全局策略:**chat 不限频**(真人使用,天然有人类节奏,限制只会拖慢真人);
**只对 coding 限频**(agent 连发工具调用是风控/封号主因)。
实现:`internal/provider/coding_limit.go` 的 `CodingLimiter` —— 全局串行,
间隔 = 基础 + 随机抖动,各 provider 的 coding 入口调用 `limiter.Wait()`:

| provider | coding 限频 | 备注 |
|---|---|---|
| ChatGPT | 2s + rand(0~2s) | `toolCallingRetry` 单入口(chat/responses 共享) |
| Gemini | 2s + rand(0~1.5s) | CDP 桥转发,限频在 aurora 侧;桥侧默认不限 |
| Grok | 2s + rand(0~1.5s) | 防 `usage_limit_reached` |
| DeepSeek | 1.5s + rand(0~1.5s) | — |
| GLM | 1.5s + rand(0~1.5s) | — |
| Kimi | 1.5s + rand(0~1.5s) | — |
| Doubao | — | coding 已禁用 |
| Qianwen | — | 无 coding 变体 |

> 单测:`internal/provider/coding_limit_test.go`(首调用不阻塞、间隔 >= base、抖动范围)。

## 八、接线点一览

| 文件 | 职责 |
|---|---|
| `internal/provider/provider.go` | Provider 接口 + Registry + Capability/Model 类型 |
| `internal/provider/*.go` | 各 provider 入口(模型解析、chat/coding 变体路由) |
| `internal/provider/*_chat.go` | chat 变体实现 |
| `internal/provider/*_coding.go` | coding 变体实现 |
| `internal/provider/input.go` | 双接口共享的输入拍平助手 |
| `internal/provider/sse.go` | Responses 流式事件输出器 |
| `internal/*web/` | 各网页逆向客户端包(协议层) |
| `internal/toolcall/` | 标签解析、recover、fence、prompt、params |
| `internal/handler/chat_handler.go` | `Nightmare`/`Responses` 入口 + provider 分发 + 工具调用路径 |
| `internal/handler/shared.go` | `toolCallingEnabled`、拒绝分类器、`sanitizeRefusalHistory` |
| `internal/handler/router.go` | Registry 构建 + 路由注册 |
| `internal/handler/models_handler.go` | ChatGPT 模型目录 + `normalizeCodingModel` |
| `internal/config/config.go` | 各 provider 环境变量 |
| `internal/accounts/` | 账号池、能力(CapToolCalling/CapResponses) |
| `internal/bootstrap/bootstrap.go` | `tokenFilePath` 路径解析 |
| `conversion/requests/chatgpt/convert.go` | 工具指令提示词注入 |
| `typings/official/` | Responses 请求/响应/事件类型 |
| `env.template` | 环境变量模板 |
| `docker-compose.nas.yml` | NAS 部署 compose |

## 九、能力如实标注

`/v1/models` 里每个 provider 模型带 `capabilities` 数组:

| 能力 | 含义 | 适用 |
|---|---|---|
| `web_search` | 联网搜索 | chat 变体 |
| `reasoning` | 深度思考/思维链 | chat 变体、thinking 挡位 |
| `vision` | 识图/多模态图片理解 | chat 变体(部分) |
| `function_call` | 工具调用(文本协议模拟) | coding 变体 |
| `sandbox_code` | 云端沙箱代码执行 | 智谱/Grok coding 变体(模型自带沙箱,非客户端工具) |

如实标注,避免客户端误用。例如智谱 coding 标 `sandbox_code` 而非 `function_call`。

## 十、部署与配置

### 环境变量约定

每个 provider 一组 env 变量(`{PROVIDER}_WEB_*` 或 `{PROVIDER}_*`):

| provider | Token 文件 | 模型目录 | 其他 |
|---|---|---|---|
| ChatGPT | `access_tokens.txt`(路径固定,`tokenFilePath`) | 硬编码 `models_handler.go` | `TOOL_CALLING_ENABLED`、`REFUSAL_RETRIES` |
| DeepSeek | `DEEPSEEK_WEB_TOKENS` | `DEEPSEEK_MODELS` | `DEEPSEEK_WEB_BASE`、`DEEPSEEK_PROXY` |
| GLM | `GLM_WEB_TOKENS` | `GLM_MODELS` | `GLM_WEB_BASE`、`GLM_PROXY` |
| Kimi | `KIMI_WEB_TOKENS` | `KIMI_MODELS` | `KIMI_WEB_BASE`、`KIMI_PROXY` |
| Grok | `GROK_COOKIES` | `GROK_MODELS` | — |
| Gemini | `GEMINI_CDP_URL`(桥转发;直连 `GEMINI_ACCOUNTS` 已停用) | `GEMINI_MODELS` | `GEMINI_CDP_KEY`(可选,桥鉴权) |
| Doubao | `DOUBAO_ACCOUNTS`(JSON) | `DOUBAO_MODELS` | — |
| Qianwen | `QIANWEN_WEB_TOKENS` | `QIANWEN_MODELS` | `QIANWEN_WEB_BASE`、`QIANWEN_PROXY` |

### Token 文件路径

`internal/bootstrap/bootstrap.go` `tokenFilePath()`:
- 优先 `./.runtime/tokens/`(docker-compose.nas.yml 挂载 `/work/.runtime/tokens:ro`)
- 目录不存在时回退当前目录(扁平读取)

> 官方 `ghcr.io` 镜像不识别 `tokenFilePath`,只从 cwd 扁平读取——这是必须本地构建
> `local-toolfix` 镜像的原因。

### Docker 部署

- 镜像:NAS 本地构建 `aurora:local-toolfix`(Dockerfile 多阶段构建)
- 端口:宿主机 `65432:8080`
- 部署脚本:`scripts/deploy_nas.sh`(tar → ssh → 清空 → 解压 → `docker compose up -d --build` → curl 探活)
- 详细见 `docs/NAS_DEPLOYMENT.md`

## 十一、已知权衡与风险

### 结构性风险

- **网页逆向是结构性封号风险**:token 只放可丢弃小号、主号永不入池、会话用完即删、
  并发≈账号数/2、非美区出口。字节系(元宝前车之鉴)和 Google(Gemini)风控最强。
- **`a_bogus` 短时效**(豆包):分钟级,服务端无法自生成,需定期浏览器刷新。
  ==定位低频备用通道,不做保活。==
- **Grok 用量限制**:账号有 `usage_limit_reached`,需浏览器重新抓 cookie。
- **免费版限额**:ChatGPT 免费账号有周/小时限额,超限返回错误(非代码问题)。

### 各 provider 能力权衡

- **GLM 搜索概率性**:`is_networking:true` 已正确透传但模型有时拒绝搜索(上游行为)。
- **GLM coding 不可靠**:模型无 function calling 训练,工具调用不稳定(标 `sandbox_code`)。
- **Grok coding 只限云端**:原生沙盒/搜索工具,不能访问客户端本地文件。
- **Kimi coding 无自定义函数**:ToolType enum 无 FUNCTION,只有 `ipython`/`web_search`。
- **Doubao 无 coding**:已注释禁用,代码保留。
- **Qianwen 无 coding**:网页 API 不支持外部工具。

#### Coding(工具调用)能力推荐排名

| 排名 | provider | 推荐理由 | 限制 |
|---|---|---|---|
| 1🥇 | **ChatGPT**(gpt-5-6 / gpt-5-6-coding) | 文本协议 `<tool_call>` 最稳定,有完整重试机制(REFUSAL_RETRIES + SYSTEM OVERRIDE + 拒绝分类器 + RecoverFromText 兜底),实测 attempt 1 绕开→attempt 2 触发 | 免费版有周/小时限额 |
| 2🥈 | **DeepSeek**(deepseek-v4-flash-coding) | 文本协议 `<|tool▁calls▁begin|>` 可靠,有重试机制,PoW 认证,attention 已修复 | 无 |
| 3🥉 | **Grok**(grok-3-coding) | WebSocket 原生工具通道,能调云端沙盒搜索 | 不能访问本地文件;usage_limit 风险 |
| 4 | **Kimi**(kimi-coding) | 原生工具透传(ipython),指令直通性好 | 无自定义函数工具(ToolType enum 无 FUNCTION),客户端自定义工具折叠为文本 |
| 5 | **Gemini**(gemini-3-flash-coding) | 文本协议 `<tool_call>`,与 ChatGPT 同源 | 严格限频(2s/账号),Google 反爬最强,封号风险大 |
| 6 | **GLM**(glm-5.2-coding) | 云端沙箱代码执行(模型自带) | 无 function calling 训练,工具调用概率性(~1/3 成功),标 `sandbox_code` |
| — | **Doubao**(doubao-coding) | — | 已注释禁用 |
| — | **Qianwen**(Qwen3.8-Max-coding) | — | 网页 API 不支持外部工具,无 coding 变体 |

#### Chat(对话)能力推荐排名

| 排名 | provider | 推荐理由 | 限制 |
|---|---|---|---|
| 1🥇 | **ChatGPT**(gpt-5-6) | 账号 tier 下最高可用模型(免费版=GPT-5.5-mini),对话最自然,联网搜索+识图 | 免费版限额,超限降级;slug 不保证实际版本 |
| 2🥈 | **DeepSeek**(deepseek-v4-flash-chat) | 快速响应+智能搜索+识图,能力全面,免费限额宽松 | 无 |
| 3🥉 | **Kimi**(kimi-chat) | 快速模式(K2.6),联网搜索默认开启,refresh_token 90 天自动续期,维护成本最低 | 无 |
| 4 | **Grok**(grok-3-chat) | 原生搜索+云端沙盒,回复质量高 | usage_limit 需频繁抓 cookie |
| 5 | **Gemini**(gemini-3-flash-chat) | Google 模型能力强 | 严格限频(2s),封号风险高,只宜低频备用 |
| 6 | **GLM**(glm-5.2-chat) | 快速挡联网搜索+识图,两挡位可选 | 搜索概率性触发(上游行为),模型能力中等 |
| 7 | **Doubao**(doubao-chat) | 纯聊天,联网搜索自动 | a_bogus 分钟级失效,使用成本高(需频繁刷新),低频备用 |
| 8 | **Qianwen**(Qwen3.8-Max) | 纯 chat | 需 Chrome TLS 指纹 + x5sec 20 分钟刷新,维护成本高 |

### 已修复的历史问题

| 问题 | 修复 |
|---|---|
| `[citation:N]` 噪音 | `citationStripper`(a409ed3) |
| Grok `stream_error` 200 空正文 | `response.grok.output` 解析(96fb3b1) |
| Gemini `card_content` 链接 | `sanitizeText` 过滤(3622496) |
| responses 流式工具调用偶发不触发 | `toolCallingRetry` 共享收集器(8c694e3) |
| Nightmare 无 Provider 分发 | `edf0025` |
| Gemini card_content 链接泄漏 | 3622496 |
| ChatGPT 模型目录过时(13→6) | 抓包更新(a48c0c8) |
| GLM 搜索概率性| 文档声明为上游行为,非代码问题 |

### 后续方向

- **官方 API 通道**(如火山引擎 Doubao API、deepseek.com):当前全网页逆向;
  官方 API 作为可选可靠性通道后续再评估。
- 新 provider 复用同一 Provider 接口即可。