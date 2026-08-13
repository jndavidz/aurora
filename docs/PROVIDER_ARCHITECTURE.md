# aurora 多 Provider 架构探索报告(新增 Provider 落点速查)

> 来源:子智能体对 `D:\repos\aurora` 的多 Provider 架构探索(2026-08-13)。
> 本文是**实现级速查**:精确到文件与行号的接线点、每个新 Provider 要改哪些文件、怎么抄。
> 总览性架构见 `docs/ARCHITECTURE.md`,网页协议实测见 `docs/DEEPSEEK.md` / `docs/GLM.md`。

---

## 一、`internal/provider/` 目录与整体架构

| 文件 | 职责 |
|---|---|
| `provider.go` | Provider 接口 + Registry 注册表 + Capability/Model 类型定义 |
| `deepseek.go` | DeepSeek provider 入口(模型解析、chat/coding 变体路由) |
| `deepseek_chat.go` | DeepSeek chat 变体(Responses + ChatCompletions 双表面) |
| `deepseek_coding.go` | DeepSeek coding 变体(文本协议工具调用) |
| `glm.go` | GLM(智谱清言)provider 入口 |
| `glm_chat.go` | GLM chat 变体 |
| `glm_coding.go` | GLM coding 变体 |
| `glm_test.go` | GLM 单测(模型解析/消息转换/工具剥离) |
| `kimi.go` | Kimi(www.kimi.com)provider 入口 |
| `kimi_chat.go` | Kimi chat 变体(拍平历史进单条 message) |
| `kimi_coding.go` | Kimi coding 变体(工具上下文注入 + 原生工具透传) |
| `kimi_test.go` | Kimi 单测(模型解析/拍平/工具剥离) |
| `input.go` | 双接口共享的输入拍平助手(`responsesInputItems`、`apiMessagesToItems`、图片上传) |
| `sse.go` | Responses 流式事件输出器(`sseWriter`)与各事件构造器 |
| `provider_test.go` | DeepSeek/Registry/双接口共享核心测试 |

> 目录约定:`internal/provider/<name>.go` 实现 Provider 接口;`internal/<name>web/`
> 放网页逆向客户端(如 `deepseekweb/`、`glmweb/`)。

## 二、Provider 接口与注册表 — `internal/provider/provider.go`

每个 provider 实现 4 个方法:

```go
type Provider interface {
    Name() string                                      // 上游标识,如 "deepseek" / "zhipu"
    Models() []Model                                   // 喂给 /v1/models(含能力标注)
    Handles(model string) bool                         // 精确匹配模型 id
    Responses(c *gin.Context, req *official.ResponsesAPIRequest)
    ChatCompletions(c *gin.Context, req *official.APIRequest)
}
```

Registry(`Register()`/`Resolve()`/`Models()`)语义:

- **后注册的优先级更高**(slice 顺序遍历)。
- `Resolve(model)` 返回第一个 `Handles()` 命中的 provider;未命中返回 nil → 走 **ChatGPT 兜底**。
- **ChatGPT 不实现本接口**,是 handler 内的默认兜底路径(脆弱、依赖账号池/指纹/sentinel,不强行抽象)。

## 三、GLM 新文件的结构 — 三层模板(`internal/provider/glm.go`)

1. **模型目录解析**(L36-75):`defaultGlmModels` 默认 4 个 id(`glm-5.2-chat/coding`、`glm-5-chat/coding`);
   `parseGlmModel` 按后缀解析 —— `-chat` → chat 变体(`CapReasoning/CapWebSearch/CapVision`),
   `-coding` → coding 变体(`CapFunctionCall/CapReasoning`);**无 `glm-` 前缀保护返回 nil**
   (防 `gpt-5-chat` 这类 id 误命中)。
2. **懒加载网页客户端**(L87-92):`webClient()` 首次调用时 `glmweb.NewClient(cfg.GlmWebBase, cfg.GlmWebTokens, ...)`。
3. **变体路由**(L95-120):`Responses`/`ChatCompletions` 按 `m.Variant` 分发到 `chatResponses`/`codingResponses`
   (定义在 `glm_chat.go`/`glm_coding.go`)。与 DeepSeek 的 `deepseek.go`(L114-146)完全同构。

GLM 特有:`Glm` struct 多一个 `lastToken` 字段(L32),配合 `glm_chat.go` 的 `ensureToken`(L41-60)——
refresh_token 池轮换换发 JWT 失败时避免死循环。

### 每个变体 4 个函数的标准骨架(以 GLM chat 为例,`glm_chat.go`)

