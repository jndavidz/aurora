# 全模型凭证有效期与保活策略总览

> 更新时间: 2026-08-14。汇总 aurora 各 provider 的密钥凭证类型、有效期与保活机制。
> 凭证文件统一在 `.runtime/tokens/`(gitignore 已排除),NAS 挂载为只读。

## 总表

| provider | 凭证 | 文件 | 有效期 | 保活/续期策略 |
|---|---|---|---|---|
| **ChatGPT** | access_token(JWT) | `access_tokens.txt` | 会话级(几小时~1天) | **自动**:后台健康检查每 10 分钟检查,过期时用 refresh/session 换发 |
| | refresh_token | `refresh_tokens.txt` | 长期(数周~月) | 自动换发 access_token |
| | session_token | `session_tokens.txt` | 会话级 | 自动换发 access_token |
| **DeepSeek** | userToken(localStorage) | `deepseek_tokens.txt` | 会话级(实测稳定) | 无自动保活;失效手动重抓浏览器 localStorage |
| **GLM** | chatglm_refresh_token(JWT) | `glm_tokens.txt` | **~90 天** | **自动**:换发 access_token(JWT ~2h),refresh 轮换 |
| **Kimi** | refresh_token(JWT) | `kimi_tokens.txt` | **~90 天** | **自动**:换发 access_token(~15 分钟),refresh 轮换 |
| **Grok** | cookie 串(uid\|cookie) | `grok_cookies.txt` | 会话级;有 `usage_limit_reached` 配额 | 无自动保活;超限/失效手动重抓 cookie |
| **豆包** | cookie + 签名参数 | `doubao_accounts.json` | `a_bogus` **分钟级**;其余会话级 | **保活被否决**(成本高);低频备用,失效重抓 |
| **千问** | tongyi_sso_ticket(cookie) | `qianwen_tokens.txt` | **~1 年** | 无自动保活 |
| | x5sec 通关 cookie | 同上(同文件) | **~20 分钟** | 无自动保活;失效需浏览器过滑块重抓 |
| **Gemini**(CDP 桥) | Google 登录 cookie(profile) | Chrome profile 磁盘 | 数天~周(登录态) | **真浏览器全自动**:桥每次请求自捕获刷新会话令牌;30 分钟无活动休眠、请求自动唤醒;崩溃后恢复标签页令牌仍在;异常失效时"页面发一条消息"自愈 |
| | 会话令牌 at/SNlM6e/f.sid | `.runtime/bridge/gemini_session.json` | 会话级(随页面实例轮换) | 同上 |
| **Claude**(CDP 桥) | 登录 cookie | Chrome profile 磁盘 | 数天~周(登录态) | 全自动:模板/客户端头每次请求自刷新;**无会话令牌**;5h+7d 双限额实时监控与预警 |
| **MiniMax** | token(JWT) | `minimax_tokens.txt` | **~38 天**(实测 exp) | 无自动保活;过期重抓 localStorage._token;另有 Token Plan 配额耗尽风险 |
| **Mimo** | Cookie 串(ph/serviceToken/userId) | `mimo_tokens.txt` | **未实测**(通常数天~数周) | 无自动保活;失效重抓浏览器 Cookie |
| ~~元宝~~ | — | — | — | **已关停**(2026-08-14 风控冻结) |

## 保活机制分级

**全自动(无需人工)**:
- ChatGPT:10 分钟健康检查自动换发(session/refresh → access)
- GLM / Kimi:refresh_token 自动换发 access_token 并轮换 refresh
- Gemini / Claude(CDP 桥):请求自捕获刷新 + 唤醒/休眠闭环 + 限额监控(Claude)

**半自动(人工一次性操作后长期有效)**:
- DeepSeek / Grok / MiniMax / Mimo:token 会话级/数周级,失效后浏览器重抓一次即可
- 千问:ticket 年级,但 x5sec 20 分钟——**实际可用性受 x5sec 支配**,高频使用需频繁过滑块(定位低频备用)

**高风险(需留意)**:
- 豆包:a_bogus 分钟级,保活成本高,低频备用
- MiniMax:Token Plan 配额可能耗尽(错误码 2056),配额恢复后自动可用
- Grok:usage_limit_reached 需重抓 cookie

## 统一重抓方法

所有网页凭证的提取都走 CDP 抓包(scripts/cdp/ 下的 capture-*.mjs 或
docs/CDP_BROWSER_DEBUG.md 的通用流程),在独立 profile 的小号浏览器
(桌面快捷方式「Gemini小号浏览器」)里操作,大号浏览器永不混用。
