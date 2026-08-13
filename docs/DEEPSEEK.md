# DeepSeek 接入(chat.deepseek.com 网页逆向)

> 更新时间: 2026-08-13
> 关联: `docs/deepseek网页协议整理.md`(逆向资料,实施前参考)、`docs/ARCHITECTURE.md`(多 Provider 架构)、
> `docs/NAS_DEPLOYMENT.md`(部署)。

---

## 一、状态

**架构已完成,协议字段待官网实测(P0)**。代码中标注 `[P0]` 的位置均依据逆向资料(deepseek网页协议整理.md §9 验证清单)实现,需抓包逐项确认后才算可用:

| # | 待验证项 | 位置 | 影响 |
|---|---|---|---|
| 1 | `model_type` 枚举(default/expert/vision?) | `internal/provider/deepseek_chat.go:modelTypeFor` | chat 快速/专家模式 |
| 2 | `search_enabled` / `thinking_enabled` 字段与取值 | `internal/provider/deepseek_chat.go` | 联网搜索/深度思考 |
| 3 | 识图图片上传端点与 `ref_file_ids` | `internal/provider/input.go:uploadImages`(当前返回空) | 识图不可用 |
| 4 | PoW(`create_pow_challenge` + `x-ds-pow-response`,DeepSeekHashV1) | `internal/deepseekweb/client.go:solvePow`(当前占位空) | 需 PoW 的账号/区域 403 |
| 5 | session create 响应字段(`chat_session.id` vs `id`) | `client.go:CreateSession` | 会话创建 |
| 6 | SSE V4 fragments 结构(THINK/RESPONSE、APPEND 双层数组) | `internal/deepseekweb/stream.go` | 流式解析 |
| 7 | 是否必等 `event: ready` | `client.go:Complete` | 时序 |
| 8 | 中止用 `stop_stream` 还是 delete session | `client.go:StopStream`(占位) | 会话清理 |

> **未验证前不要上生产**。验证方式:浏览器 DevTools 抓 chat.deepseek.com 真实请求,对照本文档逐项确认。

## 二、模型目录(配置驱动)

`DEEPSEEK_MODELS`(逗号分隔)控制暴露的模型 id;不配置时用默认目录。
每个 id 按后缀路由到 chat / coding 变体:

| exposed id | 变体 | 通道 | 能力 |
|---|---|---|---|
| `deepseek-v4-flash-chat` | chat | 网页·快速 | 对话 + 智能搜索 + 识图,无工具 |
| `deepseek-v4-pro-chat` | chat | 网页·专家 | 对话 + 深度思考,无搜索/识图/工具 |
| `deepseek-v4-flash-coding` | coding | 网页 | 文本协议工具调用 |
| `deepseek-v4-pro-coding` | coding | 网页 | 文本协议工具调用(重推理) |

命名规则:
- `-chat` 后缀 → chat 变体;内含 `flash` → 快速模式(web_search + vision + reasoning),否则专家模式(reasoning)。
- `-coding` 后缀 → coding 变体(function_call + reasoning)。

## 三、chat 变体硬规则

**chat 变体上游只发「真人对话」形态的请求** —— 剥离客户端 `tools`/`tool_choice`,
不注入 `<|tool_calls_begin|>` 或任何工具说明,只携带网页模式开关(快速/专家、智能搜索、深度思考、识图)。

- 这些开关是**真人点网页 UI 会产生的行为**,不暴露给大模型的"工具"。
- 多轮历史用 `User:`/`Assistant:` 角色前缀拍平为单一 `prompt`(网页无服务端历史,每轮全量提交)。
- `function_call`/`function_call_output` item 防御性跳过(chat 变体不该出现)。
- **识图(快速模式)与联网搜索互斥**:有图片时不带 `search_enabled`(DeepSeek 网页行为)。

## 四、coding 变体

复用 `internal/toolcall` 文本协议机制(ChatGPT `<tool_call>` 同源),仅标签不同:

- 标签:`<|tool_calls_begin|>{...}<|tool_calls_end|>` 系(DeepSeek 网页 §7.2;全角变体模糊匹配待 P0 实测补)。
- 工具提示词:`toolcall.BuildInstructionsWithTags(deepseekTagSet(), ...)`。
- 流式:`toolcall.NewParserWithTags` 解析 → `function_call` item / `output_text` delta。
- 非流式:`RecoverFromText` + `StripTags`。

## 五、配置

```bash
# 网页 token 注入池文件路径(每行一个 user_token;只放可丢弃小号,主号永不入池)
DEEPSEEK_WEB_TOKENS=/path/to/deepseek_tokens.txt

# 暴露的模型目录(逗号分隔;不配置用默认 4 个)
DEEPSEEK_MODELS=deepseek-v4-flash-chat,deepseek-v4-pro-chat,deepseek-v4-flash-coding,deepseek-v4-pro-coding

# 网页端 base(默认 https://chat.deepseek.com)
DEEPSEEK_WEB_BASE=https://chat.deepseek.com

# 网页通道出口代理(非美区,绕 WAF;参考协议整理 §2.3)
DEEPSEEK_PROXY=http://proxy:port
```

> 仅当 `DEEPSEEK_WEB_TOKENS` 非空时 DeepSeek provider 才会注册(`/v1/models` 不广告无 token 的模型)。

## 六、封号风险(结构性,不可消除,只能降低)

依据 `deepseek网页协议整理.md` §11(社区实测):
- 认证不限号、一用就封是常态;多为 24h 临时风控自动解封。
- **池内只放可丢弃小号**(gmail `+tag` 子账号、临时邮箱;勿用主域名注册),主号永不入池。
- 行为仿真:新号先人工网页登录/简单对话几次;会话用完即删、不驻留。
- 并发控制:并行数 ≈ 账号数/2;限流检测 + 指数退避重试。
- 非美区出口代理;每账号独立出口,避免多账号共享 IP 快速轮换。

## 七、对外接口

- **`/v1/responses`**(主):DeepSeek provider 处理 chat / coding 双变体。
- **`/v1/models`**:DeepSeek 模型带 `owned_by: "deepseek"` + `capabilities` 标注。
- **`/v1/chat/completions`**:不接 DeepSeek(仅 ChatGPT)。
- **别名路由**:`POST /v1/models/responses` → 同一个 `Responses` handler(pi 的 responses 适配器实际路径)。

## 八、联调冒烟清单

```bash
# 1. chat 变体普通对话(流式)
curl -N -X POST http://10.10.10.2:65432/v1/responses \
  -H "Authorization: Bearer david" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash-chat","input":"你好","stream":true}'

# 2. chat 变体联网搜索(快速模式)
curl -N -X POST http://10.10.10.2:65432/v1/responses \
  -H "Authorization: Bearer david" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash-chat","input":"今天比特币价格?","stream":true}'

# 3. coding 变体工具调用(带 tools)
curl -N -X POST http://10.10.10.2:65432/v1/responses \
  -H "Authorization: Bearer david" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash-coding","input":"用 bash 列出当前目录","tools":[{"type":"function","function":{"name":"bash","description":"run shell","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}],"stream":true}'

# 4. /v1/models 应包含 deepseek-* 且带 capabilities
curl -s -H "Authorization: Bearer david" http://10.10.10.2:65432/v1/models
```