| 函数 | 职责 | 位置 |
|---|---|---|
| `chatResponses` | Responses 流式入口:ensureToken → 构建 messages → Complete | L20-38 |
| `chatResponsesStream` | SSE 事件流输出(`sseWriter` 事件序) | L90-132 |
| `chatResponsesNonStream` | 整包 JSON | L135-153 |
| `chatCompletions` | ChatCompletions 入口 | L156-173 |
| `chatCompletionsStream` / `chatCompletionsNonStream` | `data:` 分块 + `[DONE]` | L194-254 |

> 两个 provider 的 chat 变体对同名方法做了不同实现(`chatResponses` 等),分别挂在
> `*DeepSeek` 和 `*Glm` receiver 上,靠 receiver 类型区分,不冲突。

## 四、Provider 注册/选择方式(无 models.json)

**没有 `models.json` 或任何 JSON 配置文件**。模型目录由环境变量(`DEEPSEEK_MODELS` / `GLM_MODELS` CSV)驱动,
默认值硬编码在 provider 里。**唯一注册点是 `internal/handler/router.go` L26-32**:

```go
registry := provider.NewRegistry()
if cfg.DeepSeekWebTokens != "" {
    registry.Register(provider.NewDeepSeek(cfg))
}
if cfg.GlmWebTokens != "" {
    registry.Register(provider.NewGlm(cfg))
}
```

### 新增一个 provider 需要改动的文件清单

1. `internal/provider/provider.go` — 无需改动(接口已就绪,除非加新能力枚举)
2. `internal/provider/xxx.go` + `xxx_chat.go` + `xxx_coding.go` — 新 provider 本体
3. `internal/xxxweb/` — 新网页逆向客户端包(协议层)
4. `internal/config/config.go` — Config 加字段 + `Load()` 加 env 读取
5. `internal/handler/router.go` — 条件注册
6. `internal/handler/models_handler.go` — 无需改(自动聚合 `registry.Models()`)
7. `env.template` + `docker-compose.nas.yml` — 环境变量约定
8. 若工具标签不同:`internal/toolcall/tags.go` 加标签变体正则

Token 文件路径约定在 `internal/bootstrap/bootstrap.go` L21-29:`tokenFilePath()` 优先
`./.runtime/tokens/`(docker-compose.nas.yml 挂载到 `/work/.runtime/tokens:ro`),目录不存在时回退当前目录。

## 五、网页逆向类 Provider 的共同模式

### 5.1 Token 来源

- **DeepSeek**:`DEEPSEEK_WEB_TOKENS` 文件每行一个 `localStorage["userToken"].value`
  (不透明 token,文档 `docs/DEEPSEEK.md` §五 明确不是 cookie)。`deepseekweb.NewClient` 读文件进
  `tokens` 池,`NextToken()` 轮询取号。
- **GLM**:`GLM_WEB_TOKENS` 每行一个 `chatglm_refresh_token`(长期凭据),用 refresh_token 换发
  access_token(JWT,约 2 小时)——见 `internal/glmweb/client.go` L109-146 `RefreshAccessToken()`
  (POST `/chatglm/user-api/user/refresh`),换发成功后 completion 用 JWT 做 `Authorization: Bearer`。
- **Qwen 千问**:`QIANWEN_WEB_TOKENS` 每行一个**完整 cookie header**(`tongyi_sso_ticket` 做账号凭据,
  约 1 年;WAF 升级后还需 `x5sec` 通关 cookie,约 20 分钟,浏览器过滑块签发)。无需换发。
  协议见 `docs/QIANWEN.md`。
- **Kimi**:`KIMI_WEB_TOKENS` 每行一个 `localStorage["refresh_token"]`(JWT,约 90 天,刷新时轮换),
  用 refresh_token 换发 access_token(JWT,仅 15 分钟)——见 `internal/kimiweb/client.go`
  `RefreshAccessToken()`(POST `auth.kimi.com/api/account.gateway.v1.AuthService/RefreshToken`,
  返回 `{accessToken, refreshToken}` 两者都轮换);completion 用 JWT 做 `Authorization: Bearer`。
  账号身份头(x-msh-device-id/session-id/traffic-id)从 refresh_token 的 JWT claims 解出。
  **无签名、无 PoW、无必选 shield**,标准库 http.Client 即可(实测无 WAF/指纹风控)。
  协议见 `docs/KIMI.md`。

> **TLS 指纹(JA3)风控是网页逆向的通用门槛**:千问 WAF 按客户端 TLS 指纹风控,
> Go 标准库 `http.Client`/curl 直连请求量一上来就 captcha,必须用 Chrome 指纹客户端
> (`httpclient/bogdanfinn` 的 tls-client + `profiles.Chrome_146`),并显式声明
> `Accept-Encoding: gzip` 手动解压(tls-client 无透明解压)。新 provider 直接抄 qianwenweb 的模式。

