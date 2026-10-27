# 06 — 文档与回退说明

**Type:** task

**What to build:** 让运维与后续会话能从文档里还原「这个功能怎么开、怎么关、边界在哪」。

### 6.1 `docs/ARCHITECTURE.md`

- 在 provider 相关小节（§5 附近，按现场实际编号）新增「provider 工具模拟」小节：
  - 数据流：`client(tools) → handler 分派 → TextCollector 全文 → toolcall 解析 → tool_calls / 降级文本`；
  - B > A 的优先级与 `Registry.NativeTools` 槽位；
  - 边界：A 仅 `/v1/chat/completions`；`/v1/responses` 带 tools 时维持剥离；
  - 历史翻译：`function_call` → 「调用 name(args)」、`function_call_output` → 「工具 name 的执行结果：…（4KB 截断）」；
  - 一句「为什么不改 `internal/toolcall`」（已被 ChatGPT 通道验证）。
- 遵循该文件既有排版（中文、表格/编号列表），别新造一个顶层章节。

### 6.2 `D:\_work\dev\apps\aurora\部署说明.md`（仓库外，直接改，不入 git）

新增「工具调用（function calling）」小节：
- 环境变量表：`PROVIDER_TOOL_EMULATION` / `PROVIDER_TOOL_MODELS` / `PROVIDER_TOOL_RETRIES` / `NATIVE_TOOLS_BASE` / `NATIVE_TOOLS_KEY` / `NATIVE_TOOLS_MODEL_MAP`，逐个写默认值与语义；
- **灰度步骤**：先 `PROVIDER_TOOL_MODELS=deepseek-no-search` 单模型开，验证 bridge 点歌与 HA；
- **回退步骤**：`PROVIDER_TOOL_EMULATION=0`（或清空白名单）→ 重启 aurora；无需回滚镜像，bridge 无改动；
- 一句 B 的前置条件（`BASE`+`KEY`+非空映射，映射非法则不注册）。

### 6.3 校验

- `docs/ARCHITECTURE.md` 里的文件路径/符号必须与最终代码一致（逐个 grep 确认，别凭记忆写）。
- 部署说明里的变量名逐个对照 `internal/config/config.go`。

**文件归属：** `docs/ARCHITECTURE.md`、`D:\_work\dev\apps\aurora\部署说明.md`（后者只落盘、不暂存）。

**验收：** 两处文档改完，`git status` 显示只有 `docs/ARCHITECTURE.md` 属于本仓改动。

**Status:** done（`83a0737` → 并入 `a8b8b7c`）

**Blocked by:** 05

## Comments
- 2026-10-27 实现：`docs/ARCHITECTURE.md` 新增 §5.5 与 §八 两行接线点；`env.template` 补 6 个变量；`D:\_work\dev\apps\aurora\部署说明.md` 新增「八、工具调用」（仓库外，不入 git）。
- 发现既有不一致（未改）：`env.template` DeepSeek 段仍写默认只有 `deepseek`，而 `deepseek.go` 已含 `deepseek-no-search`。
- review 后同步：`39b4c38` 修订 400 边界措辞。
