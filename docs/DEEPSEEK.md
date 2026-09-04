# DeepSeek 接入(chat.deepseek.com 网页逆向)

> 更新时间: 2026-08-13
> 关联: `docs/deepseek网页协议整理.md`(逆向资料,实施前参考)、`docs/ARCHITECTURE.md`(多 Provider 架构)、
> `docs/NAS_DEPLOYMENT.md`(部署)。

---

## 一、状态

**P0 网页协议验证已完成(2026-08-13,真实小号 + 浏览器抓包 + 端到端实测)。**
`deepseek网页协议整理.md` 的参考实现有多处过时,以下为**实测结论**(已落进代码):

| # | 验证项 | 实测结论(与文档差异) |
|---|---|---|
| 1 | 认证 | ✅ **`Authorization: Bearer <localStorage["userToken"].value>`**(不透明 token,会轮换);**不是**文档说的 `user_token` cookie。cookie 非必需 |
| 2 | 请求头 | ✅ `x-client-platform: web`、`x-client-version: 2.3.0`(文档写 android/2.0.0 已过时);需 Chrome UA + Origin + Referer(过 WAF) |
| 3 | PoW | ✅ **必选**(缺 `x-ds-pow-response` → 40300 MISSING_HEADER)。`DeepSeekHashV1` = **23 轮 Keccak-f[1600]**(跳过第 0 轮),rate=136(SHA3-256 海绵);解 `H(salt_expireAt_nonce)==challenge`;difficulty 144000。已过官方测试向量 |
| 4 | session | ✅ `POST /chat_session/create` body `{}`;响应 `data.biz_data.chat_session.id`;用完 delete |
| 5 | completion | ✅ body:`{chat_session_id, parent_message_id(整数或 null), model_type(null|"default"|"vision"), prompt, ref_file_ids, thinking_enabled, search_enabled, action:null, preempt:false}`;**无 stream 字段**;`parent_message_id` 必须是整数(字符串报 422) |
| 6 | SSE | ✅ V4:`event: ready`(message id 是整数)→ 初始快照 `{"v":{"response":{"fragments":[{type:RESPONSE|THINK,content}]}}}` → 增量 `{"v":"文本"}`(纯 v 字符串)或 `{"p":"response/fragments/-1/content","o":"APPEND","v":"文本"}` → 结束 `{"p":"response/status","o":"SET","v":"FINISHED"}`(也有 `BATCH` op)+ `event: close` |
| 7 | 工具调用 | ✅ coding 变体:模型遵循 `<|tool▁calls▁begin|>`(▁=U+2581)标签,但常丢前导 `|`(`<tool▁calls▁begin|>`)或混用 ASCII 下划线 —— 解析器已对全部变体归一化 + "半个标签"流式保护 |
| 8 | 识图 | ✅ `upload_file`(multipart)→ `fetch_files`(READY)→ **`fork_file_task {file_id, to_model_type:"vision"}`**(关键,返回新 file_id)→ completion `model_type:"vision"` + fork 后的 `ref_file_ids`。缺 fork 步骤报"发送至识图模式" |
| 9 | 多轮 | ✅ 网页无服务端历史需全量提交?否 —— **服务端按 session+parent_message_id 记忆**;aurora 每请求新会话,需把 input 全量拍平进 prompt(不加角色前缀,模型用专用 token 锚定角色) |
| 10 | prompt | ✅ **不加 "User:"/"Assistant:" 前缀**(实测加前缀模型报"乱码");纯文本拼接 |

> 遗留:识图 completion 在不同账号/网络下的稳定性未做大规模验证(浏览器本身在默认会话直接带图也会报"发送至识图模式",需 fork 步骤)。

## 一·五、原生能力实测边界(2026-08-13 三轮真实页面抓包)

DeepSeek 网页版**没有"工具调用"通道**,只有两个原生能力;客户端工具必须走文本协议:

| 能力 | 是否原生 | 实测证据 |
|---|---|---|
| 智能搜索 | ✅ 原生 **SEARCH fragment** | 响应 `fragments:[{type:"SEARCH", status:"WIP", queries:[...]}]` + 独立 results 帧(`{"p":"response/fragments/-1/results","v":[...]}`)+ 正文 `[citation:N]` 引用;请求体 `search_enabled:true` 开关 |
| 识图 | ✅ 原生 `model_type:"vision"` + `ref_file_ids` | 见上表 #8(upload → fork_file_task) |
| **代码执行 / 工具调用** | ❌ **无** | 算数题响应只有 `frag:RESPONSE`,模型输出 ```python 代码**文本**(无 tool/code/execute fragment);请求体始终无 `tools` 字段 |

**推论(回答"文本协议 vs 原生工具"):**
- 对 DeepSeek 而言两者**互补**:搜索用原生 SEARCH(真实结果帧 + citation),客户端工具(bash/read 等)用文本协议 —— 因为网页端没有工具通道,文本协议是唯一让模型调客户端工具的路,且模型认标签所以可靠。
- 与智谱对比:智谱有原生 `execute_sandbox_code`(真实云端沙箱)但**不认外部工具名**,所以智谱客户端工具调用不可靠;DeepSeek 无原生工具但**认文本标签**,所以可靠。两者成败都由"模型认不认"决定,与有无原生工具无关。

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
# 网页 token 注入池文件路径(每行一个 token;只放可丢弃小号,主号永不入池)
# token 来源(实测 P0):浏览器登录 chat.deepseek.com 后,
#   localStorage["userToken"] 是 {"value":"<token>","__version":"0"},
#   取 .value 一行一个写入本文件。不是 cookie!
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

- **`/v1/responses`**(pi 等):DeepSeek provider 处理 chat / coding 双变体。
- **`/v1/chat/completions`**(zcode / 测试 HTML 等):同样走 DeepSeek provider(chat/coding 双变体),输出 chat.completion 格式。两接口共享同一上游核心,仅输出格式不同。
- **`/v1/models`**:DeepSeek 模型带 `owned_by: "deepseek"` + `capabilities` 标注。
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

## 九、性能专项

- `docs/DEEPSEEK_PERF_2026-09-02.md` —— DeepSeek 通道提速闭环（实测对照 → 差距拆解 → 搜索开关 `DEEPSEEK_WEB_SEARCH` 实施 → 复测验收；含面板测速口径修正）。调延迟/对比官方 API 时先读它。