### 5.2 请求体格式

- **DeepSeek**(`internal/deepseekweb/client.go` L200-251):`POST /api/v0/chat/completion`,
  拍平单字符串 `prompt`(无服务端历史,每轮全量提交),`chat_session_id`、`parent_message_id`(u32 整数)、
  `model_type`(default/expert/vision)、`ref_file_ids`、`thinking_enabled`、`search_enabled`。
  **请求前必须先 solve PoW**(`create_pow_challenge` → DeepSeekHashV1 23 轮 Keccak,`pow.go`),
  否则 40300;请求头带 `x-client-platform: web` + Chrome UA。
- **GLM**(`internal/glmweb/client.go` L193-236):`POST /chatglm/backend-api/assistant/stream`,
  结构化 `messages` 数组 + 服务端会话(`conversation_id` 续轮),`meta_data.chat_mode`="thinking"、
  `is_networking` 开关。**所有请求带签名头**(`sign.go`:X-Timestamp 混淆 + X-Nonce + X-Sign = MD5,
  固定 key 硬编码 L22)。
- **Kimi**(`internal/kimiweb/client.go`):`POST www.kimi.com/apiv2/kimi.gateway.chat.v1.ChatService/Chat`,
  **Connect 协议**(请求体帧 = `flags(1)+长度(4BE)+JSON`,Content-Type `application/connect+json`)。
  **只收单条 `message`(singular),不认 `messages` 数组** → 上下文靠服务端 chat_id 会话,
  或把全量历史**拍平进单条 message 文本**(aurora 采用,保持无状态)。快速模式
  `scenario:"SCENARIO_K2D5"`、`options.reasoning_effort:"REASONING_EFFORT_LOW"`。

### 5.3 流式响应解析(SSE)

两个 web 包各有一个 `ConsumeStream(r, onDelta(Delta))`,归一化为 `Delta{Text, Reasoning}` 增量,
provider 层再转成 OpenAI 事件:

- **DeepSeek**(`deepseekweb/stream.go` L31-74):`event:`/`data:` 分行,p/o/v JSON-Patch 流;
  支持 V4 fragments(THINK/RESPONSE)与 V3(response/content)双格式;`event: close` 或 `data: [DONE]` 收尾。
- **GLM**(`glmweb/stream.go` L53-123):每帧 `data:` 是完整 JSON,parts **全量重发**(非增量),
  用 `strings.TrimPrefix` 与上一帧做差值。
- **Kimi**(`kimiweb/stream.go`):Connect 服务端流,**按字节读长度前缀帧**(非 SSE 行),
  帧内 JSON 为增量状态同步 op:`op:set/append` + `mask:block.think(.content)` / `block.text(.content)`;
  原生工具调用 `block.tool`(PENDING 起帧 → `block.tool.args` append 拼参数 → RUNNING 定稿整单上报);
  `flags=2` 收尾帧结束,夹 `{"heartbeat":{}}` 心跳。真正增量流,直接拼接即可。

Provider 层输出两种表面:

- **Responses**:`sse.go` 的 `sseWriter.event()` 输出
  `response.created → output_item.added → reasoning_text.delta/output_text.delta → output_item.done → response.completed`
  (见 `glm_chat.go` L90-132)。
- **ChatCompletions**:手写 `data: {chunk}\n\n` + `[DONE]`(见 `glm_chat.go` L194-233)。

### 5.4 工具调用处理(文本协议模拟,非原生)

核心在 `internal/toolcall/`:

- `prompt.go` `BuildInstructionsWithTags(tags, tools, toolChoice)`:把工具注入 system prompt,
  教导模型输出 `<tool_call>{"name":...,"arguments":{...}}</tool_call>` 标签块。
- `parser.go` `Parser.Feed/Flush`:流式状态机,从 chunk 流切出 `(textDelta, toolCalls)`,
  对标签变体归一化(`tags.go` `NormalizeTagged`,覆盖 `<tool_call>`、`<|tool_calls_begin|>`、U+2581 ▁ 变体),
  含 robust JSON 修复(Windows 路径反斜杠)。
- `recover.go` `RecoverFromText`:兜底扫无标签的裸 JSON(markdown 围栏),`mergeRecoveredCalls`
  (deepseek_coding.go L130-144)按 name+arguments 去重合并;`StreamToToolCallDeltas` 把完整调用切成
  `{index,id,name}` + `{index,arguments}` 两段 delta。
- `params.go` `InferToolFromParams`:"参数直给"格式按 schema 推断工具名。
- 各 provider 用不同的 `TagSet`:DeepSeek 用 `<|tool▁calls▁begin|>`(deepseek_coding.go L149-154),
  GLM coding 目前复用 `toolcall.DefaultTags`(glm_coding.go L91 注释明确"智谱原生 tool_calls 结构未抓到,先走文本协议")。

