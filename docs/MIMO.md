# Mimo(aistudio.xiaomimimo.com)网页逆向接入 —— 直连通道

> 完成时间: 2026-08-14。**直连逆向**(不走 CDP 桥):Mimo 认证简单
> (登录 cookie + URL 参数),无需签名。chat/coding/ASR 三接口实测全通。

## 一、对外模型

| id | 变体 | 能力 |
|---|---|---|
| `mimo-v2.5-pro-chat` | chat | 纯真人对话(网页模型 mimo-v2.5-pro;`<think>` 思考段自动过滤) |
| `mimo-v2.5-pro-coding` | coding | 客户端工具调用(围栏 JSON + FenceParser,实测可用) |
| `mimo-v2.5-asr` | asr | 语音识别(经 `/v1/audio/transcriptions`,model 填 mimo-v2.5-asr) |

仅当 `MIMO_WEB_TOKENS` 指向的 token 文件非空时注册。

## 二、协议要点(2026-08-14 CDP 抓包 + 直连实测)

- **认证**:登录 cookie `xiaomichatbot_ph`/`xiaomichatbot_serviceToken`/`userId`。
  token 文件每行一个**完整 Cookie 串**(`xiaomichatbot_ph="..."; ...; userId=...`);
  客户端从中提取 ph 值作 URL 查询参数,并把整串放 Cookie 头。
- **Chat**:`POST /open-apis/bot/chat?xiaomichatbot_ph=<urlencode(ph)>`
  body `{msgId, conversationId, query, isEditedQuery:false, modelConfig:{enableThinking,
  webSearchStatus, model}, multiMedias:[]}`。
  - conversationId 前端生成(32hex),每轮新建(多轮靠拍平 prompt)
  - 响应 SSE:`event:message` + `data:{"type":"text","content":...}` 正文增量;
    `<think>...</think>` 思考段剔除;`event:finish`(`[DONE]`)结束;
    `event:usage` 用量
- **ASR**(`/v1/audio/transcriptions` model=mimo-v2.5-asr)四步:
  1. `POST /open-apis/resource/genUploadInfo {"fileName":"x.mp3"}`(页面统一 .mp3 命名)
     → `{resourceUrl(下载签名), uploadUrl(上传签名)}`
  2. `PUT uploadUrl` 音频字节(Content-Type: application/octet-stream;
     **必须用 uploadUrl**,用 resourceUrl 会 403 Signature Does Not Match)
  3. `POST /open-apis/chat/conversation/save` 建会话(conversationId 前端生成,
     否则 recognize 报 conversation not exist)
  4. `POST /open-apis/asr/recognize {conversationId,msgId,audioUrl:resourceUrl,
     language:"auto",modelConfig:{modelCode:"mimo-v2.5-asr"}}` → taskId(数字)
  5. 轮询 `GET /open-apis/asr/recognizeStatus?taskId=` → status=success 时
     data.text 为识别文本

## 三、代码结构

```
internal/mimoweb/client.go        — 直连客户端(chat SSE/think过滤 + ASR 四步)
internal/provider/mimo.go         — Provider 入口(chat/coding/asr 路由)
internal/provider/mimo_chat.go    — chat + coding 变体
internal/handler/audio_handler.go — /v1/audio/transcriptions 的 mimo 分支
```

## 四、环境变量

```
MIMO_WEB_TOKENS=/work/.runtime/tokens/mimo_tokens.txt  # 每行一个完整 Cookie 串
MIMO_MODELS=                      # 可选,默认 mimo-v2.5-pro-chat/coding/asr
```

## 五、限频

与全局策略一致:chat 不限(真人使用);coding 限频 1.5s + rand(0~1.5s)。
cookie 有效期未实测(小米登录态,通常数天~数周),失效需浏览器重新提取 Cookie 串。

## 六、验证

```bash
# chat
curl http://127.0.0.1:8080/v1/chat/completions -H "Content-Type: application/json" \
  -d '{"model":"mimo-v2.5-pro-chat","messages":[{"role":"user","content":"1+1等于几"}],"stream":false}'

# asr
curl http://127.0.0.1:8080/v1/audio/transcriptions \
  -F "file=@test.wav;type=audio/wav" -F "model=mimo-v2.5-asr"
```
