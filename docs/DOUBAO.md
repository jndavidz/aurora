# 豆包(www.doubao.com)网页逆向接入

2026-08-14 完成。本文记录协议实测结论、代码结构、账号提取与限频。

> ⚠️ **封号风险**:字节系风控强(元宝前车之鉴,两账号一天封号)。代码已内置
> 限频(单账号并发 1、间隔 >=2s),只放可丢弃小号。

## 一、对外模型

| id | 变体 | 能力 |
|---|---|---|
| `doubao-chat` | chat | 纯真人对话(模型自动用原生搜索/深度思考),**绝不注入工具信息** |

> 豆包只做 chat 变体,不做 coding:coding 代码已**注释禁用**(`doubao_coding.go`
> 整体块注释 + `doubao.go` 中相关分支注释),保留待将来恢复。工具调用场景由
> 其他 provider(DeepSeek/Gemini/Grok 等)承担,豆包纯对话体验更好、不冒文本
> 协议工具调用的兼容风险。

默认目录见 `defaultDoubaoModels`(DOUBAO_MODELS 未配置时);仅当 `DOUBAO_ACCOUNTS`
指向的账号 JSON 非空时 provider 才注册。

## 二、协议要点(CDP 抓包 + Node/Go 直连验证,2026-08-14)

### 端点与认证

```
POST https://www.doubao.com/chat/completion?<URL参数>
```
- **无 Authorization/cookie 头**(cookie 自动带);认证靠 URL 参数:
  `aid`(应用 id)、`device_id`、**`fp=verify_`**(风控指纹)、**`msToken`**、
  **`a_bogus`**(签名,短时效,需定期刷新)、`web_id`/`tea_uuid`/`web_tab_id`
- **cookie**(bd_sso/sessionid 等)是登录凭证,必须带
- fp/msToken/web_id 会话级固定可复用;**a_bogus 短时效**(分钟级,见 §四)

### 请求 body(JSON)

```json
{
  "client_meta": {"conversation_id":"...","bot_id":"7338286299411103781","last_section_id":"...","last_message_index":N},
  "messages": [{"local_message_id":"uuid","content_block":[{"block_type":10000,"content":{"text_block":{"text":"消息"}}}],"message_status":0}],
  "option": {"need_deep_think":0,"agent_mode":2,"sse_recv_event_options":{"support_chunk_delta":true},
             "model_config":{"model_item_key":"0","model_extra_params":{}},
             "aggregate_params":{"model_item_key":"0","provider_id":""}, ...}
}
```
- bot_id `7338286299411103781` = 默认豆包 bot
- **多轮 = messages 数组全量回放**(用户/助手交替),不靠 conversation 服务端记忆

### 响应(SSE 事件流)

```
SSE_HEARTBEAT → SSE_ACK(question_id/message_index/section_id)→ FULL_MSG_NOTIFY → STREAM_CHUNK × N → 完成
```
- **正文增量在 `patch_op[].patch_value.tts_content`(patch_object=111)**!
  `patch_object=1` 的 `content_block[].text_block.text` 只含首个字(冗余,忽略)
- 完成:`patch_object=50 ext.is_finish:"1"`;`patch_type=2` 是删除操作
- 关键坑:**不能只读 text_block**(否则文本截断,只拿到首字),正文在 tts_content

## 三、代码结构

```
internal/doubaoweb/
  client.go     — 客户端(completion + SSE 解析 + 账号池 + 限频)
  client_test.go— 账号池/限频/body 构造单测
  live_test.go  — 真实上游冒烟(DOUBAO_ACCOUNT_FILE)
internal/provider/
  doubao.go      — Provider 接口、模型路由(仅 chat;coding 分支已注释)
  doubao_chat.go — chat 变体(多轮全量回放)
  doubao_coding.go — coding 变体(文本协议工具,**已整体注释禁用**,恢复见文件头)
  doubao_test.go — 模型解析/chat 无工具(coding 用例已注释)
```

## 四、已知固有权衡

1. **a_bogus 短时效**:字节风控签名,实测分钟级失效。aurora 服务端无法自己生成,
   需**定期从浏览器刷新**到账号文件。这是与 DeepSeek/Gemini(可复用)的本质差异。
2. **不能创建新会话**:豆包强制用已有 conversation_id,空 conversation + 
   need_create_conversation 会报 `common invalid param`。账号必须内置一个会话 id。
3. **多轮靠全量回放**:不靠 conversation 记忆,像 DeepSeek 一样 messages 数组回放历史。

## 五、环境变量

```
DOUBAO_ACCOUNTS=/work/.runtime/tokens/doubao_accounts.json  # 账号池 JSON(不入库)
DOUBAO_MODELS=                                             # 可选,默认 doubao-chat
```
## 六、账号提取(一次性)

1. 浏览器登录 www.doubao.com(小号)
2. CDP 抓一次 `/chat/completion` 请求,提取:
   - **cookie**:完整 cookie 串
   - **URL 参数**:aid/device_id/fp/msToken/a_bogus/web_id/tea_uuid/web_tab_id
   - **client_meta**:conversation_id/last_section_id/last_message_index
3. 写账号 JSON:
   ```json
   [{"cookie":"...","aid":"497858","device_id":"...","fp":"verify_...","ms_token":"...",
     "a_bogus":"...","web_id":"...","tea_uuid":"...","web_tab_id":"...",
     "conv_id":"...","section_id":"...","last_msg_idx":N}]
   ```

## 七、验证

```bash
go test ./internal/doubaoweb/ ./internal/provider/

# 真实上游冒烟
DOUBAO_ACCOUNT_FILE=./tokens/doubao_accounts.json go test ./internal/doubaoweb/ -run TestLiveComplete -v

# 本地服务冒烟
DOUBAO_ACCOUNTS=./tokens/doubao_accounts.json go run .
curl -N http://127.0.0.1:8080/v1/responses -H "Content-Type: application/json" \
  -d '{"model":"doubao-chat","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"用一句话介绍你自己"}]}],"stream":true}'
```
