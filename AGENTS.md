# Aurora 项目指令

## 浏览器抓取 / CDP 资产(2026-08-23 沉淀)

- **抓取强风控站(知乎等)与逆向网页协议**: `scripts/cdp/`(零依赖 CDP 客户端 + 抓取脚本)
- **入口文档**: `docs/CDP_BROWSER_DEBUG.md`(含 WSL2 网络解法、Playwright 高阶用法)
- **快捷工具**:
  - `bash scripts/cdp/start-chrome-cdp.sh {start|stop|status}` — 启停 Chrome for Testing(带登录态 profile)
  - `node scripts/cdp/cdp-drive.mjs <url> [--selector .x] [--out f.txt] [--shot f.png]` — 通用抓取 CLI
  - `node scripts/cdp/capture-*.mjs` — 各站协议逆向(抓 /api/ 请求)
- **环境要点**:
  - 浏览器: Chrome for Testing(`/mnt/d/PortableApps/_net/Chrome for Testing`), profile `chrome-cdp`(带知乎等登录态)
  - 抓取统一在 **Windows 侧**跑: `D:\PortableApps\_sys\node\node.exe D:\repos\aurora\scripts\cdp\cdp-drive.mjs <url> --out out.txt`
  - WSL NAT 下 `127.0.0.1` 不可达 Windows 回环端口(Chrome 152 强制绑回环, `--remote-debugging-address` 无效)
  - 关闭浏览器必须优雅(禁 `/F` 强杀)
- **登录态复用是抓强风控站的关键**(裸 curl 会被 zse-ck WAF / 登录墙 / 接口风控拦截)
