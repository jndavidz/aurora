# 智谱清言(chatglm.cn)网页逆向接入

2026-08-13 完成。本文记录协议实测结论、代码结构、已知固有权衡。

## 一、对外模型

| id | 变体 | 能力 |
|---|---|---|
| `glm-5.2-chat` | chat(快速挡) | 纯真人对话(快速模式 + 联网搜索 + 识图),**绝不注入工具信息** |
| `glm-5.2-chat-thinking` | chat(思考挡) | 纯真人对话(深度思考 + 联网搜索 + 识图),**绝不注入工具信息** |
| `glm-5.2-coding` | coding | 工具调用(围栏 JSON 文本协议 + 原生 tool_calls 双通道) |

> 2026-08-14 用户决定:glm-5 系列(`glm-5-chat`/`glm-5-coding`)下线,只留 5.2;
> `glm-5.2-chat` 拆成两个挡位 —— `glm-5.2-chat`(快速,speed)与
> `glm-5.2-chat-thinking`(思考,thinking),由模型 id 决定 `chat_mode`。
> 解析白名单:只接受 `glm-5.2-` 前缀。

默认目录见 `defaultGlmModels`(GLM_MODELS 未配置时);仅当 `GLM_WEB_TOKENS` 指向的 token 池文件非空时 provider 才注册。

## 二、协议要点(CDP 抓包 + 主 JS 逆向,2026-08-13)

### 认证
- 长期凭据:`chatglm_refresh_token` cookie(JWT,~2h 换发一次 refresh_token 本身也轮换)
- `POST /chatglm/user-api/user/refresh`(body `{}`)换发 `access_token`(JWT,~2h)
- completion 用 `Authorization: Bearer <access_token>`

### 签名(X-Sign)
所有 `/chatglm/` 请求带 `X-Timestamp` / `X-Nonce` / `X-Sign`:
```
X-Sign = MD5(混淆timestamp + "-" + nonce + "-" + signKey)
signKey = 8a1317a7468aa3ad86e997d08f3f31cb   (从主 JS 逆向)
```
timestamp 混淆(与网页 JS 一致):`A=Date.now()` 字符串,`e=len(A)`,
`t = 各位数字之和 - A[e-2]`,`结果 = A[0:e-2] + (t%10) + A[e-1:e]`。
缺签名 → 40001;签名但 timestamp 未混淆 → 40012。实现:`internal/glmweb/sign.go`。

### Completion(SSE)
`POST /chatglm/backend-api/assistant/stream`(assistant_id `65940acff94777010aa6b796` = 全部工具智能体):
```json
{
  "assistant_id": "...", "conversation_id": "", "project_id": "", "chat_type": "user_chat",
  "meta_data": {"cogview": {"rm_label_watermark": false}, "is_test": false,
                "input_question_type": "xxxx", "channel": "", "draft_id": "",
                "chat_mode": "thinking", "is_networking": true, "quote_log_id": "", "platform": "pc"},
  "messages": [{"role": "user", "content": [{"type": "text", "text": "..."}]}]
}
```
- SSE 每帧 `data:{JSON}`,**parts 数组全量重发**(非增量 patch)
- content 类型:`think`(思考)、`text`(正文)、`tool_calls`(原生工具调用)、`tool_result`(工具结果)
- 原生 tool_calls 结构:
  ```json
  {"type":"tool_calls","tool_calls":{"id":"tool-xxx","name":"search","arguments":"{\"search_query\":[...]}"}}
  ```
  `name:"finish"` 是工具阶段结束哨兵,不是真实调用。
- `status:"finish"` 收尾(每帧也可能带 `last_error`)
- 首轮 `conversation_id` 为空,响应帧回填;续轮带上即可续会话

### 模型
页面 DOM 读取:GLM-5.2 / GLM-5(默认"全部工具智能体"助手)。

## 三、代码结构

```
internal/glmweb/
  sign.go      — X-Sign 签名(混淆 timestamp + MD5)
  client.go    — refresh 换发 access_token、completion SSE 请求、token 池(每行一个 refresh_token,轮询/轮换)
  stream.go    — SSE 解析:think/text 增量差值 + 原生 tool_calls 提取(去重、finish 哨兵过滤)
  live_test.go — 真实上游冒烟(GLM_TEST_TOKEN 环境变量)
internal/provider/
  glm.go              — Provider 接口、模型路由(chat/coding)、Registry 注册
  glm_chat.go         — chat 变体(Responses + ChatCompletions,剥离 tools)
  glm_coding.go       — coding 变体(FenceParser 围栏拦截 + 原生 tool_calls 过滤)
  glm_instructions.go — coding 提示词(智谱原生结构示例 + 禁内部沙箱)
internal/toolcall/
  fence.go     — FenceParser 流式拦截 markdown 围栏 JSON + StripFencedBlocks + FlushCalls
  fence_test.go
```

## 四、已知固有权衡(重要)

智谱网页版是**"全部工具智能体"**结构:模型原生带真实沙箱工具
(`execute_sandbox_code` 等),后端真的会执行并回传结果。这带来:

1. **chat 变体**。联网搜索 + 识图是原生能力,模型自然使用。
   2026-08-14 起默认 **快速模式**(ChatMode:"speed",用户决定默认快速而非深度思考);
   之前为 "thinking"。⚠️ 实测联网搜索触发是**概率性**的:同一 query 重复问,
   有时触发(带 `【turn*search*】` 引用)、有时模型拒绝("无法实时联网")——上游
   模型行为,非 aurora 代码问题(请求体 `is_networking:true` 已正确透传)。

2. **coding 变体无法可靠工具调用 —— 模型级能力边界,非提示词可解**。
   经 20+ 组实测(2026-08-13 二次深挖),穷尽以下方案**全部无效**:
   - ChatGPT `<tool_call>`、DeepSeek `<|tool▁calls▁begin|>` 标签 → 忽略并散文回答
   - 智谱原生 `{"type":"tool_calls","tool_calls":{...}}` 结构示例 → 不稳定(约 1/3,
     且纯属温度采样,同提示词多次结果随机)
   - markdown 围栏 JSON 示例 / 裸 JSON 示例 → 模型不当回事
   - 强约束(禁沙箱、禁模拟、只调 TOOLS AVAILABLE)→ 模型更困惑
   - few-shot(消息历史放成功范例)→ 无效,默认助手 100% 仍调沙箱
   - 6 种 `chat_mode`(""/thinking/speed/chat/auto/normal)→ 全部仍触发沙箱
   - 无沙箱 agent(如 `669fb16ffdf0683c86f7d903`)→ 不调沙箱但换成 browser 工具,
     且被固定为图生视频用途,仍不遵循外部工具指令
   - 请求体注入 `tools`/`functions`/`tool_definitions` 字段 → 模型不识别
     (网页端协议无 function calling,与官方 API open.bigmodel.cn 是两套)

   **根因**:智谱网页版模型**没有 function calling 训练**,只训练过"内置工具智能体"
   (search / execute_sandbox_code / draw / browser 等)。它对"输出一个任意外部工具调用
   让外部系统执行"没有概念 —— 它要么用自己的沙箱真实执行(见下),要么诚实拒绝
   ("作为 GLM 大语言模型,我没有访问本地文件系统的权限")。这与 DeepSeek(有标签协议
   训练)、ChatGPT(有 function calling)本质不同。

3. **有价值的替代能力:云端沙箱执行**。
   `execute_sandbox_code` 是 **assistant 级配置**(默认助手 65940acff94777010aa6b796
   带沙箱,特定 agent 不带),且是**真实云端执行** —— 后端真跑代码并回传结果。
   实测 coding 变体问"用 python 算 1 到 100 的和"返回 `5050`(正确)。
   因此智谱 coding 变体的真实定位是**"云端代码执行助手"**:
   - ✅ 适合:算数、写脚本并验证、数据分析(执行在智谱云端沙箱)
   - ❌ 不适合:读用户代码仓库、改本地文件(需要客户端工具,智谱做不到)

4. **实现对策**(尽力而为 + 合理降级):
   - FenceParser 拦截围栏 JSON(模型偶尔输出时能抓到,概率低但无害)
   - 原生 tool_calls 通道解析后**只转发客户端声明过的工具**(`toolNameInList` 过滤),
     `execute_sandbox_code`/`search`/`finish` 不暴露给客户端(客户端执行不了智谱沙箱)
   - 模型调沙箱时,aurora 正常返回其文本结果(沙箱执行能力对客户端透明)

> 结论:智谱 **chat 场景(搜索/思考/识图)体验最好**;**coding 场景**不要依赖智谱输出
> 客户端工具调用(做不到),需要稳定工具调用的 coding agent(zcode/pi)请用
> DeepSeek / ChatGPT provider。

## 五、环境变量

```
GLM_WEB_TOKENS=/work/.runtime/tokens/glm_tokens.txt   # 每行一个 chatglm_refresh_token(不入库)
GLM_MODELS=                                          # 可选,默认 glm-5.2-chat, glm-5.2-coding, glm-5-chat, glm-5-coding
GLM_WEB_BASE=https://chatglm.cn
GLM_PROXY=
```

## 六、验证

```bash
# 本地单测(签名/SSE/token池/路由/FenceParser)
go test ./internal/glmweb/ ./internal/provider/ ./internal/toolcall/

# 真实上游冒烟(token 从浏览器 cookie 提取)
GLM_TEST_TOKEN=<chatglm_refresh_token> go test ./internal/glmweb/ -run TestLiveRefresh -v
GLM_TEST_TOKEN=<chatglm_refresh_token> go test ./internal/glmweb/ -run TestLiveCompletion -v

# 本地服务冒烟
GLM_WEB_TOKENS=./tokens/glm_tokens.txt go run .
curl -N http://127.0.0.1:8080/v1/responses -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2-chat","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"用一句话介绍你自己"}]}],"stream":true}'
```
