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

## 八、CDP 真浏览器执行通道(2026-08-14 实测,推荐替代直连)

> **定位**:Google 反爬最强(动态令牌 + 行为分析),手搓指纹(`browserfp`/`fingerprint`)
> 永远追不上真实浏览器。改用**操作真实浏览器**执行:请求从已登录真实页面的
> JS 上下文用 `fetch()` 发出 —— cookie/TLS/指纹/JS 运行时全部由浏览器自带(零模拟),
> 流式响应经 console 逐块回传,再转成 OpenAI SSE。**已全链路实测通过**(2026-08-14)。

### 架构

```
Chrome for Testing 152(干净独立 profile,--remote-debugging-port=9222)
   │  CDP(Runtime.evaluate + Network 监听)
   ▼
scripts/cdp/bridge.mjs(Node 零依赖,127.0.0.1:8799;NAS 转发场景 0.0.0.0)
   │  OpenAI 兼容 /v1/chat/completions(流式 + 非流式)
   ▼
aurora 网关(NAS,65432) —— GEMINI_CDP_URL=http://<PC>:8799,只做 HTTP 转发
   │  /v1/models 出现 gemini-3-flash-chat,客户端单入口
   ▼
客户端(pi/zcode/aurora-chat.html)
```

家庭 PC 跑桥 + 浏览器(需要真核显做真实 WebGL 指纹);NAS 经 `GeminiCDP` provider
(`internal/provider/gemini_cdp.go`)转发,不动其余直连通道。
**不可放 NAS**:DS416play 内核 3.10 跑不动现代 Chromium,且无 GPU → SwiftShader
软件渲染恰是 Google 认 bot 的特征。

> NAS 侧只需在 compose 配置(已内置 docker-compose.nas.yml):
> `GEMINI_CDP_URL=http://<PC_IP>:8799`(桥需监听局域网:start-gemini.ps1 默认
> `BRIDGE_HOST=0.0.0.0`;`GEMINI_CDP_KEY` 与桥的 `BRIDGE_AUTH` 一致时启用鉴权)。
> 注意 PC 的 IP 需固定(或改 compose),桥不在线时 gemini 请求返回 502。

### 组件(scripts/cdp/)

| 文件 | 职责 |
|---|---|
| `bridge.mjs` | 常驻桥服务:适配器路由、限频队列(串行+>=2.1s)、令牌自续、SSE 输出 |
| `capture-streamgenerate.mjs` | 引导:挂 Network 监听,从一条真实请求提取会话令牌 |
| `gemini-replay.mjs` | 一次性调试:页内 fetch 复刻 + RPC 帧解析(不常驻) |
| `cdp-helper.mjs` | 零依赖 CDP over WebSocket 客户端(所有脚本共用) |

### 启动流程(家庭 PC)

```bash
# 1. 独立 profile 启动 Chrome for Testing(只登小号,勿与大号浏览器共用 profile)
"D:\PortableApps\_net\Chrome for Testing\chrome.exe" \
  --remote-debugging-port=9222 \
  --user-data-dir=D:\PortableApps\_net\chrome-cdp\profile \
  --disable-extensions --disable-sync --disable-background-networking \
  --disable-component-update --no-first-run --no-default-browser-check

# 2. 浏览器里登录 gemini.google.com(可丢弃小号)

# 3. 首次引导:抓一条真实请求提取会话令牌(用户手动发一条消息即可)
node scripts/cdp/capture-streamgenerate.mjs 120   # 挂监听
#    → 浏览器里发一条消息 → 监听器写 %TEMP%/gem_capture.json

# 4. 启动桥(自动导入令牌,之后每次请求自动续令牌,无需再手动抓)
node scripts/cdp/bridge.mjs
```

> **按需启动(推荐)**:通道是低频备用,不需要常驻。登录态在 Chrome profile 磁盘、
> 令牌在 `.runtime/bridge/gemini_session.json` 磁盘,随起随用:
> - 一键起:`powershell -File scripts/cdp/start-gemini.ps1`(自动起 Chrome + 桥,
>   检测 9222 被占会提示,如 Min 抢端口需先关 Min;Chrome 已在运行则复用)
> - 停止:Ctrl+C 停桥 + 关闭 Chrome 窗口;或什么都不做 —— **30 分钟无对话活动
>   自动停止**(桥经 CDP `Browser.close` 优雅关闭整个 Chrome 后退出;
>   `/health`、`/v1/models` 不算活动,监控探针不会阻止休眠;
>   `IDLE_TIMEOUT_MIN` 可调,0=关闭自动停止)
> - **实测启动耗时**(i3-12100T + SSD,profile 已初始化):Chrome 冷启动 **~0.9s**、
>   桥就绪 **~0.7s**,一键起总耗时 ~1.5s,秒级可用
> - **令牌过期自愈**:起桥后若对话报 `token_stale`(BardErrorInfo 1096/1157),
>   在浏览器页面上**手动发任意一条消息**,桥的 Network 监听自动刷新令牌,重试即可;
>   无需重跑第 3 步引导。若页面已掉登录,重新登录后再发一条消息。

### 桥的端点

| 端点 | 说明 |
|---|---|
| `GET /health` | 浏览器连接 / 令牌 / 账号状态 |
| `GET /v1/models` | 桥自身目录(桥只当纯 chat 用;coding 变体由 aurora 侧实现) |
| `POST /v1/chat/completions` | OpenAI 兼容;`stream:true` 走 SSE;多轮用 messages 数组全量拍平 |

