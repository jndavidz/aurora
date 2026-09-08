# Qwen(千问,www.qianwen.com)网页逆向接入实测

> 逆向时间:2026-08-13(CDP 抓包 + curl 复刻验证闭环)。
> 关联:`docs/ARCHITECTURE.md`、`docs/PROVIDER_ARCHITECTURE.md`(落点速查)、`docs/CDP_BROWSER_DEBUG.md`(抓包方法)。
> 实现:`internal/qianwenweb/` + `internal/provider/qianwen*.go`,暴露模型 `Qwen3.8-Max`。

---

## 〇、结论速览

| 项 | 值 |
|---|---|
| 聊天端点 | `POST https://chat2.qianwen.com/api/v2/chat` |
| 认证 | cookie **`tongyi_sso_ticket`**(httpOnly,约 1 年)为账号凭据;WAF 升级后还需 **`x5sec`** 通关 cookie(约 20 分钟,浏览器过滑块后签发) |
| 模型 id | `"model": "Qwen3.8-Max"`(网页真实 id;默认款是 `Qwen3.7`,另有 `Qwen3.7-Max`/`Qwen3.6-Flash`) |
| 工具调用 | **不支持**自定义外部工具(`tools` 字段被忽略);仅内置「联网搜索」(`enable_web_search` 操作) |
| 思考模式 | `chat_mode:"expert"` 无效(返回乱码);用 `"quick"`,无 reasoning 内容 |
| 多轮 | 单请求 `messages` 数组带完整历史 + `scene_param:"first_turn"` + 随机 session/topic id 即可 |
| WAF | `Accept` 必须显式含 `text/event-stream`;`Origin`+`Referer` 必须匹配;否则 captcha / 空流 |
| WAF(TLS) | **必须 Chrome 指纹 TLS**(tls-client Chrome_146);Go 标准库/curl 直连会被 JA3 风控拦截 |
| 安全头 | `clt-acs-sign` 签名、`bx-ua`、`eo-clt-actkn` 等全套**均不需要**,静态头即可 |

## 一、认证与 Token

- 账号凭据:浏览器 cookie **`tongyi_sso_ticket`**(httpOnly,secure,`.qianwen.com` 域),长期有效
  (实测 expires 2027-08,约 1 年)。单独用 `tongyi_sso_ticket_hash` 会报 `{"code":"EX015","msg":"签名错误"}`。
- **WAF 通关 cookie `x5sec`**(`chat2.qianwen.com` 域):当 WAF 升级(短时间大量请求触发)后,
  不带 `x5sec` 的请求会被 captcha 拦截(返回 `FAIL_SYS_USER_VALIDATE`/`RGV587_ERROR`)。
  `x5sec` 是浏览器过滑块验证码后签发,实测**有效期约 20 分钟**,过期后需重新在浏览器过验证码
  (同 IP/账号会连带解锁)。冷却期内(几分钟~几十分钟)仅靠 `tongyi_sso_ticket` 也可能恢复。
- **token 池文件 `tokens/qianwen_tokens.txt`**:每行一个**完整 cookie header**(含 `tongyi_sso_ticket`
  与 `x5sec` 等),客户端解析后全量发送;只有 ticket 的行在 WAF 平静期也可用。
  格式示例:
  ```
  tongyi_sso_ticket=<ticket>; x5sec=<x5sec>; x5sectag=<tag>; sm_ruid=<ruid>; sm_uuid=<uuid>; JSESSIONID=<sid>
  ```
- `ut` 查询参数与 `x-device-id` 请求头:**非空即可**,服务端不校验与账号绑定(实测随机值成功,空值失败)。
  实现里每个 client 实例生成一个固定 uuid 即可。

## 二、请求

### 2.1 URL 查询参数

```
POST https://chat2.qianwen.com/api/v2/chat
  ?biz_id=ai_qwen&fe_version=1.0.0&chat_client=h5&device=pc&fr=pc&pr=qwen
  &ut=<非空用户标识>&la=zh-CN&tz=Asia%2FShanghai&wv=4.2.1&ve=4.2.1
  &nonce=<随机>&timestamp=<毫秒时间戳>
```

### 2.2 必需请求头

