// check-doubao-login.mjs — 检查 NUC Chrome 上豆包页面的登录态(零依赖,用 cdp-helper)
// 用法: cd /opt/aurora-bridge/scripts/cdp && node check-doubao-login.mjs
import http from "node:http";
import { cdp } from "./cdp-helper.mjs";

const get = (p) => new Promise((res, rej) => {
  const r = http.request({ host: "127.0.0.1", port: 9222, path: p }, (x) => {
    let d = ""; x.on("data", c => d += c); x.on("end", () => res(d));
  });
  r.on("error", rej); r.end();
});

const targets = JSON.parse(await get("/json"));
const page = targets.find(t => t.type === "page" && /doubao\.com/.test(t.url));
if (!page) { console.log("no doubao page"); process.exit(0); }
console.log("page:", page.url.slice(0, 60));

const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Runtime.enable", {});

const expr = `(() => {
  const composer = document.querySelector('[contenteditable="true"]')
    || document.querySelector('textarea')
    || document.querySelector('[class*="chat_input"], [data-testid*="input"]');
  const loginHint = document.body.innerText.slice(0, 2000).match(/登录|login|扫码/i);
  return JSON.stringify({
    url: location.href.slice(0, 60),
    title: document.title.slice(0, 40),
    hasComposer: !!composer,
    loginHintText: loginHint ? loginHint[0] : null,
    cookieLen: document.cookie.length,
    bodySnippet: document.body.innerText.replace(/\\s+/g, " ").slice(0, 120)
  });
})()`;

const r = await c.cmd("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
console.log("登录态:", r.result?.result?.value || JSON.stringify(r.result).slice(0, 200));
c.close();
process.exit(0);
