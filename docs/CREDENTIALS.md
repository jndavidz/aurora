# 全模型凭证有效期与保活策略总览

> 更新时间: 2026-09-05。汇总 aurora 各 provider 的密钥凭证类型、有效期与保活机制。
> 凭证文件统一在 `.runtime/tokens/`(gitignore 已排除),NAS 挂载为只读。
>
> ⚠️ **架构变更(2026-09-05,用户拍板)**:各模型登录态权威已统一在 **NUC Chrome**
> (`/opt/chrome-cdp/profile`,10.10.10.3)。凭证提取主通路 = NUC **token-harvester**
> (每日 07:00+rand30min systemd timer,CDP 读 localStorage/cookie → 推 NAS 部署区,
> 配合 E3 热加载免重启生效),见文末 §"NUC 统一凭证提取器"。
> PC 侧 `.runtime/tokens/` 降级为历史快照(keepalive 周任务保留为备份链路)。
> 注意:Drive 同步规则排除 `.runtime` 隐藏目录(`black_prefix="."`),NAS
> `/volume2/dev/apps/aurora/.runtime/tokens/` 是**死水快照**,不作任何更新源。

## 总表

| provider | 凭证 | 文件 | 有效期 | 保活/续期策略 |
|---|---|---|---|---|
| **ChatGPT** | access_token(JWT) | `access_tokens.txt` | **~90 天**(实测 2026-08-21 抓,至 2026-11-19) | 页面上下文直接抓取(`capture-chatgpt.mjs`);**session→access exchange 链路 2026-08 起失效**(bogdanfinn 换出的 token 被 ChatGPT 判 `token_expired`),90 天到期需重抓。NUC Chrome 有 chatgpt.com 登录态,重抓迁移 NUC 待办(见文末) |
| **DeepSeek** | userToken(localStorage) | `deepseek_tokens.txt` | 会话级(实测稳定) | **NUC harvester 每日提取**(2026-09-05 三次修正后上线):⚠️ deepseek 网页改版,userToken 现存 **JSON 包裹** `{"value":"<token>","__version":"1"}`,裸发整个字符串即 invalid token(前两轮"游客票"误判实为未解包);harvester 按 jsonField=value 解包兼容裸字符串,live 验证通过 |
| **GLM** | chatglm_refresh_token(JWT) | `glm_tokens.txt` | **~90 天**(实测 refresh 响应不带新 refresh_token → 静态不轮换) | **自动**:换发 access_token(JWT ~2h);`persistRefreshToken` 预防性回写(2026-08-22);**A1 修复(2026-08-31,bcb69eb)**:临期 10min 自动重换发 + 失败清票,修复"过期票 502 到重启"的短路,行为对齐网页版(90 天内无感)。**不经 harvester**(容器读 tokens-state,进程自愈) |
| **Kimi** | refresh_token(JWT) | `kimi_tokens.txt` | **~90 天**(换发轮换,已自动回写池文件;⚠️ **禁止手动调 RefreshToken API 测试**——每次调用轮换作废池内 token,2026-08-23 实踩) | **自动**:换发 access_token(~15 分钟),新 refresh_token 由 `persistRefreshToken` 原子回写(2026-08-22 修复"漂移");**A1 修复(2026-08-31,bcb69eb)**:临期 3min 自动重换发 + 失败清票;token 失效用 `grab-kimi.mjs` 从页面 localStorage 重抓。**不经 harvester**(同 GLM) |
| **Grok** | cookie 串(uid\|cookie) | `grok_cookies.txt` | 会话级;有 `usage_limit_reached` 配额 | **NUC harvester 每日提取**(2026-09-05 登录态确认入 NUC Chrome);超限重抓 = NUC Chrome 重新登录后次日自动同步 |
| **豆包** | cookie + 签名参数 | `doubao_accounts.json` | `a_bogus` **分钟级**;其余会话级 | **通道冻结(2026-09-05 拍板,勿操作)**;doubao-hook 捕获通路保留不运行;解冻 SOP 见 PROJECT_STATUS §四 |
| **千问** | tongyi_sso_ticket(cookie) | `qianwen_tokens.txt` | **~1 年** | **NUC harvester 每日提取**(2026-09-05 起含新鲜 x5sec,通道复活) |
| | x5sec 通关 cookie | 同上(同文件) | **~20 分钟** | harvester 每日提取最新值;高频使用仍受 20 分钟时效支配(低频备用定位不变) |
| **Gemini**(CDP 桥) | Google 登录 cookie(profile) | Chrome profile 磁盘 | **~399 天滚动**(实测 2026-08-14 抓,至 2027-09-18) | **真浏览器全自动**:桥每次请求自捕获刷新会话令牌;30 分钟无活动休眠、请求自动唤醒;崩溃后恢复标签页令牌仍在;异常失效时"页面发一条消息"自愈;PC 每周保活任务见文末 |
| | 会话令牌 at/SNlM6e/f.sid | `.runtime/bridge/gemini_session.json` | 会话级(随页面实例轮换) | 同上 |
| **Claude**(CDP 桥) | 登录 cookie | Chrome profile 磁盘 | **~28 天滚动**(实测 2026-08-14,至 2026-09-11) | 全自动:模板/客户端头每次请求自刷新;**无会话令牌**;5h+7d 双限额实时监控与预警;PC 每周保活任务见文末 |
| **MiniMax** | token(JWT) | `minimax_tokens.txt` | **~38 天**(实测 exp) | **NUC harvester 每日提取** + **每日签到迁 NUC**(`minimax-checkin.timer` 09:00+rand30min,harvester 后用当日新票,实测 day4 +1000 积分);JWT 38 天/登录 cookie 最长 60 天,到期前预警,需人工重新登录 |
| **Mimo** | Cookie 串(ph/serviceToken/userId) | `mimo_tokens.txt` | **~30 天固定,不滚动**(实测 2026-08-14 抓,至 2026-09-13) | **PC 周保活代取**:cookie 固定 30 天,代取只省手动抓取;到期前预警,仍需每月人工登录一次 |
| **豆包** | cookie + URL 参数 + body 会话模板 | `doubao_accounts.json` | **2026-08-23 改版:completion 已无 `a_bogus` 签名**(URL/headers/cookie 均无,页面请求无签名 200) | **自动捕获(2026-08-23)**:`doubao-hook.mjs`(keeper 集成)在页面注入 fetch hook,捕获每次真实 completion 请求(query+body)自动更新模板;aurora 每次请求重读、只替换 prompt —— 无签名、无时效问题,模板永远当前版本。逆向记录:`bdms.frontierSign` 只出 X-Bogus(16)不足以过业务层;抖音开源算法 makeABogus(180)不稳定;页面已取消签名 |
| ~~元宝~~ | — | — | — | **已关停**(2026-08-14 风控冻结) |