| 头 | 值 | 必要性 |
|---|---|---|
| `Content-Type` | `application/json` | 必须 |
| `User-Agent` | Chrome 级 UA | 必须 |
| `Accept` | `application/json, text/event-stream, text/plain, */*` | **必须显式含 `text/event-stream`**;否则服务器直接返回空流(curl 默认 `*/*` 也不行) |
| `Origin` | `https://www.qianwen.com` | **必须**;缺失触发阿里 WAF 人机验证(captcha,`rgv587_flag`) |
| `Referer` | `https://www.qianwen.com/chat/<session_id>` | **必须**(与 Origin 同因) |
| `Cookie` | `tongyi_sso_ticket=<ticket>` | 认证,唯一必需 cookie |
| `x-device-id` | 同 `ut` | 建议(与 ut 一致) |
| `x-platform` | `pc_tongyi` | 建议(静态) |
| `prod_id` | `tongyi` | 建议(静态) |

> **不需要**:`clt-acs-sign`(签名)、`clt-acs-request-params`、`bx-ua`、`bx-umidtoken`、
> `eo-clt-actkn`、`eo-clt-sacsft`、`clt-acs-bfg`、`x-wpk-*`、`x-chat-biz` 等(浏览器会发,服务端不校验)。

> **TLS 指纹**:必需 Chrome 指纹 TLS 连接(见 §四),不能用 Go 标准库/curl 直连 ——
> WAF 按 JA3 风控,非浏览器指纹请求量一大就 captcha。

### 2.3 请求体

```json
{
  "req_id": "<32位hex uuid>",
  "parent_req_id": "0",
  "messages": [
    {"mime_type": "text/plain", "content": "用户消息", "meta_data": {"ori_query": "用户消息"}, "status": "complete"},
    {"mime_type": "text/plain", "content": "助手回复", "meta_data": {}, "status": "complete"},
    {"mime_type": "text/plain", "content": "下一条用户消息", "meta_data": {"ori_query": "..."}, "status": "complete"}
  ],
  "scene": "chat", "sub_scene": "", "scene_param": "first_turn",
  "session_id": "<32位hex uuid>", "biz_id": "ai_qwen", "topic_id": "<32位hex uuid>",
  "model": "Qwen3.8-Max", "from": "default", "protocol_version": "v2",
  "messages_merge": false, "chat_client": "h5", "deep_search": null,
  "temporary": false, "chat_mode": "quick", "bucket": {}
}
```

- `scene_param`:`"first_turn"` 开新会话(客户端可随机生成 session/topic id,服务端自动建档);后续轮次可用 `"continue_chat"` + 同一 session/topic。**aurora 实现每轮用 `first_turn` + 随机 id + 全量历史**,保持无状态。
- `messages` 的 user 消息带 `meta_data.ori_query`(与 content 相同);assistant 消息 `meta_data` 为 `{}` 或省略。
- `chat_mode` 固定 `"quick"`(默认网页模式)。`"expert"`(思考研究)实测无效。
- system 消息:网页协议无 system 角色,aurora 层需把 system 内容并入首条 user 消息或忽略(网页无此概念)。

## 三、响应(SSE)

```
event:message
data:{"communication":{"chat_assistant_name":"MainChatAgent","chat_chain_name":"openChat",
      "disconnection_signal":0,"front_followup":true,"llm_client_instance":"qwen_agent_instance",
      "reqid":"...","resid":0,"sessionid":"..."},
      "data":{"messages":[{"meta_data":{"operation_types":[...],"intent_content":"MainChatAgent",
      "generate_mode":"stream","agent_mode":"0"},"mime_type":"signal/post","status":"complete"},
      {"meta_data":{"elements":[{"type":"text","content":""}],"type":"bar_update"},
      "mime_type":"bar/progress","status":"processing"}],"status":"processing"},
      "error_code":0,"error_msg":"","success":true,"traceId":"..."}
...
event:complete
data:{... 全部 status:"complete" ...}
event:complete
data:true
```

解析规则:

1. 每帧 `data:` 是完整 JSON(与 GLM 相同,**全量重发**非增量)。
2. 助手文本 = `data.messages[]` 中最后一条 **`mime_type == "multi_load/iframe"`** 消息的 `content` 字段;
   同一消息的 `status` 由 `"processing"` → `"complete"`。
3. 增量 = 当前 content `strings.TrimPrefix` 掉上一帧的 content(实现需记录上一帧全文)。
4. `communication.resid` 每帧 +1,可作顺序校验。
5. 结束:出现 `event:complete` 且 `data:true`,或 `data.messages` 全部 `status:"complete"`。
6. 错误:`error_code != 0` 时按错误处理;HTTP 200 但 `Content-Length: 0` 视为空流(头不齐)。
7. 无 reasoning 内容(quick 模式),`reasoning_text` 事件不产出。

