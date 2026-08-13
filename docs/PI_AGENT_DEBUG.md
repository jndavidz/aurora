# pi agent 连 NAS aurora 排障记录(2026-08-13)

> 状态:**调查完成,修复已就绪但未部署**(部署动作被用户叫停,以本文档交接给新对话)。
> 关联:`docs/NAS_DEPLOYMENT.md`(部署方案)、`docker-compose.nas.yml`(NAS 容器编排)。

---

## 一、背景

- NAS(群晖 DS416play,10.10.10.2)上 aurora 以 **本地构建 local-toolfix 镜像**(`aurora:local-toolfix`)运行,
  compose 见 `docker-compose.nas.yml`,宿主机映射 **65432 → 8080**。
- 客户端由 **pi agent(pi-web)** 使用,Base URL `http://10.10.10.2:65432/v1`,Model `auto`。
- 此前本地 aurora 服务的是 **ZCode**;现在 NAS 上服务的是 **pi agent** —— 这一差异是慢的关键。

## 二、症状链

1. 404:`Error: OpenAI API error (404): 404 404 page not found`
2. 修好 404 后能对话,但**单轮 45 秒+**,pi 界面长时间"正在等待模型..."

## 三、问题 1:404 根因与修复

### 3.1 现象

容器日志(`docker logs aurora`)显示客户端实际请求路径:

```
[GIN] POST "/v1/models/responses" → 404
```

### 3.2 根因

aurora 路由(`internal/handler/router.go`)只注册了 `POST /v1/responses`(**没有** `/v1/models/responses`)。

pi 配置文件 `C:\Users\david\.pi\agent\models.json` 中 Aurora 条目:

```json
"Aurora": {
  "api": "openai-completions",
  "baseUrl": "http://10.10.10.2:65432/v1",
  "models": [{ "id": "auto", "api": "openai-responses" }],  // ← 模型级 api 覆盖,顶掉了 provider 级
  "apiKey": "david"
}
```

模型 `auto` 有**模型级 `api: "openai-responses"` 覆盖**,pi 因此走 Responses 适配器,
请求路径变成 `/v1/models/responses`(pi 的 responses 适配器路径),而 aurora 不认 → 404。

### 3.3 修复(已改)

删除模型级 `api` 覆盖,让模型继承 provider 级的 `openai-completions`:

```json
"Aurora": {
  "api": "openai-completions",
  "baseUrl": "http://10.10.10.2:65432/v1",
  "models": [{ "id": "auto" }],
  "apiKey": "david"
}
```

验证:`POST /v1/chat/completions`(pi 走 completions 适配器后的真实路径)返回 200;
`POST /v1/responses` 也 200(aurora 两套接口都通)。
原始文件备份:`~/.pi/agent/models.json.bak.20260813111208`。

## 四、问题 2:反应慢(45s)根因

### 4.1 现象

容器日志(`docker logs aurora`)关键行:

```
[chatgpt] no tool call in reply (attempt 1/5), retrying
[chatgpt] no tool call in reply (attempt 2/5), retrying
[chatgpt] no tool call in reply (attempt 3/5), retrying
[chatgpt] no tool call in reply (attempt 4/5), retrying
[GIN] POST "/v1/chat/completions" | 45.03s
```

每次对话触发 **4 次重试**(每轮上游调用 ~10s + 退避 1+2+4+8s)= 45s。
另有一次 `413 history=263055 chars > 100000`(见 §6)。

### 4.2 机制链(源码定位)

1. **pi 是 coding agent,请求总是带 tools 定义**(bash/read/write 等 pi 自己的工具)。
2. `toolCallingEnabled()`(`internal/handler/shared.go:227-232`):
   ```go
   if cfg != nil && !cfg.ToolCallingEnabled { return false }
   return len(tools) > 0   // ← 只要请求带 tools 就进入工具调用分支
   ```
3. `TOOL_CALLING_ENABLED=true`(compose 设置)→ 带 tools 的请求进入 `handleToolCalling`
   (`internal/handler/chat_handler.go:668`)。
4. `handleToolCalling` 期望模型输出 **`<tool_call>{...}</tool_call>` 文本块**(local-toolfix
   为 ZCode 定制的协议,靠 SYSTEM OVERRIDE 提示词引导,`chat_handler.go:732/751`)。