## 保活机制分级

**全自动(无需人工)**:
- ChatGPT 池内:10 分钟健康检查自动换发(session/refresh → access)
- GLM / Kimi:refresh_token 自动换发 access_token 并轮换 refresh
- Gemini / Claude(CDP 桥):请求自捕获刷新 + 唤醒/休眠闭环 + 限额监控(Claude)
- MiniMax / Mimo / Grok / 千问 / DeepSeek:**NUC token-harvester 每日提取**(2026-09-05 起)

**半自动(人工一次性操作后长期有效)**:
- MiniMax:登录态死亡(≥60 天)需在 NUC Chrome 重新登录,之后 harvester 自动接管
- Mimo:cookie 固定 30 天不滚动,到期前预警,每月在 NUC Chrome 人工登录一次

**高风险(需留意)**:
- 豆包:通道冻结(2026-09-05);解冻 SOP 见 PROJECT_STATUS §四
- MiniMax:Token Plan 配额可能耗尽(错误码 2056),配额恢复后自动可用
- Grok:usage_limit_reached 需重抓(NUC Chrome 重新登录即可,次日自动同步)

## NUC 统一凭证提取器(2026-09-05 上线)

- **位置**:NUC(10.10.10.3)`/opt/credential-keeper/token-harvester.mjs`;权威副本
  `scripts/nuc/token-harvester.mjs` + systemd service/timer(改配置先改仓库再同步)。
