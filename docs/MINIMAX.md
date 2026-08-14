# MiniMax(agent.minimaxi.com)网页逆向接入 —— 直连通道

> 完成时间: 2026-08-14。**直连逆向**(不走 CDP 桥):MiniMax 风控中等,
> 签名可逆(明文盐在 JS bundle 里),无需真浏览器。实测全链路通过。

## 一、对外模型

| id | 变体 | 能力 |
|---|---|---|
| `minimax-m3-chat` | chat | 纯真人对话(普通模式,网页模型 MiniMax-M3) |
| `minimax-m3-coding` | coding | 客户端工具调用(围栏 JSON + FenceParser,实测可用) |

仅当 `MINIMAX_WEB_TOKENS` 指向的 token 文件非空时注册。

## 二、协议要点(2026-08-14 CDP 抓包 + Node/Go 直连实测)

- **认证**:`token`(JWT,`localStorage._token`),**同时放在请求头与 URL 查询参数**。
- **签名**:`x-signature = MD5(x-timestamp秒 + "I*7Cf%WZ#S&%1RlZJ&C2" + 请求体)`
  (盐为明文,存在于 JS bundle,已验证 100% 匹配)。
- **会话**:`POST /minimax-cloud/api/v1/agent/{agentId}/session`(带 `yy` 头,
  32hex 固定值)→ 返回 `session_id`。
- **发消息**:`POST https://agent-stream.minimaxi.com/.../session/{sid}/message`,
  响应为 SSE(`data:` JSON 行):
  - `type=10` 心跳;`type=2` 消息回显/最终完整消息(含 usage)
  - `type=6` `agent_message_chunk`:正文增量(`msg_content`),`finish:true` 结束
- **URL 公共参数(缺一 401)**:`device_platform/biz_id/app_id/version_code/unix/
  timezone_offset/sys_language/lang/uuid/device_id/os_name/browser_name/
  device_memory/cpu_core_num/browser_language/browser_platform/user_id/
  screen_width/screen_height/token/client/region`
  —— `uuid` 为页面实例级固定值(跨请求复用),`device_id`/`user_id` 为账号级
  数字参数(换账号需更新,从抓包提取)。
- **模型**:普通模式 `MiniMax-M3`(`variant:""` + `team_mode_off:true`);
  agent 模式为 `variant:"thinking"`(未接入)。

## 三、代码结构

```
internal/minimaxweb/client.go   — 直连客户端(签名/公共参数/会话/SSE 解析)
internal/provider/minimax.go    — Provider 入口(模型路由)
internal/provider/minimax_chat.go — chat + coding 变体
```

## 四、环境变量

```
MINIMAX_WEB_TOKENS=/work/.runtime/tokens/minimax_tokens.txt  # 每行一个 JWT
MINIMAX_DEVICE_ID=92622880       # 账号级 URL 参数(抓包提取)
MINIMAX_USER_ID=544661156126703622
MINIMAX_AGENT_ID=430731272630966 # 普通模式 agent,默认已内置
MINIMAX_MODELS=                  # 可选,默认 minimax-m3-chat/coding
```

## 五、限频与资源

- **限频**:与全局策略一致:chat 不限(真人使用);coding 限频 1.5s + rand(0~1.5s)。
- **Token Plan 配额**(2026-08-14 实测):免费账号有 Token Plan 用量上限,
  耗尽后**所有模式**(含普通对话)返回 `42212: 已达到 Token Plan 用量上限 (2056)`
  —— 需等配额重置(免费额度按周期发放,签到每日送 400 积分)或充积分/订阅。
  aurora 侧表现为上游空回复/错误,属账号限制,非代码问题。
- token 有效期约 38 天(JWT exp),过期需浏览器重新提取 `localStorage._token`。

## 六、Agent 团队模式(未接入,备忘)

- 页面三选项:**Agent 团队开关**(手动开启,消耗积分)/ 思考开关 / 模型选择。
- 团队模式(`team_mode:true` + `variant:"thinking"` + `enable_team:true`,
  agent_name 切换为 `430731272630967`)下,服务端编排多 Agent 团队
  (General/Coder/Verifier)执行工具链 —— **对客户端是黑盒**:
  执行期网络层只有 GET /session 轮询与消息拉取,无任何工具调用接口。
- 与 GLM 的 `execute_sandbox_code` 不同:**没有客户端可调用的原生工具协议**。
- 因积分成本与黑盒特性,不接入;如需接入需先逆向 agent 模式消息体字段
  (`enable_team` 等)并接受积分消耗。

## 七、验证

```bash
curl -N http://127.0.0.1:8080/v1/chat/completions -H "Content-Type: application/json" \
  -d '{"model":"minimax-m3-chat","messages":[{"role":"user","content":"用一句话介绍你自己"}],"stream":false}'
```
