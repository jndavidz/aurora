# 腾讯元宝(yuanbao.tencent.com)网页逆向接入实测

> 逆向时间:2026-08-13(CDP 抓包 + bogdanfinn tls-client 复刻验证闭环)。
> 关联:`docs/ARCHITECTURE.md`、`docs/PROVIDER_ARCHITECTURE.md`(落点速查)、`docs/CDP_BROWSER_DEBUG.md`(抓包方法)。
> 实现:`internal/yuanbaoweb/` + `internal/provider/yuanbao*.go`,暴露模型 `hy3-*` / `yb-deepseek-*`。
> 页面地址:`https://yuanbao.tencent.com/chat/<agentId>`,agentId 默认 `naQivTmsDa`(元宝主 agent,hy3/deepseek 共用)。

---

## 〇、结论速览

| 项 | 值 |
|---|---|
| 建会话 | `POST /api/user/agent/conversation/create` body `{"agentId":"naQivTmsDa"}` → `{"id": cid}` |
| 聊天端点 | `POST /api/chat/{cid}`,`Content-Type: text/plain;charset=UTF-8`(body 是 JSON 字符串) |
| 认证 | 浏览器请求头**不带 Cookie**,认证靠 **`X-Uskey`** + **`X-ID`/`T-UserID`/`X-device-id`/`X-HY93`** 头(实测全量 /api/ 请求均无 Cookie 头);池里的 cookie 用于**派生 X-* 头**(`hy_user`→X-ID,`_qimei_uuid42`→X-device-id),不当作 Cookie 头发送 |
| 模型字段 | `model` 固定 `gpt_175B_0404`(agent 主模型标识);**真正模型由 `chatModelId` 决定**:`hunyuan_gpt_175B_0404`(Hy3)/`deep_seek_v3`(DeepSeek) |
| 联网搜索 | 模型均支持 `supportInternetSearch`;网页默认「自动联网搜索」(`supportFunctions:["openAutoSearchSwitch","autoInternetSearch"]` + extInfo 带 `internetSearch`) |
| 工具调用 | 网页 API **无原生 function calling**(coding 变体是文本协议注入式模拟,见 §五) |
| 多轮 | 每请求新会话(cid)+ 全量拍平 prompt,无服务端历史依赖(与 DeepSeek 网页通道一致) |
| WAF(TLS) | **必须 Chrome 指纹 TLS**(bogdanfinn tls-client `Chrome_146`);curl 可过,Go 标准库 / node 被 `stgw` 网关按 JA3 拦截(401/400) |
| 凭据有效期 | X-Uskey 随登录有效,浏览器登录态过期即失效(实测会话中途失效,需重新登录后提取) |

## 一、认证与 Token

- 两个凭据都从**已登录浏览器**抓取(见 `docs/CDP_BROWSER_DEBUG.md` 方法):
  - **`X-Uskey`**:网页 `/api/` 请求头的值(URL 编码长字符串)。页面 localStorage / cookie 里没有直接存储
    (JS 里是 `getUSKeySync("7800385", id, "h38=...&timestamp=...&platform=web")` 签名产物,见 `_app.*.js`),
    只能从真实请求头里抓。跨页面 reload **稳定**(同登录态),登录态过期后整体失效。
  - **`Cookie`**:`hy_token`(httpOnly,`.tencent.com`,长期)、`hy_user`、`_qimei_uuid42` 等,用 CDP
    `Network.getCookies` 全量拼出。**注意:浏览器实际请求不发 Cookie 头**(实测全量 /api/ 请求均无),
    池里的 cookie 是给客户端派生 `X-ID`(`hy_user`)、`X-device-id`/`X-HY93`(`_qimei_uuid42`)用的——
    这组 X-* 头与 X-Uskey 一起构成请求身份,缺了会 401。
