# Gemini(gemini.google.com)网页逆向接入

2026-08-14 完成。本文记录协议实测结论、代码结构、账号提取与限频。

> ⚠️ **封号风险**:Google 反爬最强(动态令牌 + 行为分析)。必须严格限频
> (单账号并发 1、间隔 >=2s,代码已内置),只放可丢弃小号。

## 一、对外模型

| id | 变体 | 能力 |
|---|---|---|
| `gemini-3-flash-chat` | chat | 纯真人对话(模型自动用原生搜索/地图),**绝不注入工具信息** |
| `gemini-3-flash-coding` | coding | 云端能力助手 + 客户端工具调用(**实测可靠**,见 §四) |

默认目录见 `defaultGeminiModels`(GEMINI_MODELS 未配置时);仅当 `GEMINI_ACCOUNTS`
指向的账号 JSON 文件非空时 provider 才注册。

## 二、协议要点(CDP 抓包 + Node/Go 直连验证,2026-08-14)

### 认证(纯 cookie,无 Authorization/API key)

- cookie:SID / SAPISID / HSID 等(浏览器登录后提取)
- **`at` 令牌** = `window.WIZ_global_data.SNlM0e`(格式 `base64url前缀:时间戳`,会话级固定)
- **`SNlM6e` 大令牌**(~2.6KB,StreamGenerate f.req 内层 [3],会话级固定)
- **`f.sid`**(StreamGenerate URL 参数,会话级固定)
- 三者均**会话级固定可复用**(实测多次请求不变)

### 生成对话(StreamGenerate)

```
POST /[u/N]/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate
  ?bl=boq_assistant-bard-web-server_...&f.sid=<fsid>&hl=zh-CN&pageId=none
Content-Type: application/x-www-form-urlencoded;charset=UTF-8
body: f.req=<嵌套JSON>&at=<at>
```

f.req 内层 97 字段(骨架见 `geminweb/client.go` 的 `innerSkeleton`),动态位:
- `[0]` prompt:`[text, 0, null×4, 0]`
- `[2]` ids:`[cid, rid, rcid, null×6, "Aw..."]`
- `[3]` SNlM6e
- `[4]` uuid

### 响应(Google RPC 帧)

```
[["wrb.fr",null,"<JSON>"], ["wrb.fr",null,"<JSON>"], ...]   ← 可能多帧拼一行
```
- 文本帧 payload:`[null,[cid,rid],meta,null,[[rc_id,[text],...],...]]` → `data[4][0][1][0]`
- 结束帧 payload:`[null,[cid,rid],{"44":true,...}]`(3 元素,无 data[4])
- **结束帧可能与文本帧在同一行**(结束在前文本在后),解析须收集完整行

## 三、代码结构

```
internal/geminweb/
  client.go     — 客户端(StreamGenerate + RPC 帧解析 + 账号池 + 严格限频)
  client_test.go— 帧解析/限频/body 构造单测
  live_test.go  — 真实上游冒烟(GEMINI_ACCOUNT_FILE 环境变量)
internal/provider/
  gemini.go        — Provider 接口、模型路由(chat/coding)
  gemini_chat.go   — chat 变体(Responses + ChatCompletions)
  gemini_coding.go — coding 变体(FenceParser 工具通道)
  gemini_test.go   — 模型解析/chat 无工具注入/coding prompt
```

## 四、能力(实测)

| 能力 | 是否可靠 | 实测 |
|---|---|---|
| 原生搜索/地图 | ✅ | 问天气自动触发,回复带引用来源 |
| **客户端工具调用** | ✅ **可靠** | coding 变体带 tools 请求,返回标准 `tool_calls:[{name:"list_files",arguments:...}]`(与其他 provider 不同,优于智谱/Grok) |
| 多轮 | ✅ | 全量拍平 prompt 即可(同 DeepSeek),不依赖 rcid |
| 思考/推理 | ✅ | 模型自动深度思考 |

## 五、环境变量

```
GEMINI_ACCOUNTS=/work/.runtime/tokens/gemini_accounts.json  # 账号池 JSON(不入库)
GEMINI_MODELS=                                             # 可选,默认 gemini-3-flash-chat/coding
```

## 六、账号提取(一次性脚本,之后可复用)

1. 浏览器登录 gemini.google.com(小号)
2. CDP 提取三样:
   - **cookie**:`Network.getCookies` 全量(domain gemini.google.com + www.google.com)
   - **at**:`window.WIZ_global_data.SNlM0e`
   - **SNlM6e + f.sid**:发一条消息,hook 抓 StreamGenerate 请求,
     f.req 内层 [3] 是 SNlM6e,URL 的 `f.sid=` 是 fsid
3. 写入账号 JSON(每账号一个对象):
   ```json
   [{"cookie":"...","at":"...","snlM6e":"...","fsid":"f.sid=...","pathPrefix":"/u/1"}]
   ```
   `pathPrefix` 是 URL 在 `/_/BardChatUi` 前的账户路径(单账户可为 "")。

## 七、验证

```bash
# 本地单测
go test ./internal/geminweb/ ./internal/provider/

# 真实上游冒烟(严格限频,单次)
GEMINI_ACCOUNT_FILE=./tokens/gemini_accounts.json go test ./internal/geminweb/ -run TestLiveComplete -v

# 本地服务冒烟
GEMINI_ACCOUNTS=./tokens/gemini_accounts.json go run .
curl -N http://127.0.0.1:8080/v1/responses -H "Content-Type: application/json" \
  -d '{"model":"gemini-3-flash-chat","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"用一句话介绍你自己"}]}],"stream":true}'
```
