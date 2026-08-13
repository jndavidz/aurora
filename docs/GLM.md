# 智谱清言(chatglm.cn)网页逆向接入

2026-08-13 完成。本文记录协议实测结论、代码结构、已知固有权衡。

## 一、对外模型

| id | 变体 | 能力 |
|---|---|---|
| `glm-5.2-chat` / `glm-5-chat` | chat | 纯真人对话(深度思考 + 联网搜索 + 识图),**绝不注入工具信息** |
| `glm-5.2-coding` / `glm-5-coding` | coding | 工具调用(围栏 JSON 文本协议 + 原生 tool_calls 双通道) |

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

1. **chat 变体完美**。深度思考 + 联网搜索 + 识图都是原生能力,模型自然使用,
   已验证端到端(ChatMode:"thinking" + IsNetworking:true)。

2. **coding 变体的工具调用不可靠**。实测(多次):
   - 模型不认 ChatGPT `<tool_call>`、DeepSeek `<|tool▁calls▁begin|>` 标签,会忽略并散文回答
   - 对自定义工具(list_files 等)**,概率性**输出 markdown 围栏 JSON
     (````json {"name":"list_files","arguments":{...}} ````),或直接调用自己的沙箱工具
   - `bash`/`shell`/`python` 类工具名**必触发**内置沙箱锚定 —— 模型自认有 `/mnt/data`
     权限,编造执行结果,任何提示词都无法覆盖
   - 提示词示例必须是**智谱原生 tool_calls 结构**(`{"type":"tool_calls","tool_calls":{...}}`),
     普通 `{"name":..}` 示例模型不当回事

3. **实现对策**:
   - 提示词用智谱原生结构示例 + 显式"无沙箱、禁模拟、只调 TOOLS AVAILABLE"
   - FenceParser 拦截围栏 JSON(模型遵约时),StreamToToolCallDeltas 输出
   - 原生 tool_calls 通道解析后**只转发客户端声明过的工具**(`toolNameInList` 过滤),
     智谱内置工具(execute_sandbox_code/search/finish)不泄漏给客户端
   - 结果:模型偶尔(约 1/3)输出可用工具调用;其余时候散文回答或只调内置工具

> 结论:智谱 coding 适合轻量工具场景;需要稳定工具调用的 coding agent
> (zcode/pi)仍建议用 DeepSeek / ChatGPT provider。chat 场景(搜索/思考/识图)智谱体验最好。

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
