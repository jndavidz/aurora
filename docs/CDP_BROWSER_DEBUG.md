# CDP 接管浏览器抓包流程(Min/Electron/Chromium)

> 更新时间: 2026-08-13
> 场景:在已登录的浏览器(如 Min)里读 localStorage/cookie、抓 chat.deepseek.com 等网页接口的真实请求,
> 用于逆向验证(如 aurora 的 DeepSeek P0 实测)。

---

## 一、原理

Chromium/Electron 应用带 `--remote-debugging-port=9222` 启动后,暴露 CDP(Chrome DevTools Protocol)HTTP + WebSocket 端点。
用零依赖 WebSocket 客户端(`scripts/cdp/cdp-helper.mjs`)连上即可:
- 读 cookie(`Network.getCookies`)、localStorage(`Runtime.evaluate`)
- 抓网络请求(`Network.requestWillBeSent` + `Network.getResponseBody`)
- 驱动页面(`DOM.setFileInputFiles` 注入文件、`Input.insertText` 输入、`Runtime.evaluate` 点击)

browser-use 的 IAB/CDP 托管浏览器是**全新 profile**,看不到 Min 里的登录态 —— 所以直接接管 Min 本身。

## 二、启动 Min 并开 CDP

```bash
# 1. 关闭正在运行的 Min(单实例锁会顶掉新实例)
MSYS_NO_PATHCONV=1 taskkill /F /IM Min.exe /T

# 2. 带调试端口重启(登录态在磁盘 cookie 里,重启不丢)
cd "/d/PortableApps/_net/Min-v1.35.4"
nohup ./Min.exe --remote-debugging-port=9222 > /tmp/min-cdp.log 2>&1 & disown
sleep 6

# 3. 验证
curl -s --noproxy '*' http://127.0.0.1:9222/json/version
```

> Min profile:`C:\Users\david\AppData\Roaming\Min`(标准 Chromium 结构)。
> 注意 curl 在 Git Bash 里可能走代理,加 `--noproxy '*'`。

## 三、通用抓包脚本骨架

```js
// scripts/cdp/capture.mjs(示例:列标签页 → 连 page → 抓 /api 请求)
import { pathToFileURL } from "node:url";
import http from "node:http";
const { cdp } = await import(pathToFileURL("C:/.../scripts/cdp/cdp-helper.mjs").href);

function getJSON(path) {
  return new Promise((res, rej) => {
    http.get({ host: "127.0.0.1", port: 9222, path }, (r) => { let d=""; r.on("data",c=>d+=c); r.on("end",()=>res(JSON.parse(d))); }).on("error", rej);
  });
}
const targets = await getJSON("/json");
const page = targets.find(t => t.type === "page" && t.url.includes("chat.deepseek.com"));
const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Page.enable");
await c.cmd("Network.enable");
c.on((m) => {
  if (m.method === "Network.requestWillBeSent") {
    const u = m.params.request?.url || "";
    if (u.includes("/api/v0/")) console.log(u.replace("https://chat.deepseek.com",""), m.params.request?.postData?.slice(0,200));
  }
});
// ...驱动页面 / 等待 ...
c.close();
```

## 四、常用 CDP 命令速查

| 目的 | 命令 |
|---|---|
| 导航 | `Page.navigate {url}` |
| 读 cookie(含 httpOnly) | `Network.getCookies {urls:[...]}` |
| 读 localStorage | `Runtime.evaluate {expression:"localStorage.getItem('userToken')", returnByValue:true}` |
| 抓请求头/体 | `Network.requestWillBeSent`(监听)+ `Network.getResponseBody {requestId}`(注意:SSE 流式响应体抓不到) |
| 注入文件(上传) | `DOM.getDocument` → `DOM.querySelector {selector:"input[type=file]"}` → `DOM.setFileInputFiles {files:[path]}` |
| 输入文本 | `Runtime.evaluate` 聚焦 textarea → `Input.insertText {text}` → `Input.dispatchKeyEvent`(Enter) |
| 点击 | `Runtime.evaluate {expression:"el.click()"}` |

## 五、DeepSeek 实测要点(2026-08-13 结论)

- **认证**:`localStorage["userToken"]` 是 `{"value":"<token>","__version":"0"}` 包装,取 `.value` 作 `Authorization: Bearer`。
- **PoW 必选**(DeepSeekHashV1):`create_pow_challenge` → 解 `H(salt_expireAt_nonce)==challenge`(23 轮 Keccak)→ `x-ds-pow-response`。
- **请求头**:需 Chrome UA + Origin + Referer + `x-client-platform: web` + `x-client-version: 2.3.0`,否则 WAF 拦(curl 裸请求返回 "Error - Request Blocked")。
- **识图**:`upload_file` → `fetch_files` → `fork_file_task {file_id, to_model_type:"vision"}` → completion `model_type:"vision"` + fork 后 file_id。
- 完整协议细节见 `docs/DEEPSEEK.md` §1。

## 六、注意事项

- 调试端口仅绑定 127.0.0.1;用完建议正常关浏览器(下次不带参数启动即恢复)。
- 抓到的 token 属可丢弃小号,勿入库、勿进聊天记录。
- 文件上传用 `DOM.setFileInputFiles`(IAB 不支持文件选择器,但 CDP 直连支持)。
