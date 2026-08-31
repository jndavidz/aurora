# scripts/cdp/ — CDP 浏览器自动化脚本目录

> 本目录是 aurora CDP 桥与各 AI 站抓取脚本的**唯一权威位置**。
> 全景文档（三通道 / 资产地图）见 `docs/CDP_BROWSER_DEBUG.md`。
> 路径稳定性约定：`bridge.mjs`、`keeper.mjs`、`cdp-helper.mjs`、`cdp-drive.mjs`、
> `start-chrome-cdp.sh` 被 Go 源码注释、docker-compose、权威文档与 dsh AGENTS.md 引用，
> **勿随意改名或移入子目录**；新增脚本请按下表归类。

## 核心（桥与运行时）

| 文件 | 说明 |
|---|---|
| `bridge.mjs` | CDP 桥主程序（各 AI 站适配器，页内 fetch；被 `internal/provider/*_cdp.go` 调用；NUC 部署目标 `/opt/aurora-bridge/scripts/cdp/bridge.mjs`） |
| `keeper.mjs` | 桥唤醒守护（Go 侧 GeminiCDPWakePort 对接） |
| `keepalive-node.mjs` / `keepalive.ps1` / `keepalive-daily.ps1` | 保活：定时向 gemini/claude 发问候、Windows 计划任务 |
| `graceful-close.mjs` | 页面优雅关闭工具 |
| `cdp-helper.mjs` | 零依赖 CDP over WebSocket 客户端（**多仓库权威源**，见下） |
| `cdp-drive.mjs` | 通用抓取驱动（aurora 原版；增强版 --new-tab 在 open-xiaoai 仓库） |
| `start-chrome-cdp.sh` | Chrome for Testing 启停（端口 9222，登录态在 `D:\PortableApps\_net\chrome-cdp\profile`） |

## 抓取引导（按站点）

| 文件 | 说明 |
|---|---|
| `capture-{chatgpt,claude,doubao,mimo,minimax,streamgenerate,yuanbao}.mjs` | 各站 /api/ 请求抓包，提取模板与客户端头 |
| `grab-{doubao-js,kimi,mimo}.mjs` | 抓取各站前端 JS / 页面资源 |
| `doubao-hook.mjs` / `test-…` 之外的豆包辅助 | 豆包站点辅助 |
| `refresh-tokens.mjs` | 唤醒 Chrome 从页面代取凭证（见 `docs/CREDENTIALS.md`） |
| `minimax-checkin.mjs` | MiniMax 签到 |
| `gemini-replay.mjs` / `start-gemini.ps1` / `show-gemini.ps1` | Gemini 请求回放 / 前台启动 / 唤出窗口 |

## 一次性实验（archive/，勿引用）

`test-*.mjs`（bdms-sign、doubao-full、douyin-*、frontier-sign）、`find-*.mjs`（abogus、leak*）、
`analyze-mmr.mjs` — 各站逆向排查时的一次性脚本，仅存档；新实验也放这里。

## 产物规则

抓取输出（`*.txt`、截图等）**一律落 `aurora/_scratch/`**，不进本目录。

## 多仓库分工（防漂移）

- **aurora `scripts/cdp/`**：核心桥 + AI 站抓取（本目录，权威）。
- **open-xiaoai `scripts/cdp/`**：增强版 `cdp-drive.mjs`（--new-tab）；`cdp-helper.mjs` 为同步副本。
- **repos/soft-query**：`cdp-helper.mjs` 为同步副本（CLI 版依赖）。
- 改 `cdp-helper.mjs` 只改本目录，再同步到上述两处副本。