## 四、边界与坑

- **Accept 头**:复刻时最容易踩 —— 必须显式带 `text/event-stream`,curl 默认 `Accept: */*` 会拿到空流。
- **Origin/Referer**:缺了不是 403,而是返回 WAF captcha HTML(`rgv587_flag:sm` + `x5secdata`),
  需要 `Location` 重定向做滑块验证,程序侧无法绕过 —— 头必须齐。
- **TLS 指纹(JA3)风控(关键)**:千问 WAF 会按客户端 TLS 指纹风控。Go 标准库 `http.Client` /
  curl 请求量一上来就被拦(返回 `FAIL_SYS_USER_VALIDATE`/`RGV587_ERROR` captcha),而浏览器指纹
  (Chrome)长期可用。**实现必须用 Chrome 指纹客户端**(`aurora/httpclient/bogdanfinn` 的
  tls-client + `profiles.Chrome_146`),并显式声明 `Accept-Encoding: gzip` 手动解压
  (tls-client 无透明解压;防御性兼容 zstd)。
- **`ut` 为空**:返回非空流但无内容(OTHER);非空即用。
- **工具调用**:带 `tools` 数组请求会成功但模型回复「您的问题似乎出现了编码错误」—— 协议不支持,勿注入。
- **多账号**:token 池每行一个 ticket,轮询取号;并发≈账号数。
- **请求频率**:WAF 有滑动窗口限流,短时间大量请求(尤其非浏览器指纹)会把 IP/指纹拉进 captcha
  墙;上线后控制并发,风控墙一般几分钟内冷却。若冷却无效,需在浏览器过一次滑块签发新的
  `x5sec` cookie(约 20 分钟有效)并更新 token 池(见 §一)。

## 五、curl 复刻最小示例(验证用)

```bash
curl -s --compressed -X POST 'https://chat2.qianwen.com/api/v2/chat?biz_id=ai_qwen&fe_version=1.0.0&chat_client=h5&device=pc&fr=pc&pr=qwen&ut=<uid>&la=zh-CN&tz=Asia%2FShanghai&wv=4.2.1&ve=4.2.1&nonce=<r>&timestamp=<ms>' \
  -H 'Content-Type: application/json' \
  -H 'User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36' \
  -H 'Accept: application/json, text/event-stream, text/plain, */*' \
  -H 'Origin: https://www.qianwen.com' \
  -H 'Referer: https://www.qianwen.com/chat/<session>' \
  -H 'x-device-id: <uid>' -H 'x-platform: pc_tongyi' -H 'prod_id: tongyi' \
  -H 'Cookie: tongyi_sso_ticket=<ticket>; x5sec=<x5sec>' \
  -d '{"req_id":"<uuid>","parent_req_id":"0","messages":[{"mime_type":"text/plain","content":"你好","meta_data":{"ori_query":"你好"},"status":"complete"}],"scene":"chat","sub_scene":"","scene_param":"first_turn","session_id":"<uuid>","biz_id":"ai_qwen","topic_id":"<uuid>","model":"Qwen3.8-Max","from":"default","protocol_version":"v2","messages_merge":false,"chat_client":"h5","deep_search":null,"temporary":false,"chat_mode":"quick","bucket":{}}'
```

## 六、抓包原始资料

- 页面:https://www.qianwen.com (预连接 `chat2-api.qianwen.com` / `chat2.qianwen.com` / `sec.qianwen.com` / `member.qianwen.com`)
- 模型下拉实测:`Qwen3.7-千问`(默认)、`Qwen3.8-Max`(新,旗舰,视觉)、`Qwen3.7-Max`(代码)、`Qwen3.6-Flash`(快)
- localStorage 关键键:`qianwen-selectModel`(当前选中模型)、`lswucn`(umidtoken)、`itracingjs:dycf:66ur41cs-cntu1744`(x-wpk-bid)
- 无关安全头清单(浏览器发出但服务端不校验):`clt-acs-sign/request-params/reqt/bfg/caer`、`bx_et`、`bx-ua`、`bx-umidtoken`、`eo-clt-actkn/sacsft/snver/ve`、`x-wpk-reqid/traceid/bid/rel`、`x-chat-id/biz`、`x-csrf-token`、`sec-ch-ua*`、`XSRF-TOKEN`
