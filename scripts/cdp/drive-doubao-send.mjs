// drive-doubao-send.mjs — 在豆包页面 UI 里发一条消息(触发真实 completion 请求供捕获)
// 用法: cd /opt/aurora-bridge/scripts/cdp && node drive-doubao-send.mjs
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
if (!page) { console.log("no doubao page"); process.exit(1); }

const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Runtime.enable", {});

const expr = `(async () => {
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  // 豆包 composer 是 contenteditable div
  const el = document.querySelector('[contenteditable="true"]')
    || document.querySelector('textarea')
    || document.querySelector('[data-testid="chat_input_input"]');
  if (!el) return JSON.stringify({ err: "no composer" });
  el.focus();
  // contenteditable 用 input 事件注入文本
  if (el.isContentEditable) {
    el.innerText = "你好";
    el.dispatchEvent(new Event("input", { bubbles: true }));
  } else {
    const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    const setter = Object.getOwnPropertyDescriptor(proto, "value");
    setter.set.call(el, "你好");
    el.dispatchEvent(new Event("input", { bubbles: true }));
  }
  await sleep(600);
  // 回车发送
  el.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", code: "Enter", keyCode: 13, which: 13, bubbles: true }));
  return JSON.stringify({ sent: true });
})()`;

const r = await c.cmd("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
console.log("发送结果:", r.result?.result?.value || "?");
c.close();
process.exit(0);