- **调度**:每日 **08:15** + rand 30min(`token-harvester.timer`,对齐 NUC 开机窗口
  08:00–00:30;Persistent=true 错点开机补跑)。
- **站点(5)**:deepseek(localStorage userToken,**JSON 解包**兼容改版)/
  minimax(localStorage _token+JWT exp 校验)/ mimo(cookie 串)/
  qianwen(tongyi_sso_ticket+x5sec)/ grok(uid|cookie 串)。
  排除:豆包(冻结)/ GLM·Kimi(tokens-state 自愈)/ Gemini·Claude·ChatGPT(桥/池体系)。
- **幂等**:提取值与 NAS 部署区 md5 对比,变化才推;`cdp.cmd` 20s 超时防挂起。
- **凭证红线**:日志只记 OK/FAIL + 长度,绝不打印凭证内容。
- **手动触发**:`ssh root@10.10.10.3 'node /opt/credential-keeper/token-harvester.mjs'`

## PC 桥保活任务(备份链路,状态 2026-09-05)

- Windows 任务计划 `aurora-cdp-keepalive`:每天 08:30 触发,先读
  `.runtime/keepalive-state.txt`(上次成功时间),距上次成功 **< 7 天则秒退**
  (平时零开销);**≥ 7 天**才随机延迟 0~15.5h 后执行 —— 实际时刻每天不同,
  落在 08:30~24:00 之间,不固定;成功才回写状态,失败或当天 PC 关机,
  次日自动补跑。
- **2026-09-05 状态**:任务计划已由用户删除。MiniMax/Mimo 代取与 scp 推送由
  NUC harvester 接管;MiniMax 签到也已迁 NUC(minimax-checkin.timer 09:00)。
  本节降级为历史记录(gemini/claude PC 备份桥会话保留于 `.runtime/bridge/`)。
- 执行动作(三步,全部成功才回写状态,失败次日重试):
  1. `scripts/cdp/refresh-tokens.mjs`:唤醒 Chrome,从页面代取 **MiniMax**(
     `localStorage._token`)/ **Mimo**(登录 cookie)凭证,回写本地 token 文件
     (剩余 ≤14 天打 WARN);—— 已被 NUC harvester 接管,保留为备份
  2. **scp 直推 NAS** `/volume2/docker/aurora/tokens/`(容器挂载目录,即读即生效;
     不走 Drive —— Drive 同步规则 `black_prefix = "."` 排除 `.runtime` 隐藏目录);
  3. `scripts/cdp/keepalive-node.mjs`:gemini / claude 各发一条**随机问候**
     (12 条池,要求短回复)→ CDP `Browser.close` 优雅关闭 Chrome。
  全程约 1-2 分钟,Chrome 非常驻。
- 日志:`.runtime/keepalive.log`。
- 覆盖范围:Gemini / Claude 桥通道(登录 cookie 滚动续期)+ MiniMax / Mimo
  凭证代取与同步(备份)。**登录态死亡时(WARN)仍需人工重新登录一次**,
  之后脚本自动接管抓取。

## 统一重抓方法

所有网页凭证的提取都走 CDP 抓包(scripts/cdp/ 下的 capture-*.mjs 或
docs/CDP_BROWSER_DEBUG.md 的通用流程)。2026-09-05 起登录态权威在 NUC Chrome
(`/opt/chrome-cdp/profile`,VNC 或 NUC 本机操作);PC 小号浏览器为备份链路。
操作原则:小号专用 profile,大号浏览器永不混用。
