# 07 — 文档同步与 spec 收口

**What to build:** 把验收数据与最终配置回写到事实层，关闭全部 ticket 与 spec。

- spec `Further Notes` 追加实测三段数据（L1 首轮/续轮、L2、L3）与决策门结论。
- 若部署细节与 `docs/NAS_DEPLOYMENT.md` 现状有偏差（镜像 tag、端口、探活命令），就地修正。
- 全部 ticket `Status: done`；spec `Status: closed`。
- 记录遗留项：其余 `*web` 通道 UA 对齐（Chrome/150）、豆包流式 TTS、bridge 换话题 `X-Session-Action` 未接。

**验收：** spec/tickets 状态一致；文档中无过时的「官方 API 直连」断言。

**Status:** done（2026-10-26 收口）：
- spec `Further Notes` 已追加实测三段数据（T05/T06 里的 L1/L2/L3 与首音拆解）与决策门结论。
- `docs/NAS_DEPLOYMENT.md` 核对：镜像 tag `aurora:local-toolfix`、端口 65432→8080、探活命令（`/ping`、`/v1/models`）与部署现状一致，无偏差、未改动。
- 全部 ticket `Status: done`；spec `Status: closed`。
- 遗留项记录：① 其余 `*web` 通道 UA 对齐（Chrome/150，另立项）；② 豆包流式 TTS（只留 bridge 既有 stream 接口形态，不实施）；③ bridge 换话题 `X-Session-Action: new` 未接；④ **重建 aurora 镜像部署 NAS + bridge `model` 切 `deepseek-no-search`**（T06 拍板的默认化动作，属远程变更待批准）。
- 本次收口随附落地：aurora `deepseek-no-search` 免搜索变体（`internal/provider/deepseek.go`/`deepseek_chat.go` + docs/DEEPSEEK.md）、bridge `llm_first_token_ms` 埋点。

**Blocked by:** 05, 06
