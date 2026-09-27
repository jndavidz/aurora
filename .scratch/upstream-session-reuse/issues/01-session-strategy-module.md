# 01 — Session Strategy deep module（seam 先行）

**What to build:** provider 层新增 Session Strategy deep module：对外只暴露 acquire/release 小接口，吸收池化、TTL、信令处理、失效降级全部 implementation。本 ticket 是 "make the change easy" 的 prefactor，本身无用户可见行为，仅单测可验证（常规豁免项）。

覆盖内容：

- 池化：上限 16 entries，LRU 淘汰。
- TTL = 30 分钟：惰性过期（每次命中时检查）+ 后台清理循环（参照 handler 层既有 SessionManager 模式）。
- clientKey 解析，取值优先级：`X-Session-Key` 请求头 → chat completions `user` 字段（剥除 `#new#` 前缀后）→ 无（不进池）。缓存键 = `clientKey + "|" + exposed model id`。前缀一致性校验不做（aurora 只做反代，信任客户端）。
- 显式作废信令：`X-Session-Action: new` 头；`user` 字段 `#new#` 前缀（解析在此层，接线在后续 ticket）。

**Blocked by:** None — can start immediately

**Status:** ready-for-agent

- [ ] 池化/TTL/LRU 全部穿过 acquire/release seam，用内存 fake 上游测试，无网络依赖
- [ ] clientKey 优先级解析单测：头 → user 字段剥 `#new#` → 不进池三档
- [ ] 缓存键 = clientKey + model id，同 clientKey 不同 model 不串池
- [ ] `X-Session-Action: new` 与 `#new#` 前缀解析单测
- [ ] LRU 淘汰与后台清理循环单测（超上限淘汰最旧、TTL 过期条目被清）
