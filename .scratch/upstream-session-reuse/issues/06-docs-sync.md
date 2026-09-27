# 06 — 文档同步

**What to build:** 会话复用行为全部落定后，同步既有文档中已不再成立的断言。

- `docs/ARCHITECTURE.md`：「多轮 = 客户端无状态」一节改写（该表述将不再成立）。
- `docs/DEEPSEEK.md`：多轮说明更新（增量续轮为主、全量拍平降级为新会话引导例外）。
- `CONTEXT.md`：「拍平」词条补注「仅新会话引导路径使用」。

**Blocked by:** 04 — chat completions 表面接入 + `#new#` 信令

**Status:** ready-for-agent

- [ ] `docs/ARCHITECTURE.md` 多轮表述改写
- [ ] `docs/DEEPSEEK.md` 多轮说明更新
- [ ] `CONTEXT.md` 拍平词条补注
- [ ] 跑 `bash /mnt/d/_work/dev/scripts/check/doc-consistency-check.wsl.sh` 排查其他文档过时断言
