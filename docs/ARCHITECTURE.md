# aurora 多 Provider 架构(Responses 统一表面)

> 更新时间: 2026-08-13
> 关联: `docs/DEEPSEEK.md`(DeepSeek 接入)、`docs/GLM.md`(智谱清言接入)、`docs/GROK.md`(Grok 接入)、`docs/GEMINI.md`(Gemini 接入)、`docs/NAS_DEPLOYMENT.md`(部署)。

---

## 一、定位

aurora 是「网页端 → OpenAI 兼容 API」网关。对外提供 **两个 OpenAI 兼容表面**:

- **`/v1/chat/completions`**:主流客户端(测试 HTML、zcode、多数 agent)使用
- **`/v1/responses`**:pi 等 Responses 客户端使用

两个表面都同时服务 **ChatGPT**、**DeepSeek**、**智谱(GLm)** 与 **Grok**(都是网页逆向),共享同一套 Provider 逻辑 ——
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
        │                                            └─ 共享核心:toMessages → buildPrompt → session/PoW/SSE
        │                                               ↓ 两薄适配器输出
        └─ 否(含 "auto"、gpt-*)──▶ ChatGPT 网页逆向路径(原逻辑,默认兜底)
```

## 二、为什么统一到 Responses

1. **联网搜索只在 Responses**:DeepSeek/Qwen/OpenAI 的官方接口都只在 Responses 的 `web_search` 内置工具里有;
   chat completions 没有。
2. **DeepSeek 网页逆向(chat.deepseek.com)是自研通道**,按 Responses 语义落地,后续官方 API(若接)可直接直通。
3. reasoning / function_call / web_search_call 都是 Responses 的第一类 item,对 coding agent 更干净。

**多轮 = 客户端无状态**:每轮客户端全量重发 `input` 数组(历史 message + function_call +
function_call_output items)。**不依赖 `previous_response_id`/`store`**(DeepSeek 不支持,aurora 也无需)。
网页通道每轮把完整 `input` 拍平成网页 prompt 发上游。`maxHistoryChars`(100000)预检保留,防长历史膨胀。

## 三、Provider 抽象(`internal/provider`)

```go
type Capability string // "web_search" | "reasoning" | "vision" | "function_call"
type Model struct { ID, OwnedBy string; Caps []Capability }
type Provider interface {
    Name() string
    Models() []Model                                    // 喂给 /v1/models(含能力标注)
    Handles(model string) bool                          // 精确匹配模型 id
    Responses(c *gin.Context, req *official.ResponsesAPIRequest)
}
type Registry struct{ ... }                             // Register / Resolve / Models
```

- **ChatGPT 不实现 Provider 接口** —— 它是 handler 默认兜底(脆弱、依赖账号池/指纹/sentinel,
  强行抽象风险大收益低)。
- 新上游(DeepSeek、后续 Qwen)实现该接口,在 `router.go` 注册。
- 每个 Provider 内部再按模型 id 拆 **chat / coding 两个变体**。

## 四、chat / coding 变体

**每个 provider 的每个模型,用后缀路由到两种形态之一:**

| 变体 | 定位 | 行为 |
|---|---|---|
| `-chat` | 对话为主(模仿网页真人) | **绝不注入工具调用信息**:剥离客户端 `tools`/`tool_choice`,只发真人对话形态的请求 + 网页模式开关(快速/专家、智能搜索、深度思考、识图) |
| `-coding` | coding agent 为辅(工具调用) | 文本协议工具调用:把 tools 注入提示词,引导模型输出工具标签块,解析成 Responses 的 `function_call` item |

- ChatGPT 标签:`<tool_call>{...}</tool_call>`(现有 local-toolfix 机制,已参数化到 `internal/toolcall.TagSet`)。
- DeepSeek 标签:`<|tool_calls_begin|>...</|tool_calls_end|>` 系(网页实测待定,见 DEEPSEEK.md)。
- `-chat` 请求即便客户端带了 tools,上游也**不含任何工具信息**(单测 `TestFlattenChatInputNoToolInjection` 断言)。

## 五、Responses 工具协议(ChatGPT coding 迁入)

原 local-toolfix 的 `<tool_call>` 工具调用从 `/v1/chat/completions` 迁到 `/v1/responses`:

- `ChatHandler.responsesToolCalling`:带 tools 的 Responses 请求 → converter 注入工具提示词 →
  上游 SSE → `toolcall.Parser` 流式解析 → 输出 `response.output_item.added/function_call_arguments.delta/done/output_item.done`。
- 非流式:`RecoverFromText` 全量解析 → `ResponsesResponse.Output[]` 追加 `function_call` item。
- 流式事件对齐官方序:`response.created → output_item.added → (output_text.delta | function_call_arguments.delta/done) → output_item.done → response.completed`。

## 六、接线点

| 文件 | 改动 |
|---|---|
| `internal/provider/provider.go` | Provider 接口 + Registry |
| `internal/provider/deepseek*.go` | DeepSeek provider(chat/coding 双变体) |
| `internal/deepseekweb/` | DeepSeek 网页协议客户端(会话/PoW/SSE 双解析/识图) |
| `internal/provider/glm*.go` | 智谱 provider(chat/coding 双变体) |
| `internal/glmweb/` | 智谱网页协议客户端(签名/refresh/SSE 原生 tool_calls) |
| `internal/provider/grok*.go` | Grok provider(chat/coding 双变体,WS 协议) |
| `internal/grokweb/` | Grok 网页协议客户端(WebSocket + Realtime 事件流) |
| `internal/provider/gemini*.go` | Gemini provider(chat/coding 双变体) |
| `internal/geminweb/` | Gemini 网页协议客户端(StreamGenerate + RPC 帧解析 + 严格限频) |
| `internal/toolcall/fence.go` | FenceParser 围栏 JSON 拦截(智谱/Grok coding 用) |
| `internal/handler/chat_handler.go` | `Responses` 入口 provider 分派 + ChatGPT responses 工具调用 |
| `internal/handler/router.go` | Registry 构建注册 + 别名路由 `POST /v1/models/responses`(pi 适配器实际路径) |
| `internal/handler/models_handler.go` | 聚合 ChatGPT 硬编码列表 + provider 模型(含能力标注) |
| `internal/config/config.go` | DeepSeek / 智谱 / Grok 环境变量(`DEEPSEEK_WEB_*`、`GLM_WEB_*`、`GROK_COOKIES`、`GROK_MODELS`) |
| `internal/toolcall/` | 标签参数化(`TagSet`,ChatGPT / DeepSeek 双标签)+ FenceParser |
| `typings/official/` | Responses 请求/响应/事件扩展(tools、function_call item、function_call_arguments 事件) |

## 七、能力如实标注

`/v1/models` 里每个 provider 模型带 `capabilities` 数组(web_search / reasoning / vision / function_call / sandbox_code)。
用途:让客户端与文档知道**哪些能力是官方直通、哪些是网页降级**——例如 ChatGPT 网页逆向的
联网搜索受账号/套餐限制,DeepSeek 快速模式识图与联网搜索互斥,智谱/Grok coding 变体的工具调用
因模型自带沙箱而不可靠(标 `sandbox_code` 而非 `function_call`),均如实标注,避免客户端误用。

## 八、已明确不做 / 后续

- **官方 API 通道**(api.deepseek.com):当前全网页逆向;官方 API 作为可选「可靠性通道」后续再评估。
- Qwen(通义千问)等新 provider:复用同一 Provider 接口即可。
- 网页逆向是**结构性封号风险**:token 只放可丢弃小号、主号永不入池、会话用完即删、
  并发≈账号数/2、非美区出口。
