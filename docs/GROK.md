# Grok(grok.com)网页逆向接入

2026-08-14 完成。本文记录协议实测结论、代码结构、已知固有权衡。

## 一、对外模型

| id | 变体 | 能力 |
|---|---|---|
| `grok-3-chat` | chat | 纯真人对话(模型自动用原生搜索 + 云端沙盒),**绝不注入工具信息** |
| `grok-3-coding` | coding | 云端沙盒代码执行助手(同智谱 glm-*-coding 定位) |

默认目录见 `defaultGrokModels`(GROK_MODELS 未配置时);仅当 `GROK_COOKIES` 指向的
cookie 池文件非空时 provider 才注册。

## 二、协议要点(CDP 抓包 + Node/Go WS 直连验证,2026-08-14)

### 传输:WebSocket(不是 HTTP SSE)

- 端点:`wss://grok.com/ws/mgw/?uid=<x-userid>`(uid = x-userid cookie 值)
- 认证:**httpOnly cookie**(sso / sso-rw / cf_clearance / grok_device_id 等)随握手发送;
  Cloudflare(cf_clearance)是浏览器指纹挑战,直连时用浏览器抓的完整 cookie 串
- 协议:OpenAI Realtime 风格事件信封 `{"session_id":..., "event":{...}}`

### 会话与消息流程

```
1. session.create(event.session 对象必带,缺则 invalid_envelope)
   → 响应 session.created / conversation.attached
2. conversation.item.create:
   {session_id, event:{type, item:{type:"message", role:"user",
     x_grok:{client_message_id, input_chunks:[{text:{text:"..."}}]}}, parent_response_id}}
3. response.create: {session_id, event:{type:"response.create"}}
   (castle_request_token 可选,实测缺省正常)
4. 响应事件流:response.created → output_item.added → content_part.added
   → output_text.delta → output_text.done → output_item.done → response.done
```

关键细节:
- `response.output_text.delta` 的 `delta` 字段是**纯字符串**(非 `{text:..}` 对象)
- 多轮:item.create 带 `parent_response_id`(上一轮 response.id)即可续上下文
- 思考:模型可能输出 "Thinking about your request" 前缀(思考文本,无明确结束标记)

### 能力边界(实测)

| 能力 | 是否原生 | 实测证据 |
|---|---|---|
| 智能搜索 | ✅ 原生 | 问天气自动触发搜索,回复带真实来源("数据来源于中国气象局官网") |
| 云端沙盒执行 | ✅ 原生 | "用 python 算 1 到 100 的和" → 沙盒执行返回 5050;模型自述工作目录 `/home/workdir/artifacts`,可用 bash/python |
| 客户端工具调用 | ❌ | 强制 tool_choice 返回 `status:"incomplete"`;模型声明**不能访问用户本地文件**;不输出可解析的 function_call 事件 |

## 三、代码结构

```
internal/grokweb/
  client.go     — WS 客户端(session.create/item.create/response.create + 事件解析)
  client_test.go— 账号池加载/轮询单测
  live_test.go  — 真实上游冒烟(GROK_COOKIE 环境变量)
internal/provider/
  grok.go        — Provider 接口、模型路由(chat/coding)、Registry 注册
  grok_chat.go   — chat 变体(Responses + ChatCompletions,剥离 tools;流式前缀剥离)
  grok_coding.go — coding 变体(沙盒定位 + FenceParser 尽力而为工具通道)
  grok_test.go   — 模型解析/chat 无工具注入/coding prompt/splitGrokThinking
```

## 四、已知固有权衡(与智谱同款)

Grok 网页版模型有**远程沙盒 + 原生搜索**,与智谱"全部工具智能体"结构类似:

1. **chat 变体完美**。原生搜索自动触发、沙盒可执行代码,真人对话体验好。
2. **coding 变体 = 云端沙盒执行助手**。模型在沙盒(`/home/workdir/artifacts`)真跑
   代码并回传结果;但**不能访问用户本地文件**,客户端自定义工具(bash/read 等)
   无法可靠调用(强制 tool_choice 报 incomplete)。客户端工具通道走 FenceParser
   "尽力而为"(模型偶尔输出围栏 JSON 时捕获)。
3. **"Thinking about your request" 前缀**:思考文本与正文无缝拼接,无结束标记,
   仅剥离前缀,不做推理分离(见 `splitGrokThinking`)。

> 结论:与智谱一致 —— chat 场景(搜索/思考/沙盒)体验好;需要稳定客户端工具调用
> 的 coding agent(zcode/pi)请用 DeepSeek / ChatGPT provider。

## 五、环境变量

```
GROK_COOKIES=/work/.runtime/tokens/grok_cookies.txt   # 每行 uid|cookie 串(不入库)
GROK_MODELS=                                          # 可选,默认 grok-3-chat, grok-3-coding
```

## 六、验证

```bash
# 本地单测
go test ./internal/grokweb/ ./internal/provider/

# 真实上游冒烟(cookie 从浏览器提取:uid = x-userid 值;cookie = 完整串)
GROK_COOKIE="<uid>|<cookie串>" go test ./internal/grokweb/ -run TestLiveComplete -v

# 本地服务冒烟
GROK_COOKIES=./tokens/grok_cookies.txt go run .
curl -N http://127.0.0.1:8080/v1/responses -H "Content-Type: application/json" \
  -d '{"model":"grok-3-chat","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"用一句话介绍你自己"}]}],"stream":true}'
```
