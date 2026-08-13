# Kimi(www.kimi.com)网页逆向接入

2026-08-14 完成。本文记录协议实测结论(CDP 抓包 + curl 重放验证)、代码结构、已知固有权衡。

## 一、对外模型

| id | 变体 | 能力 |
|---|---|---|
| `kimi-chat` | chat | 纯真人对话(快速模式 K2.6,深度思考),**绝不注入工具信息** |
| `kimi-coding` | coding | 工具上下文注入 + 原生工具透传(见 §六,客户端自定义函数工具**不支持**) |

默认目录见 `defaultKimiModels`(KIMI_MODELS 未配置时);仅当 `KIMI_WEB_TOKENS` 指向的 token 池文件非空时 provider 才注册。

## 二、协议要点(2026-08-14 CDP 抓包 + curl 重放验证)

### 认证(纯 Bearer,无签名无 PoW)
- 凭据在 **localStorage**(非 cookie):`access_token`(JWT,~**15 分钟**)+ `refresh_token`(JWT,~**90 天**,刷新时轮换)
- 请求头:`authorization: Bearer <access_token>` + `x-msh-device-id` / `x-msh-session-id` / `x-traffic-id`(三个账号身份头可从 refresh_token 的 JWT claims 解出:device_id / ssid / sub)+ `x-msh-platform: web` + `x-msh-version: 2.0.0` + `x-language: zh-CN` + `r-timezone: Asia/Shanghai` + `connect-protocol-version: 1`
- `x-msh-shield-data` **可选**(curl 不带实测 200)

### Token 刷新(必需,access_token 仅 15 分钟)
```
POST https://auth.kimi.com/api/account.gateway.v1.AuthService/RefreshToken
Content-Type: application/json(普通 JSON,无 Connect 帧前缀,无 authorization 头)
Body: {"refresh_token":"<refresh_token>"}
Resp: {"accessToken":"...","refreshToken":"..."}   # 两者都轮换
```
实现:`internal/kimiweb/client.go` `RefreshAccessToken()`,仿 GLM 的池模式(token 池每行一个 refresh_token)。

### Completion(Connect RPC 流)
`POST https://www.kimi.com/apiv2/kimi.gateway.chat.v1.ChatService/Chat`
- **请求体帧格式**:`flags(1 字节) + 长度(4 字节大端) + JSON`(Connect unary)
- 快速模式:**`"scenario": "SCENARIO_K2D5"`**(= K2.6 快速;响应里的 TYPE_MODEL_DEGRADE 通知可证实),`options.reasoning_effort: "REASONING_EFFORT_LOW"`
- **只收单条 `message`(singular),不认 `messages` 数组**(实测被忽略)→ 上下文靠服务端 chat_id 会话,或**把全量历史拍平进单条 message 文本**(aurora 采用此方案,实测有效,保持无状态架构)
- 响应为**服务端流**:帧格式同请求,`flags=2` 收尾帧结束;帧间夹 `{"heartbeat":{}}`

### 响应流(增量状态同步 op)
| op / mask | 含义 |
|---|---|
| `op:set, mask:block.think` / `op:append, mask:block.think.content` | 思考(set 首段,append 增量) |
| `op:set, mask:block.text` / `op:append, mask:block.text.content` | 正文(set 首段,append 增量) |
| `op:set, mask:message.status` → `MESSAGE_STATUS_COMPLETED` | 消息完成 |
| `{"eventOffset":N,"done":{}}` | done 标记 |

真正增量流,直接拼接即可(无需 GLM 式的差值)。实现:`internal/kimiweb/stream.go` `ConsumeStream`。

## 三、多轮机制

- Kimi Chat RPC 只收单条 message,上下文靠服务端 chat_id + parent_id 链(首轮无 id,响应给 chat.id + 消息 id)。
- **aurora 采用无状态拍平**:把全量历史(instructions + 用户/助手消息)按"用户/助手"角色前缀拼进单条 message 文本,实测模型能正确使用上下文(如"秘密数字 777"跨轮问答)。
- chat 变体:`kimiFlattenResponses` / `kimiFlattenMessages`(剥离工具 item)。

## 四、变体行为

### chat(纯对话)
- 单条拍平消息 + `thinking:true`,tools 全空(不开联网搜索,避免正文引用标记污染),剥离客户端 tools。
- 原生工具调用(模型自带 ipython 等)**不外露**:模型服务端执行后把结果折进正文,provider 只流文本/思考。

