# 07 — 文档同步与 spec 收口

**What to build:** 把验收数据与最终配置回写到事实层，关闭全部 ticket 与 spec。

- spec `Further Notes` 追加实测三段数据（L1 首轮/续轮、L2、L3）与决策门结论。
- 若部署细节与 `docs/NAS_DEPLOYMENT.md` 现状有偏差（镜像 tag、端口、探活命令），就地修正。
- 全部 ticket `Status: done`；spec `Status: closed`。
- 记录遗留项：其余 `*web` 通道 UA 对齐（Chrome/150）、豆包流式 TTS、bridge 换话题 `X-Session-Action` 未接。

**验收：** spec/tickets 状态一致；文档中无过时的「官方 API 直连」断言。

**Status:** pending

**Blocked by:** 05, 06