- **token 池文件 `tokens/yuanbao_tokens.txt`**:每行一条 `X-Uskey\tCookie header`(Tab 分隔;cookie
  用于派生 X-* 头)。示例(已脱敏):
  ```
  DCAxFeb05...%0A...=	hy_token=<t>; hy_user=<uid>; hy_source=web; _qimei_uuid42=<dev>; ...
  ```
  提取脚本:`node scripts/capture-yuanbao-token.mjs`(驱动页面发一条消息,抓实时请求头 + 兜底
  getCookies 拼 cookie)。
- **登录态会过期**:实测会话中途失效(页面变「未登录」或账号被风控冻结),此时所有 API 返回
  `{"error":{"code":"23000","message":"登录已过期，请重新登录"}}`。需在浏览器重新登录
  (扫码/验证码)后重新抓取更新池文件。
- **风控冻结风险(2026-08-14 实踩)**:高频探针(同凭据快速 create+chat 循环、curl 复刻)触发了账号
  风控冻结。**活体测试务必用可丢弃小号,主号永不入池**(架构文档 §八 规则)。联调用网关本身少量
  请求(3 次内)实测无问题。
- **⚠️ 直连通道已停用(2026-08-22)**:直连逆向(bogdanfinn TLS 指纹模拟)累计风控 2 个账号。
  正式通道改为 **CDP 桥**(见 §七·五),**不要再启用直连**。

## 七·五、CDP 桥通道(2026-08-22,正式推荐)

**动机**:直连逆向的 TLS 指纹模拟 + 高频探针是风控源(已冻结 2 账号)。桥方案让请求由
**真实浏览器页内 fetch** 发出 —— 同源自动带 cookie、浏览器原生指纹、零签名逆向,风控暴露与
真人操作一致。

- **实现**:`scripts/cdp/bridge.mjs` 的 `hunyuan` 适配器 + `internal/provider/hunyuan_cdp.go`
  (`hunyuan-hy3-chat`)。每次请求:页面上下文 `create` 会话 → `chat` 重放(模板 body 只改
  prompt/displayPrompt/conversationId),认证头会话级复用。
- **认证头捕获**:`node scripts/cdp/capture-yuanbao.mjs`(用户手动发一条消息时抓取
  X-Uskey 等头 + chat body 模板,存 `.runtime/bridge/yuanbao_headers.json`)。
  登录态过期(接口 23000)时重新登录后重抓。
- **保护措施**(账号宝贵):
  1. 限频最保守:`hunyuan_cdp.go` chat 也限频(5s + 5s 抖动),高于其它 provider;
  2. 桥只在调用时由 keeper 唤醒,平时不驻留、不自动操作页面;
  3. 多轮靠每次新会话 + 全量拍平 prompt(与直连时代一致),不依赖服务端历史。
- **注意**:会话头与会话绑定?实测 create+chat 用捕获头正常;若某天 401/21007 持续,
  重新跑 capture-yuanbao.mjs 刷新即可。

## ⛔ 混元永久停用(2026-08-22,第三账号冻结)

**第三个账号也被冻结。** 桥方案(页内 fetch 重放)仍被腾讯识别:
- 每次请求 `create` 新会话的请求模式与真人差异大(真人同会话连续对话,自动化是快速
  create→chat 对 + 会话列表膨胀);
- 登录态过期(23000)后重新登录 + 立即自动化请求,是风控高危触发点;
- 腾讯对元宝的自动化检测远严于 Gemini/Claude,任何非 UI 交互的请求模式都风险极高。

**结论与规则**:
1. **混元不再接入**(`docker-compose.nas.yml` 已移除 `HUNYUAN_CDP_URL` 注册);
2. `internal/provider/hunyuan_cdp.go` 与桥的 `hunyuan` 适配器保留代码(不注册即不触发),
   若未来要恢复,必须实现**真人 UI 交互模拟**(同一会话续聊、慢节奏、带页面交互),禁止
   create+chat 快速模式;
3. **血的教训**:对风控敏感的平台,接入前先做"真人行为差异"分析,测试请求务必克制
   (此次我多次部署验证产生异常请求节奏,负主要责任)。

## 二、请求

