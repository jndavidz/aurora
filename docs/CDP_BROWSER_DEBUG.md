# CDP 浏览器自动化全景(三通道 + 资产地图 + 抓包方法论)

> 更新时间: 2026-08-31(由"Min 抓包流程"扩写为全景文档;原 Min 时代内容见附录 B)
> 用途: aurora 网页逆向的统一入口——先查这里确认"用哪个通道、脚本在哪、资产在哪",再进各站 `docs/<X>.md`。

---

## 一、三通道总览

| 通道 | 浏览器 | 端口 | 定位 | 启停 |
|---|---|---|---|---|
| **1. Chrome for Testing**(默认) | `D:\PortableApps\_net\Chrome for Testing\chrome.exe`(152.0.7977.42) | 9222 | 无强风控站点的通用抓取;独立 profile,与日常浏览器隔离 | `bash scripts/cdp/start-chrome-cdp.sh {start\|stop\|status}` |
| **2. Tabbit**(强风控/登录态) | Tabbit(用户日常在用) | 9223 | 强风控+需登录态站点(如 erji);复用用户真实登录态 | 快捷方式已永久带 `--remote-debugging-port=9223`(桌面+开始菜单+任务栏),无需启停 |
| **3. NUC 桥用 Chrome** | nuc-hifi(10.10.10.3)上的 Chrome | 9222(NUC 本地) | aurora-bridge 的常驻执行浏览器;禁 GPU 软渲染+CPU 绑核 | `chrome-cdp.service`(配置权威:`scripts/nuc/`,改前先改仓库再 scp 同步) |

通道 1/2 的抓取**统一在 Windows 侧跑**(`D:\PortableApps\_sys\node\node.exe`):WSL NAT 下 `127.0.0.1` 不可达 Windows 回环端口(Chrome 强制绑回环)。通道 3 在 NUC 上跑,由 bridge.mjs 常驻。

操作 Tabbit(通道 2)纪律:①操作用户在用的浏览器前先告知影响 ②一律优先 `--new-tab`(不顶掉用户正在看的页面) ③若 CDP 失联,重跑快捷方式 lnk 追加命令即可。

## 二、资产地图(东西都在哪)

### 脚本(按仓库分工)