**Chat/Coding 双变体的硬规则**:chat 变体即使客户端带 tools 也**绝不注入任何工具信息**
(`glm_chat.go` L27 `stripTools=true`,单测 `TestGlmMessagesFromResponsesNoToolInjection` 断言),coding 变体才注入。

## 六、router.go 分发 + Nightmare 的历史 bug

- `POST /v1/chat/completions` → `chatHandler.Nightmare`(L67)
- `POST /v1/responses` 和 `POST /v1/models/responses` → `chatHandler.Responses`(L68-70,后者是 pi 适配器别名)

分发逻辑在 `internal/handler/chat_handler.go`:

- `Nightmare` L68-73:命中 `providers.Resolve(model)` 即 `p.ChatCompletions(c, ...)` 短路,否则走 ChatGPT 账号池。
- `Responses` L290-295:同样的分派到 `p.Responses(c, ...)`。

> 历史 bug:"Nightmare 没有 Provider 分发"(/v1/chat/completions 的 DeepSeek 请求之前永远回 ChatGPT,
> 只有 Responses 有分派)。**已在 commit `edf0025`("fix(deepseek): /v1/chat/completions 也走 Provider")修复**,
> 当前源码两处都有分派。若部署版本早于 edf0025,那才是 bug 版本。

## 七、配置相关

### `internal/config/config.go`

- Config 结构体 L9-42:通用字段 + `DeepSeekWebBase/DeepSeekWebTokens/DeepSeekModels/DeepSeekProxy`(L31-35)
  + `GlmWebBase/GlmWebTokens/GlmModels/GlmProxy`(L37-41)。
- `Load()` L44-77:每个 provider 一组 4 个 env;`DEEPSEEK_MODELS`/`GLM_MODELS` 用 `splitCSV()`(L80-91)解析逗号列表。

### `env.template`

- DeepSeek 段 L54-70:`DEEPSEEK_MODELS`、`DEEPSEEK_WEB_TOKENS`、`DEEPSEEK_WEB_BASE`、`DEEPSEEK_PROXY`。
- GLM 段 L72-88:`GLM_MODELS`(默认 `glm-5.2-chat, glm-5.2-coding, glm-5-chat, glm-5-coding`)、
  `GLM_WEB_TOKENS`、`GLM_WEB_BASE`、`GLM_PROXY`。

### `docker-compose.nas.yml`

L28-40:`GLM_WEB_TOKENS=/work/.runtime/tokens/glm_tokens.txt`(只读挂载卷
`/volume2/docker/aurora/tokens`),`DEEPSEEK_WEB_TOKENS` 同目录。

### 文档

- `docs/ARCHITECTURE.md` — 多 Provider 架构总览(含接线点表格 §六)
- `docs/DEEPSEEK.md` — DeepSeek 接入实测结论(认证/PoW/SSE/识图/配置)
- `docs/deepseek网页协议整理.md` — 逆向抓包原始资料
- `docs/CDP_BROWSER_DEBUG.md` — 浏览器抓包 token 的方法论(与 `browser-cdp` skill 思路一致)

## 八、新增逆向 Provider(如 Qwen 千问)落点速查

1. **新包 `internal/xxxweb/`**:`client.go`(token 池 `loadTokens` + 轮询 `NextToken` + 鉴权头/签名)
   + `stream.go`(`ConsumeStream` 归一化 `Delta{Text, Reasoning}`)。GLM 的 `sign.go` 是"签名逆向"的模板;
   DeepSeek 的 `pow.go` 是"挑战应答"的模板。
2. **`internal/provider/xxx.go`**:实现 `Name/Models/Handles/Responses/ChatCompletions`,
   模型 id 用 `-chat`/`-coding` 后缀路由(照抄 `glm.go` L61-75 的 `parseGlmModel`,带前缀保护)。
3. **`xxx_chat.go` / `xxx_coding.go`**:各 2 个入口 + 4 个输出函数;prompt 构建复用 `input.go` 的
   `responsesInputItems`/`apiMessagesToItems`,工具调用复用 `internal/toolcall`(只需给 `TagSet` 换标签)。
4. **注册**:`router.go` 条件注册 + `config.go` 4 个 env + `env.template` + `docker-compose.nas.yml` 挂载 token 文件。
5. **自检**:`/v1/models` 聚合 + provider 单测(照抄 `glm_test.go` 的模型解析/消息转换/工具剥离断言模式)。

---

> 注:GLM 相关改动(glm*.go、glmweb/)当前为未提交工作区状态;本报告行号以该状态为准。