### coding(工具上下文注入 + 原生工具透传)
- 客户端工具列表注入拍平文本(TOOLS AVAILABLE 上下文,让模型知道 agent 有这些工具)。
- 响应解析 `block.tool`(原生工具调用,如 `ipython`),**只转发客户端声明过的同名工具**(`toolNameInList` 过滤,同 GLM);其余静默(模型已把结果折进正文)。
- 原生工具调用 block.tool 生命周期:
  ```
  set(无 mask):      tool{toolCallId, name, status:STATUS_PENDING}
  append block.tool.args ×N: 参数 JSON 逐 token 增量
  set(含 block.tool.args):   args 定稿 + toolCallId 归一化(ipython:1) + STATUS_RUNNING → 整单上报
  set(含 block.tool.contents): contents:[{text:"结果"}] + STATUS_DONE(结果,不转发)
  ```

## 五、验证命令

```bash
# 单测
go test ./internal/kimiweb/... ./internal/provider/...

# 本地起服务(token 池 = tokens/kimi_tokens.txt,每行一个 refresh_token)
KIMI_WEB_TOKENS=tokens/kimi_tokens.txt SERVER_PORT=18080 Authorization=test go run .

# 模型聚合
curl -s localhost:18080/v1/models -H "Authorization: Bearer test"

# chat 非流式/流式
curl -s localhost:18080/v1/chat/completions -H "Authorization: Bearer test" -H "Content-Type: application/json" \
  -d '{"model":"kimi-chat","messages":[{"role":"user","content":"17*23等于几?"}],"stream":false}'

# responses 非流式/流式
curl -s localhost:18080/v1/responses -H "Authorization: Bearer test" -H "Content-Type: application/json" \
  -d '{"model":"kimi-chat","input":"2+2等于几?","stream":false}'

# coding 原生工具透传(客户端声明 ipython 工具,模型原生调用会转成 function_call)
curl -s localhost:18080/v1/chat/completions -H "Authorization: Bearer test" -H "Content-Type: application/json" \
  -d '{"model":"kimi-coding","messages":[{"role":"user","content":"用 ipython 计算 123*456"}],"tools":[{"type":"function","function":{"name":"ipython","description":"执行python代码","parameters":{"type":"object","properties":{"code":{"type":"string"}}}}}],"stream":false}'
```

## 六、已知固有权衡

1. **客户端自定义函数工具不支持**(结构性,无法绕过):
   - Chat RPC 的 `ToolType` 枚举没有 FUNCTION(从 chat_pb 逆向):只有 SEARCH / IMAGE_GENERATION / SEMANTIC_MEMORY / AUDIO_GENERATION / DEVICE_LBS / DEVICE_TOOL / PARALLEL_AGENT / ASK_USER / CRON_JOB / GOAL / PARALLEL_AGENT_V2 / BANANA / SLIDES_OUTLINE。
   - 服务器把未知工具抹成 `[{}]`,模型拒绝假装调用("我没有 get_weather 这个工具"),GLM 式围栏 JSON 文本协议也无效(K2.6 工具诚实度极高)。
   - coding 变体因此只能:客户端工具注入上下文(尽力而为)+ 原生工具透传(ipython / web_search 等,客户端声明同名工具时转发)。
2. **access_token 只有 15 分钟**:必须有刷新流(auth.kimi.com RefreshToken),刷新会轮换 refresh_token(进程内生效,池文件不重写——与 GLM 相同的漂移问题)。
3. **降级通知 TYPE_MODEL_DEGRADE**:高峰期自动切 K2.6 快速(部分用户视角等价场景),无害,忽略。
4. **不开联网搜索**:chat/coding 的 tools 均传空,避免正文引用标记(🛠web_search:1#5🛠 等)污染输出;需要搜索时把 `TOOL_TYPE_SEARCH` 加进 kimiweb.CompletionRequest.Tools 并处理引用标记。
5. **网页逆向是结构性封号风险**(同元宝教训):token 只放可丢弃小号、主号永不入池、会话用完即删。
6. **K3 后续**:换 `scenario`(如 SCENARIO_K3)再抓一次包即可接入思考模式;届时工具协议可能不同,需重新确认。
