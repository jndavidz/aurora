# Context

aurora 是「网页端 → OpenAI 兼容 API」网关：对外提供两个 OpenAI 兼容表面（`/v1/chat/completions` 与 `/v1/responses`），对内把请求转发到各厂商的**网页逆向**通道。全量架构见 `docs/ARCHITECTURE.md`（权威）；本文只定义词汇表，供 agent 在 issue、重构提案、测试命名中统一用语。

## Language

<!-- 术语均取自代码与 docs/ARCHITECTURE.md 的既有用词；新增术语时保持与代码命名一致。 -->

**表面（surface）**:
对外暴露的两个 OpenAI 兼容端点：`/v1/chat/completions` 与 `/v1/responses`。
_Avoid_: API 模式、接口形态

**Provider**:
实现 `internal/provider.Provider` 接口的一个厂商通道（DeepSeek/GLM/Grok/Kimi/Gemini/豆包/千问/Minimax）。
_Avoid_: 后端、渠道

**兜底（fallback）**:
Registry `Resolve()` 未命中模型 id 时走 ChatGPT 网页逆向路径。ChatGPT **不是** Provider，是 handler 内默认兜底。
_Avoid_: 默认 provider

**chat 变体 / coding 变体**:
每个 provider 的每个模型按后缀路由到两种形态之一：`-chat`（模仿网页真人，**绝不注入工具调用信息**）、`-coding`（文本协议工具调用，供 coding agent）。代码中即 `*_chat.go` / `*_coding.go`。
_Avoid_: 对话模式/编程模式

**文本协议工具调用**:
网页上游不认识结构化 `tools`，工具调用靠注入提示词教学 + 解析 `<tool_call>` 标签块模拟（`internal/toolcall/`）。
_Avoid_: 原生 function calling（智谱/Grok 的 `sandbox_code` 也非客户端工具）

**拍平（flatten）**:
把客户端全量 messages/input 历史压成单条网页 prompt 发上游（多轮 = 客户端无状态，不依赖 `previous_response_id`）。
_Avoid_: 合并消息

**token 池**:
各 provider 的凭证文件集合（`*_tokens.txt` / `*_accounts.json`，见 `docs/ARCHITECTURE.md` §7.1）；**token 池文件非空**是该 provider 注册的前置条件（`router.go`）。
_Avoid_: 凭证库

**Capability（能力标注）**:
`/v1/models` 每个模型带的 `capabilities` 数组：`web_search` / `reasoning` / `vision` / `function_call` / `sandbox_code`。原则：**如实标注**。
_Avoid_: 特性、features

**coding 限频**:
全局拍板策略：chat 不限频，只对 coding 限频（agent 连发工具调用是风控主因）。实现 `internal/provider/coding_limit.go`。
_Avoid_: 限流

**CDP 桥**:
用真实浏览器页内 fetch 执行请求的转发通道（零指纹模拟），当前 Gemini 走此路径（`GEMINI_CDP_URL`）。方法论文档：`docs/CDP_BROWSER_DEBUG.md`。
_Avoid_: 浏览器代理

**引用标记剥离（citationStripper）**:
多 provider 在正文嵌入引用标记（`[citation:N]`、私有区字符等），aurora 流式跨帧安全地剥离。
_Avoid_: 清洗
