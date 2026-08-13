// 提取腾讯元宝网页凭据(X-Uskey + cookie)写入 tokens/yuanbao_tokens.txt
// 前置:Min 浏览器带 --remote-debugging-port=9222 且 yuanbao.tencent.com 已登录。
// 原理:驱动页面发一条消息,从实时 /api/chat/ 请求头抓 X-Uskey 与 Cookie(两条凭据缺一不可,
//       且仅存在于请求头,localStorage 无存储)。登录态过期时(页面"未登录")会明确报错。
// 用法: node scripts/capture-yuanbao-token.mjs ["测试消息"]
import { pathToFileURL } from "node:url";
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
const { cdp } = await import(pathToFileURL("C:/Users/david/.agents/skills/browser-cdp/scripts/cdp-helper.mjs").href);

const MSG = process.argv[2] || "你好，用一句话自我介绍";
const OUT = "tokens/yuanbao_tokens.txt";

function getJSON(p) {
  return new Promise((res, rej) => {
    http.get({ host: "127.0.0.1", port: 9222, path: p }, (r) => { let d = ""; r.on("data", c => d += c); r.on("end", () => res(JSON.parse(d))); }).on("error", rej);
  });
}

const targets = await getJSON("/json");
const page = targets.find(t => t.type === "page" && t.url.includes("yuanbao.tencent.com"));
if (!page) { console.error("未找到 yuanbao.tencent.com 页面 target"); process.exit(1); }
const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Network.enable");
await c.cmd("Runtime.enable");

// 登录态检查
const st = await c.cmd("Runtime.evaluate", { expression: "document.body ? document.body.innerText.slice(0, 200) : ''", returnByValue: true });
const bodyText = st.result.result.value || "";
if (bodyText.includes("未登录")) {
  console.error("页面处于未登录状态:请在 Min 浏览器里重新登录 yuanbao.tencent.com 后再运行本脚本");
  process.exit(1);
}

const reqs = [];
c.on((m) => {
  if (m.method === "Network.requestWillBeSent" && m.params.request.url.includes("/api/chat/")) {
    reqs.push(m.params.request.headers);
  }
});

// 注入文本(innerHTML + input 事件,Quill 认)并点发送按钮
await c.cmd("Runtime.evaluate", { expression: `(() => { const ed = document.querySelector('.ql-editor'); if (!ed) return 'no-editor'; ed.focus(); ed.innerHTML = '<p>' + ${JSON.stringify(MSG)} + '</p>'; ed.dispatchEvent(new InputEvent('input', { bubbles: true, inputType: 'insertText', data: ${JSON.stringify(MSG)} })); return ed.textContent; })()`, returnByValue: true });
await sleep(900);
await c.cmd("Runtime.evaluate", { expression: `(() => { const el = document.querySelector('a[aria-label="发送"], .style__send-btn___RwTm5'); if (el) el.click(); return !!el; })()`, returnByValue: true });

let waited = 0;
while (reqs.length === 0 && waited < 15000) { await sleep(500); waited += 500; }
if (reqs.length === 0) { console.error("未捕获到 /api/chat/ 请求(页面可能异常,请手动发一条消息重试)"); process.exit(1); }

const h = reqs[reqs.length - 1];
const uskey = h["X-Uskey"] || "";
let cookie = h["Cookie"] || "";
// 浏览器请求头通常不带 Cookie(认证走 X-Uskey + X-ID 等头);cookie 用于派生
// X-ID/T-UserID/X-device-id/X-HY93,用 Network.getCookies 兜底构建。
if (!cookie) {
  const { result } = await c.cmd("Network.getCookies", { urls: ["https://yuanbao.tencent.com"] });
  cookie = result.cookies.map(ck => ck.name + "=" + ck.value).join("; ");
}
if (!uskey || !cookie) { console.error("请求头缺 X-Uskey,且无法从浏览器获取 cookie"); process.exit(1); }
fs.mkdirSync(path.dirname(OUT), { recursive: true });
fs.writeFileSync(OUT, uskey + "\t" + cookie + "\n");
console.log(`已写入 ${OUT}:X-Uskey ${uskey.length} 字符 + Cookie ${cookie.length} 字符`);
console.log("(X-Uskey 前缀:", uskey.slice(0, 40), "...)");
c.close();