5. pi 的普通对话("你好")时,模型直接回纯文本、不输出 `<tool_call>` 块 →
   `chat_handler.go:799-805` 解析不到 calls → 走到重试分支。
6. 重试循环条件(`chat_handler.go:828-868`):仅当**最后一条消息是 tool/function 结果**且
   文本非空非停顿时才 break(`chat_handler.go:846`);否则重试,次数由
   `maxRefusalRetries = cfg.RefusalRetries`(`chat_handler.go:674`)控制。
7. compose 设 `REFUSAL_RETRIES=5` → 每轮对话最多 5 次尝试(实测 4 次重试)。

### 4.3 关键结论

- **本地 ZCode 没问题**:ZCode 请求引导模型输出 `<tool_call>` 块,命中工具调用分支,
  重试机制是"防模型绕开工具"的合理防护。
- **pi 场景不适用**:pi 普通对话模型纯文本回答是正常行为,却被当"绕开工具"反复重试。
- **`result.ToolCalls` 从未被赋值**:`internal/chatgpt/conversation.go:353` 定义了
  `HandlerResult.ToolCalls` 字段,但全仓库 grep 无一处赋值 —— aurora **不解析上游标准
  tool_calls JSON**,只识别 `<tool_call>` 文本块。这是与 pi(标准 OpenAI 客户端)协议的
  根本错位,但本次用 REFUSAL_RETRIES=1 可绕开(见 §5),不动协议本身。

## 五、已做的修复(未部署)

`docker-compose.nas.yml` + `docs/NAS_DEPLOYMENT.md`:

```yaml
- REFUSAL_RETRIES=1   # 原来是 5
```

原理:`maxRefusalRetries=1` 时,`chat_handler.go:849` `if attempt >= maxRefusalRetries-1 { break }`
让循环只跑 1 次就退出 —— 普通对话立即返回;**工具调用不受影响**(模型输出 `<tool_call>` 块时
`chat_handler.go:812` `len(calls) > 0` 直接 break,不依赖重试次数)。

代价:放弃 ZCode 式"模型拒绝工具时多次提示重试"的防护。NAS 现在只服务 pi(本地 ZCode 已停,
双端互斥),可接受;若未来 NAS 也要服务 ZCode,需按客户端分流。

## 六、遗留问题(新对话可继续)

1. **部署验证**:改动未部署。下一步在 PC 跑 `cd /d/repos/aurora && ./scripts/deploy_nas.sh`
   (构建缓存命中,秒级),再验证 pi 对话延迟。
2. **413 history 过大**:`internal/handler/chat_handler.go:683` 硬编码
   `const maxHistoryChars = 100000`;pi 长历史会话(实测 263055 字符)直接 413。
   若要支持 pi 的长任务,需调大该上限(重新构建镜像)或 pi 侧精简上下文。
3. **标准 tool_calls 协议**:aurora 只认 `<tool_call>` 文本块,不解析上游标准 tool_calls
   (`HandlerResult.ToolCalls` 字段闲置)。pi 作为标准 OpenAI 客户端若发起真实工具调用,
   可能仍不通 —— 但本次只验证了普通对话,pi 工具调用场景未实测。
4. **REFUSAL_RETRIES 双重读取**:`internal/conversationflow/flow.go:184` 也独立读
   `REFUSAL_RETRIES` 环境变量(默认 3),与 `config.go` 路径不一致;改动环境变量时两处都生效。

## 七、操作备忘

- 部署:PC 上 `./scripts/deploy_nas.sh`(tar→ssh→清空部署目录→compose up -d --build→curl 探活)。
- 容器日志:`ssh -o BatchMode=yes zxsadmin@10.10.10.2 "/usr/local/bin/docker logs aurora"`。
- 端口验证:`/usr/local/bin/docker port aurora 8080`(应显示 `0.0.0.0:65432`)。
- 直连测试(带鉴权):
  ```bash
  curl -s -X POST http://10.10.10.2:65432/v1/chat/completions \
    -H "Authorization: Bearer david" -H "Content-Type: application/json" \
    -d '{"model":"auto","messages":[{"role":"user","content":"hi"}]}'
  ```
