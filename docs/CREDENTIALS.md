# 全模型凭证有效期与保活策略总览

> 更新时间: 2026-08-14。汇总 aurora 各 provider 的密钥凭证类型、有效期与保活机制。
> 凭证文件统一在 `.runtime/tokens/`(gitignore 已排除),NAS 挂载为只读。

## 总表

| provider | 凭证 | 文件 | 有效期 | 保活/续期策略 |
|---|---|---|---|---|
| **ChatGPT** | access_token(JWT) | `access_tokens.txt` | **~90 天**(实测 2026-08-21 抓,至 2026-11-19) | 页面上下文直接抓取(`capture-chatgpt.mjs`);**session→access exchange 链路 2026-08 起失效**(bogdanfinn 换出的 token 被 ChatGPT 判 `token_expired`),90 天到期需重抓 |
| **DeepSeek** | userToken(localStorage) | `deepseek_tokens.txt` | 会话级(实测稳定) | 无自动保活;失效手动重抓浏览器 localStorage |
| **GLM** | chatglm_refresh_token(JWT) | `glm_tokens.txt` | **~90 天**(实测 refresh 响应不带新 refresh_token → 静态不轮换) | **自动**:换发 access_token(JWT ~2h);`persistRefreshToken` 预防性回写(2026-08-22) |
| **Kimi** | refresh_token(JWT) | `kimi_tokens.txt` | **~90 天**(换发轮换,已自动回写池文件) | **自动**:换发 access_token(~15 分钟),新 refresh_token 由 `persistRefreshToken` 原子回写(2026-08-22 修复"漂移":此前文件里永远是最旧的已作废 token → 重启 401) |
| **Grok** | cookie 串(uid\|cookie) | `grok_cookies.txt` | 会话级;有 `usage_limit_reached` 配额 | 无自动保活;超限/失效手动重抓 cookie |
| **豆包** | cookie + 签名参数 | `doubao_accounts.json` | `a_bogus` **分钟级**;其余会话级 | **保活被否决**(成本高);低频备用,失效重抓 |
| **千问** | tongyi_sso_ticket(cookie) | `qianwen_tokens.txt` | **~1 年** | 无自动保活 |
| | x5sec 通关 cookie | 同上(同文件) | **~20 分钟** | 无自动保活;失效需浏览器过滑块重抓 |
| **Gemini**(CDP 桥) | Google 登录 cookie(profile) | Chrome profile 磁盘 | **~399 天滚动**(实测 2026-08-14 抓,至 2027-09-18) | **真浏览器全自动**:桥每次请求自捕获刷新会话令牌;30 分钟无活动休眠、请求自动唤醒;崩溃后恢复标签页令牌仍在;异常失效时"页面发一条消息"自愈;PC 每周保活任务见文末 |
| | 会话令牌 at/SNlM6e/f.sid | `.runtime/bridge/gemini_session.json` | 会话级(随页面实例轮换) | 同上 |
| **Claude**(CDP 桥) | 登录 cookie | Chrome profile 磁盘 | **~28 天滚动**(实测 2026-08-14,至 2026-09-11) | 全自动:模板/客户端头每次请求自刷新;**无会话令牌**;5h+7d 双限额实时监控与预警;PC 每周保活任务见文末 |
| **MiniMax** | token(JWT) | `minimax_tokens.txt` | **~38 天**(实测 exp) | **PC 周保活代取** + **每日自动签到**(`minimax-checkin.mjs` 并入每日任务,400~1000 积分/天,30 天有效,持续补充 Token Plan 配额);JWT 38 天/登录 cookie 最长 60 天,到期前预警,需人工重新登录 |
| **Mimo** | Cookie 串(ph/serviceToken/userId) | `mimo_tokens.txt` | **~30 天固定,不滚动**(实测 2026-08-14 抓,至 2026-09-13) | **PC 周保活代取**:cookie 固定 30 天,代取只省手动抓取;到期前预警,仍需每月人工登录一次 |
| **豆包** | cookie + URL 签名参数 + body 会话模板 | `doubao_accounts.json` | `a_bogus` 绑定参数集+会话字段(改 prompt 无碍;实测 30 分钟+ 仍有效) | **模板重放**(2026-08-22 修复):`capture-doubao.mjs` 整段捕获 completion 请求的 query+postData 模板,aurora 每次请求重读文件、只替换 prompt 文本 —— **无需桥化**;签名失效时在页面发一条消息刷新模板 |
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

## PC 桥保活任务(每周自动)

- Windows 任务计划 `aurora-cdp-keepalive`:每天 08:30 触发,先读
  `.runtime/keepalive-state.txt`(上次成功时间),距上次成功 **< 7 天则秒退**
  (平时零开销);**≥ 7 天**才随机延迟 0~15.5h 后执行 —— 实际时刻每天不同,
  落在 08:30~24:00 之间,不固定;成功才回写状态,失败或当天 PC 关机,
  次日自动补跑。
- 执行动作(三步,全部成功才回写状态,失败次日重试):
  1. `scripts/cdp/refresh-tokens.mjs`:唤醒 Chrome,从页面代取 **MiniMax**(
     `localStorage._token`)/ **Mimo**(登录 cookie)凭证,回写本地 token 文件
     (剩余 ≤14 天打 WARN);
  2. **scp 直推 NAS** `/volume2/docker/aurora/tokens/`(容器挂载目录,即读即生效;
     不走 Drive —— Drive 同步规则 `black_prefix = "."` 排除 `.runtime` 隐藏目录);
  3. `scripts/cdp/keepalive-node.mjs`:gemini / claude 各发一条**随机问候**
     (12 条池,要求短回复)→ CDP `Browser.close` 优雅关闭 Chrome。
  全程约 1-2 分钟,Chrome 非常驻。
- 日志:`.runtime/keepalive.log`。
- 覆盖范围:Gemini / Claude 桥通道(登录 cookie 滚动续期)+ MiniMax / Mimo
  凭证代取与同步。**登录态死亡时(WARN)仍需人工在小号浏览器重新登录一次**,
  之后脚本自动接管抓取。

## 统一重抓方法

所有网页凭证的提取都走 CDP 抓包(scripts/cdp/ 下的 capture-*.mjs 或
docs/CDP_BROWSER_DEBUG.md 的通用流程),在独立 profile 的小号浏览器
(桌面快捷方式「Gemini小号浏览器」)里操作,大号浏览器永不混用。