### 2.1 建会话

```http
POST https://yuanbao.tencent.com/api/user/agent/conversation/create
Content-Type: application/json
# 头:UA / Origin / Referer / X-Uskey / Cookie / X-ID / T-UserID / X-device-id / X-HY93 /
#     X-Instance-ID:5 / X-WebVersion / X-Web-Third-Source:main / X-AgentID / Accept-Encoding:gzip

{"agentId":"naQivTmsDa"}
```
→ `200 {"id":"0PaXXXXXX"}`(即 cid,后续 chat 端点路径与 body 里的 conversationId 都用它)。

### 2.2 聊天(SSE)

```http
POST https://yuanbao.tencent.com/api/chat/{cid}
Content-Type: text/plain;charset=UTF-8
Accept: text/event-stream, application/json, text/plain, */*
X-Event-Input-Type: 11
X-Trid-Channel: undefined
# 其余头同 2.1
```
body(JSON 字符串;完整字段见 `internal/yuanbaoweb/client.go` 的 `chatBody`):

```json
{
  "model": "gpt_175B_0404",
  "prompt": "<拍平的全量 prompt>",
  "plugin": "Adaptive",
  "displayPrompt": "<同上>",
  "displayPromptType": 1,
  "agentId": "naQivTmsDa",
  "isTemporary": false,
  "projectId": "",
  "chatModelId": "hunyuan_gpt_175B_0404",
  "supportFunctions": ["openAutoSearchSwitch", "autoInternetSearch"],
  "docOpenid": "",
  "options": {"imageIntention": {"needIntentionModel": true, "backendUpdateFlag": 2, "intentionStatus": true}},
  "multimedia": [],
  "supportHint": 1,
  "chatModelExtInfo": "{\"modelId\":\"hunyuan_gpt_175B_0404\",\"subModelId\":\"\",\"supportFunctions\":{\"internetSearch\":\"\"},\"internetSearch\":\"autoInternetSearch\"}",
  "applicationIdList": [],
  "chatSource": "prompt",
  "version": "v2",
  "extReportParams": null,
  "isAtomInput": false,
  "conversationId": "<cid>",
  "offsetOfHour": 8,
  "offsetOfMinute": 0
}
```

关键点:
- **`model` 与 `chatModelId` 是两回事**:`model` 固定 `gpt_175B_0404`(agent 路由),`chatModelId` 选模型。
  实测 `model` 用 `deep_seek_v3` 反而 401;`model=gpt_175B_0404` + `chatModelId=deep_seek_v3` 才通。
- 关联网搜索时:`supportFunctions` 置空数组,`chatModelExtInfo` 用 `{"modelId":"...","subModelId":"","supportFunctions":{}}`。

## 三、SSE 响应

`data:` 帧混合,内容增量是 `{"type":"text","msg":"..."}`(逐帧小段,UTF-8 增量直出):

```
data: {"type":"text"}                                   ← 开局哨兵(无 msg,跳过)

event: speech_type                                      ← 语音帧,忽略
data: status

data: {"type":"text","msg":"我是"}                       ← 内容增量
data: {"type":"text","msg":"元宝"}

data: {"type":"tips","status":0,"internetFlag":0,"targetFunctionId":"autoInternetSearch"}   ← 搜索提示,忽略
data: [plugin: ]                                        ← 内部标记,忽略
data: [MSGINDEX:10]

data: {"type":"meta","messageId":"...","stopReason":"stop","endConv":false,"tokenUsageInfo":{"promptTokens":..,"completionTokens":..,"totalTokens":..},...}  ← 元数据(含 token 用量),忽略

data: [TRACEID:...]                                     ← 内部标记,忽略
data: [DONE]                                            ← 结束
```

- meta 帧 `chainID` 反映实际模型:`"Adaptive"`(hy3 主链)/`"DeepSeekV3"`;`oneAgentId` 为
  `main_agent_hy_for_pc` / `main_agent_deepseek` —— 联调时可用它确认切换到了 deepseek。