| 位置 | 内容 |
|---|---|
| `scripts/cdp/`(本仓库,**权威**) | 核心桥 `bridge.mjs`+`keeper.mjs`、保活、各 AI 站 `capture-*/grab-*`、`cdp-helper.mjs`(多仓库权威源)、`cdp-drive.mjs`(原版)、`start-chrome-cdp.sh`;分类索引见 `scripts/cdp/README.md` |
| `D:\repos\open-xiaoai\scripts\cdp\` | 增强版 `cdp-drive.mjs`(--new-tab);`cdp-helper.mjs` 为同步副本 |
| `D:\repos\soft-query\` | 浏览器插件 + CLI 批量查询;`cdp-helper.mjs` 为同步副本 |
| `musicdl/.state/cdp-*.mjs` | musicdl 的一次性 CDP 实验(该仓库 `.state/` 即实验区,已 gitignore) |

**cdp-helper.mjs 防漂移规则**:只改 `scripts/cdp/cdp-helper.mjs`(权威源),再同步到 open-xiaoai 与 soft-query 两份副本(文件头均有标注)。

### 浏览器与登录态(`D:\PortableApps\_net\`)

| 路径 | 内容 |
|---|---|
| `Chrome for Testing\` | 浏览器本体(152.0.7977.42) |
| `chrome-cdp\profile\` | 通道 1 的登录态 profile(~314M;含 AI 站小号登录态,勿 commit 勿外传) |

### 代码侧对接(aurora 仓库内)

- Go Provider:`internal/provider/{claude,gemini,hunyuan}_cdp.go`(经桥转发,唤醒/熔断/限频)
- 配置:`CLAUDE_CDP_URL` / `GEMINI_CDP_URL`(默认复用同一桥服务);桥部署形态见 `docs/GEMINI.md` §八
- 凭证与保活总表:`docs/CREDENTIALS.md`;`.runtime/bridge/*.json` 为桥会话缓存(gitignore)

## 三、通用抓包方法论(通道无关)

带 `--remote-debugging-port` 启动的 Chromium 暴露 CDP HTTP + WebSocket 端点。
用零依赖客户端(`scripts/cdp/cdp-helper.mjs`)即可:读 cookie(`Network.getCookies`)/
localStorage(`Runtime.evaluate`)、抓请求(`Network.requestWillBeSent`+`Network.getResponseBody`)、
驱动页面(注入文件 `DOM.setFileInputFiles`、输入 `Input.insertText`、点击 `Runtime.evaluate`)。

抓包脚本骨架:

```js
// 列标签页 → 连 page → 监听 /api 请求(完整示例见 git 历史 2026-08-13 版)
import { pathToFileURL } from "node:url";
import http from "node:http";
const { cdp } = await import(pathToFileURL("C:/repos/aurora/scripts/cdp/cdp-helper.mjs").href);

function getJSON(path) {
  return new Promise((res, rej) => {
    http.get({ host: "127.0.0.1", port: 9222, path }, (r) => { let d=""; r.on("data",c=>d+=c); r.on("end",()=>res(JSON.parse(d))); }).on("error", rej);
  });
}
const targets = await getJSON("/json");
const page = targets.find(t => t.type === "page" && t.url.includes("目标站"));
const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Page.enable");
await c.cmd("Network.enable");
c.on((m) => {
  if (m.method === "Network.requestWillBeSent") {
    const u = m.params.request?.url || "";
    if (u.includes("/api/")) console.log(u, m.params.request?.postData?.slice(0,200));
  }
});
c.close();
```

### 常用 CDP 命令速查

| 目的 | 命令 |
|---|---|
| 导航 | `Page.navigate {url}` |
| 读 cookie(含 httpOnly) | `Network.getCookies {urls:[...]}` |
| 读 localStorage | `Runtime.evaluate {expression:"localStorage.getItem('userToken')", returnByValue:true}` |
| 抓请求头/体 | `Network.requestWillBeSent`(监听)+ `Network.getResponseBody {requestId}`(注意:SSE 流式响应体抓不到) |
| 注入文件(上传) | `DOM.getDocument` → `DOM.querySelector {selector:"input[type=file]"}` → `DOM.setFileInputFiles {files:[path]}` |
| 输入文本 | `Runtime.evaluate` 聚焦 textarea → `Input.insertText {text}` → `Input.dispatchKeyEvent`(Enter) |
| 点击 | `Runtime.evaluate {expression:"el.click()"}` |

> curl 验证端点时在 Git Bash 里加 `--noproxy '*'`(可能走代理)。

## 四、注意事项

- **关闭浏览器必须优雅**(CDP `Browser.close`、`scripts/cdp/graceful-close.mjs` 或菜单正常退出),
  **禁止 `taskkill /F` / `Stop-Process -Force` 强杀** —— 强杀会损坏 profile 登录态,下次启动
  出现"恢复异常关闭"横幅,该状态下页面自动化失效(详见 docs/GEMINI.md §Chrome 生命周期铁律)。
- 调试端口仅绑定 127.0.0.1。
- 抓到的 token 属可丢弃小号,勿入库、勿进聊天记录。
- browser-use / IAB 托管浏览器是全新 profile,看不到既有登录态 —— 需要登录态时直接接管真实浏览器(通道 2)或用通道 1 的专用 profile。

---

## 附录 A:DeepSeek 实测要点(2026-08-13,方法论样板)

- **认证**:`localStorage["userToken"]` 是 `{"value":"<token>","__version":"0"}` 包装,取 `.value` 作 `Authorization: Bearer`。
- **PoW 必选**(DeepSeekHashV1):`create_pow_challenge` → 解 `H(salt_expireAt_nonce)==challenge`(23 轮 Keccak)→ `x-ds-pow-response`。
- **请求头**:需 Chrome UA + Origin + Referer + `x-client-platform: web` + `x-client-version: 2.3.0`,否则 WAF 拦(curl 裸请求返回 "Error - Request Blocked")。
- **识图**:`upload_file` → `fetch_files` → `fork_file_task {file_id, to_model_type:"vision"}` → completion `model_type:"vision"` + fork 后 file_id。
- 完整协议细节见 `docs/DEEPSEEK.md` §1。

## 附录 B:Min 浏览器通道(历史,2026-08-13)

当时经 Min(Electron)抓 DeepSeek,现已由 Chrome for Testing 通道取代;Min 本体仍在
`D:\PortableApps\_net\Min-v1.35.4\`(profile 在 `%APPDATA%\Min`)。需要复现时:

```bash
MSYS_NO_PATHCONV=1 taskkill /F /IM Min.exe /T          # 关闭单实例
cd "/d/PortableApps/_net/Min-v1.35.4"
nohup ./Min.exe --remote-debugging-port=9222 > /tmp/min-cdp.log 2>&1 & disown
curl -s --noproxy '*' http://127.0.0.1:9222/json/version
```
