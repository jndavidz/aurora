# Aurora 项目指令

Aurora 是「网页端 → OpenAI 兼容 API」网关(Go):对外暴露 `/v1/chat/completions` 与 `/v1/responses` 两个表面。ChatGPT 网页逆向是默认兜底路径(不注册为 provider,实现在 `internal/handler/`);其余各家(DeepSeek/GLM/Kimi/Grok/Gemini/Claude/Hunyuan/MiniMax/Mimo/豆包/千问)实现 Provider 接口(`internal/provider/provider.go`),在 `internal/handler/router.go` 按 token 池非空条件注册。

## 命令

- 构建 `go build -o aurora .`;测试 `go test ./...`(文件名含 `_live` 的用例由 `DS_TEST_TOKEN` 等环境变量守卫,缺省自动跳过)
- NAS 一键部署:`bash scripts/deploy_nas.sh`

## 目录地图

| 路径 | 职责 |
|---|---|
| `internal/handler/` | 两表面入口 + provider 分发 + ChatGPT 兜底 + toolCallingRetry |
| `internal/provider/` | Provider 接口/Registry + 各家 chat/coding 变体路由 |
| `internal/<x>web/` | 各家网页逆向协议客户端(deepseekweb/glmweb/kimiweb/grokweb…) |
| `conversion/` | OpenAI ↔ 上游格式转换;工具调用 system prompt 注入 |
| `internal/toolcall/` | `<tool_call>` 文本协议流式解析(parser/recover/fence/tags) |
| `internal/accounts/` | 账号池、TLS 指纹、能力控制 |
| `typings/official/` | Responses API 请求/响应/事件类型 |

## 任务入口

- **改 handler / 加 provider / 动工具调用** — 先读 `docs/ARCHITECTURE.md`(Provider 接口、chat/coding 变体约定、接线点一览、历史教训)。
- **动某家上游协议** — 读对应 `docs/<X>.md`(DEEPSEEK/GLM/GROK/GEMINI/KIMI/DOUBAO/QIANWEN/MINIMAX/MIMO/MEDIA…;`docs/CLAUDE.md` 是 claude.ai 协议文档,非 agent 指令)。
- **逆向新站或更新失效协议** — 走下方 CDP 抓取。
- **凭证失效 / 保活 / 重抓** — `docs/CREDENTIALS.md`(各家有效期与保活分级总表)。
- **部署 / 发版** — `docs/NAS_DEPLOYMENT.md`(本地构建 local-toolfix 镜像,NAS 映射 65432→8080)。

## 凭证红线

- 真实凭证只在 `.runtime/tokens/`、`tokens/*.json`:这些值不进 git、不进聊天记录。
- 网页逆向有结构性封号风险:账号池只用可丢弃小号,主号不入池,并发控制在 ≈账号数/2。
- `.gitignore` 排除了 `docs/`、`*.json`、`*.txt`:文档与 token 改动只存本地,commit 天然不含它们。

## 浏览器抓取 / CDP(scripts/cdp/,零依赖)

- 方法论入口:`docs/CDP_BROWSER_DEBUG.md`(三通道全景+资产地图);登录态复用是过强风控站的关键,裸 curl 会被 zse-ck WAF / 登录墙 / 接口风控拦截。
- 本目录 37 个脚本的分类索引(核心桥/抓取引导/一次性实验)见 `scripts/cdp/README.md`;抓取产物一律落 `_scratch/`,勿放脚本目录。
- Chrome for Testing 启停:`bash scripts/cdp/start-chrome-cdp.sh {start|stop|status}`;通用抓取:`node scripts/cdp/cdp-drive.mjs <url> [--out f.txt]`;各站协议抓包:`node scripts/cdp/capture-<site>.mjs`(抓 /api/ 请求)。
- 抓取统一在 **Windows 侧**跑(`D:\PortableApps\_sys\node\node.exe`):WSL NAT 下 `127.0.0.1` 不可达 Windows 回环端口(Chrome 强制绑回环)。
- 关闭浏览器走优雅路径(graceful-close),强杀会损坏 profile 登录态。
