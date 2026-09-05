# 全模型凭证有效期与保活策略总览

> 更新时间: 2026-09-05。汇总 aurora 各 provider 的密钥凭证类型、有效期与保活机制。
>
> **架构(2026-09-05 起)**:各模型登录态权威统一在 **NUC Chrome**(`/opt/chrome-cdp/profile`)。
> 凭证提取主通路 = NUC **token-harvester**(每日 08:15+rand30min,CDP 读 localStorage/cookie
> → 推 NAS 部署区 → 容器内 E3 热加载免重启生效),详见文末。
> ⚠️ Drive 同步规则排除 `.runtime` 隐藏目录(`black_prefix="."`),NAS
> `/volume2/dev/apps/aurora/.runtime/tokens/` 是**死水快照**,不作任何更新源。
> PC 侧 `.runtime/tokens/` 已清空;Windows keepalive 任务计划已删除(备份桥会话保留于
> PC `.runtime/bridge/`)。

## 总表

| provider | 凭证 | 文件 | 有效期 | 当前保活方式 |
|---|---|---|---|---|
| **ChatGPT** | access_token(JWT) | `access_tokens.txt` | **~90 天** | 池内 10 分钟健康检查自动换发;到期重抓走 `capture-chatgpt.mjs`(**session→access exchange 已失效**,须页面上下文直接抓;迁移 NUC 待办) |
| **DeepSeek** | userToken(localStorage) | `deepseek_tokens.txt` | 会话级(实测稳定) | **NUC harvester 每日提取**。⚠️ 网页已改版:userToken 存 **JSON 包裹** `{"value":"<token>","__version":"1"}`,裸发整串即 invalid;harvester 按 `.value` 解包(手抓脚本同样注意) |
| **GLM** | chatglm_refresh_token(JWT) | tokens-state 卷 | **~90 天** | 进程自愈:refresh 换发 access(~2h)+ 预防性回写 + 临期 10min 重换发。**不经 harvester** |
| **Kimi** | refresh_token(JWT) | tokens-state 卷 | **~90 天** | 进程自愈:refresh 换发 access(~15min)+ 原子回写 + 临期 3min 重换发。⚠️ **禁止手动调 RefreshToken API 测试**(每次调用轮换作废池内 token)。**不经 harvester** |
| **Grok** | cookie 串(uid\|cookie) | `grok_cookies.txt` | 会话级;有配额限制 | **NUC harvester 每日提取**;超限 = NUC Chrome 重新登录,次日自动同步 |
| **豆包** | cookie + 会话模板 | `doubao_accounts.json` | 模板无时效(2026-08-23 起 completion 免签名) | **通道冻结(2026-09-05,勿操作)**;doubao-hook 捕获通路保留不运行;解冻 SOP 见 PROJECT_STATUS §四 |
| **千问** | tongyi_sso_ticket + x5sec(cookie) | `qianwen_tokens.txt` | ticket **~1 年**;x5sec **~20 分钟** | **NUC harvester 每日提取**(新鲜 x5sec 使通道可用);高频使用仍受 x5sec 时效支配(低频备用) |
| **Gemini**(CDP 桥) | Google 登录 cookie | NUC Chrome profile | **~399 天滚动** | 桥全自动:每请求自捕获刷新 + 空闲休眠/请求唤醒 + 崩溃自愈 |
| **Claude**(CDP 桥) | 登录 cookie | NUC Chrome profile | **~28 天滚动** | 桥全自动:模板/客户端头每请求自刷新;5h+7d 双限额实时监控 |
| **MiniMax** | token(JWT) | `minimax_tokens.txt` | JWT **~38 天**;登录态 ≤60 天 | **NUC harvester 每日提取** + **每日签到**(`minimax-checkin.timer` 09:00,实测 day4 +1000 积分);登录态死亡需 NUC Chrome 人工重登一次 |
| **Mimo** | Cookie 串 | `mimo_tokens.txt` | **~30 天固定,不滚动** | **NUC harvester 每日提取**;每月在 NUC Chrome 人工登录一次 |
| ~~元宝~~ | — | — | — | 已关停(2026-08-14 风控冻结),勿启用 |

## 保活机制分级

**全自动**:GLM/Kimi(进程换发)、Gemini/Claude(桥自刷新)、ChatGPT 池内(健康检查换发)、
MiniMax/Mimo/Grok/千问/DeepSeek(harvester 每日提取)。

**半自动**(人工一次性操作后长期有效):MiniMax(≥60 天重登)、Mimo(每月登录)。

**高风险**:豆包(冻结)、MiniMax 积分配额(2056 耗尽,恢复后自动可用)、
Grok(usage_limit_reached 需重登)、千问高频(受 x5sec 20 分钟时效支配)。

## NUC token-harvester

- **位置**:NUC `/opt/credential-keeper/token-harvester.mjs`;权威副本 `scripts/nuc/`
  (改配置先改仓库再同步,含 service/timer)。
- **调度**:每日 **08:15+rand30min**(对齐 NUC 开机窗口 08:00–00:30;Persistent=true
  错点开机补跑);After=chrome-cdp.service。
- **站点(5)**:deepseek(localStorage,JSON 解包)/ minimax(_token+JWT exp 校验)/
  mimo(cookie 串)/ qianwen(ticket+x5sec)/ grok(uid|cookie 串)。
  排除:豆包(冻结)/ GLM·Kimi(tokens-state 自愈)/ Gemini·Claude·ChatGPT(桥/池体系)。
- **机制**:md5 幂等(变化才推)→ NAS 部署区 → E3 热加载;state 落盘
  `/opt/credential-keeper/state/`(600)供签到等后续任务使用;`cdp.cmd` 20s 超时防挂起。
- **凭证红线**:日志只记 OK/FAIL + 长度,绝不打印凭证内容。
- **手动触发**:`ssh root@10.10.10.3 'node /opt/credential-keeper/token-harvester.mjs'`

## NUC MiniMax 签到

- `minimax-checkin.timer` 每日 **09:00+rand30min**(harvester 之后,用当日新票);
  纯 Node API(签名体系同对话),无浏览器依赖;claim 幂等(已签不重复发放)。
- 实测:day4 +1000 积分,30 天有效;Token Plan 配额靠签到持续补充。

## 已退役通路(防误启用)

- **Windows keepalive 任务**(`aurora-cdp-keepalive`,2026-09-05 删除):
  MiniMax/Mimo 代取被 harvester 接管,签到迁 NUC;PC 备份桥会话保留于
  PC `.runtime/bridge/`(gemini/claude),NUC 故障切换时需重新登录刷新。
- **NAS 同步区**:`/volume2/dev/apps/aurora/.runtime/tokens/` 为死水快照,
  首次部署拷入部署区后不再有任何角色。
- 相关脚本(`refresh-tokens.mjs`/`keepalive-*.ps1`/`keepalive-node.mjs`)留于
  `scripts/cdp/` 备查,Windows 任务计划已删除。

## 统一重抓方法

所有网页凭证提取走 CDP(scripts/cdp/ 的 capture-*.mjs 或 docs/CDP_BROWSER_DEBUG.md
通用流程)。登录态权威在 NUC Chrome(VNC 或 NUC 本机操作);PC 小号浏览器为备份链路。
操作原则:小号专用 profile,大号浏览器永不混用。