- 错误帧:`data: {"type":"error","msg":"回答拉取失败，正在重试","code":"21007","error":{}}`
  (浏览器偶发,重试即可)。

## 四、模型目录(网页 ModelInfoCacheV1 实测)

| 网页模型 | `chatModelId` | `model` | 能力 |
|---|---|---|---|
| Hy3 | `hunyuan_gpt_175B_0404` | `gpt_175B_0404` | 全能对话 + 联网搜索(`supportInternetSearch`) |
| DeepSeek | `deep_seek_v3` | `gpt_175B_0404` | 同上 + 深度思考定位 |

暴露 id(带 `-chat`/`-coding` 后缀路由,前缀保护防误匹配):
`hy3-chat`、`hy3-coding`、`yb-deepseek-chat`、`yb-deepseek-coding`(`yb-` 与 chat.deepseek.com 的
现有 deepseek 模型区分)。能力标注:`-chat` → `web_search`(+`reasoning`,deepseek);`-coding` → `function_call`。

## 五、coding 变体(文本协议工具调用)

元宝网页 API 无原生 function calling;coding 变体走 aurora 通用文本协议:把客户端 tools 注入
system prompt,引导模型输出 `<tool_call>{...}</tool_call>` 标签块,流式用 `toolcall.Parser` 切分。
用 `toolcall.DefaultTags`(ChatGPT 式标签);**2026-08-14 小号联调实测 hy3 与 deepseek 都正常跟随**——
请求 "用 bash 命令列出当前目录文件" 两个模型均返回
`tool_calls:[{"name":"bash","arguments":"{\"command\":\"ls -la\"}"}]`,`finish_reason:"tool_calls"`。
coding 变体关联网搜索,保持工具上下文干净。

## 五·五、联调实测记录(2026-08-14,小号,共 4 次上游请求)

| # | 模型 | 表面 | 结果 |
|---|---|---|---|
| 1 | `hy3-chat` | `/v1/chat/completions` 非流式 | 200,正常 JSON |
| 2 | `yb-deepseek-chat` | `/v1/responses` 流式 | 200,事件序 `created → output_item.added → output_text.delta → output_item.done` 正确 |
| 3 | `hy3-coding`(带 tools) | `/v1/chat/completions` 非流式 | 200,`tool_calls` 解析正确(见 §五) |
| 4 | `yb-deepseek-coding`(带 tools) | `/v1/chat/completions` 非流式 | 200,`tool_calls` 解析正确(见 §五) |

延迟 1.2-1.9s,网关日志无错误。四个模型 × 双变体全部覆盖。

## 六、配置

```bash
# env.template 段(详见该文件):
YUANBAO_MODELS=hy3-chat,hy3-coding,yb-deepseek-chat,yb-deepseek-coding
YUANBAO_WEB_TOKENS=tokens/yuanbao_tokens.txt   # 每行 <X-Uskey>\t<Cookie header>
YUANBAO_WEB_BASE=https://yuanbao.tencent.com
YUANBAO_AGENT_ID=naQivTmsDa
YUANBAO_PROXY=
```

`docker-compose.nas.yml` 已加 `YUANBAO_WEB_TOKENS=/work/.runtime/tokens/yuanbao_tokens.txt`
(只读挂载,与其它 provider 同卷)。仅当池文件配置时 `Yuanbao` provider 才注册(`router.go`)。

## 七、风险与后续

- **登录态过期**:凭据需定期更新(重登后抓包重提)。池文件支持多账号轮换(`NextToken` + 失败重试)。
- **TLS 指纹**:必须 Chrome_146 指纹客户端,标准库直连被 stgw 拦截(JA3)。
- **`X-Bus-Params-Md5`/`X-Timestamp`**:`_app` bundle 里存在请求签名配置(部分接口需要),
  **聊天接口实测不需要**;若后续其它端点被拒再补。
- 识图(`multimedia` 上传走 COS + SHA1/HMAC 签名)未接入,`CapVision` 未标注。