环境变量:`BRIDGE_PORT`(默认 8799)、`BRIDGE_HOST`(默认 127.0.0.1,NAS 转发设 0.0.0.0)、
`BRIDGE_AUTH`(可选 Bearer 鉴权,与 aurora `GEMINI_CDP_KEY` 一致)、`CDP_PORT`(默认 9222)、
`IDLE_TIMEOUT_MIN`(默认 30,无对话活动自动停止分钟数;0=关闭)、
`MIN_INTERVAL_MS`/`JITTER_MS`(限频,默认 2000/1500)。
令牌缓存:`.runtime/bridge/gemini_session.json`(已 gitignore)。

### 限频策略(chat 不限,coding 限)

用户拍板的全局策略:**chat 不限频**(真人使用,天然有人类节奏);
**只对 coding 限频**(agent 连发工具调用是风控/封号主因)。

- **Gemini coding**(`gemini-3-flash-coding`):aurora 侧 `CodingLimiter` ——
  全局串行,间隔 = **基础 2s + 随机抖动 0~1.5s**(`internal/provider/gemini_cdp.go`)。
- **桥侧默认不限**:`MIN_INTERVAL_MS` 默认 0(chat 自由通过,仅串行防并发冲突);
  如客户端直连桥且需要桥侧限频,再设 `MIN_INTERVAL_MS=2000`/`JITTER_MS=1500`。
- 其余 provider 的 coding 变体同样限频(见 `docs/ARCHITECTURE.md` §7.4):
  ChatGPT 2s+0~2s、Grok 2s+0~1.5s、DeepSeek/GLM/Kimi 1.5s+0~1.5s。

### 多桥(桥池)

`GEMINI_CDP_URL` 逗号分隔多桥:轮询 + 故障转移 —— 某桥离线/5xx 熔断 60s
快速跳过(3s 拨号超时,不会挂几十秒),如办公室桥关机时自动全走家庭桥。
桥数 = 小号数 = 吞吐翻倍,但**每桥必须登录不同小号**(同号双端互踢)。

### 变体(-chat / -coding)

- `-chat`:请求原样转桥(纯对话,模型自动用原生搜索/地图)。
- `-coding`:工具调用 —— **aurora 侧**注入工具指令 prompt(复用直连时代
  gemini_coding.go 的围栏 JSON 协议 + FenceParser),桥只当纯 chat 用。
  实测:tool_calls 触发、流式 name/arguments delta、工具结果回填多轮闭环全通。

### Chrome 启动减配(已内置 start-gemini.ps1)

| 参数 | 作用 | 是否影响真指纹 |
|---|---|---|
| `--disable-sync` `--disable-background-networking` `--disable-component-update` | 关同步/后台网络/组件更新(省内存 CPU 大头) | 否 |
| `--disable-background-mode` | 关窗即全退,不留后台进程(配合自动停止) | 否 |
| `--renderer-process-limit=4` | 限制渲染进程数 | 否 |
| `--disk-cache-size=100M` | 限制 profile 磁盘增长 | 否 |
| `--disable-crash-reporter` `--noerrdialogs` `--disable-logging` | 关噪声组件 | 否 |
| ~~`--headless`~~ / ~~`--disable-gpu`~~ | **禁止加** | headless 的 WebGL 走 SwiftShader 软件渲染 = bot 信号 |

引擎本体(Blink+V8)已是最小可用集,进一步减重的空间在引擎之外(上表已做完);再减
只能换内核更轻的浏览器,但那些(Gecko 系)不支持 CDP。

### 令牌自续

桥每次页内 fetch 都同时挂 `Network.requestWillBeSent` 监听,**抓自己的
StreamGenerate 请求**刷新 `at`/`SNlM6e`/`f.sid`/jspb 头(会话级令牌无需人工重抓,
浏览器保持登录即可)。用户手动在页面里聊天也会顺带刷新。

### 资源占用与限制

- Chrome 152 headful ~250-400MB + 桥 ~50-80MB,全部在 PC;NAS 零影响
- 限制:单通道串行 + 2s+抖动间隔(防封号);一 profile 一账号,
  多账号 = 多桥(不同 PC)或多浏览器上下文

### 协议更新(2026-08-14 抓包,`internal/geminweb/client.go` 已滞后)

直连通道若恢复,须按以下实测更新(否则 BardErrorInfo 1096/1157):

1. **新 jspb 头 4 条**(缺头报 1096):
   `x-goog-ext-525005358-jspb`(与会话 UUID 同值)、`x-goog-ext-525001261-jspb`、
   `x-goog-ext-73010989-jspb`、`x-goog-ext-73010990-jspb`
2. **URL**:`_reqid=<随机>&rt=c` 替代 `pageId=none`;Referer 为 `https://gemini.google.com/`
3. **ids 格式**:`["c_<cid>","r_<rid>","rc_<rcid>",null×6,"Aw..."]`(`c_/r_/rc_` 前缀,
   `Aw...` 值已变);**rcid 必须 `rc_` 格式或空**,塞错格式报 1157
4. 会话级固定仍成立:`at`/`SNlM6e`(~2.6KB)/`f.sid` 实测多次复用不变
