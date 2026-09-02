// scripts/cdp/capture-chatgpt-conv.mjs — 在已登录 chatgpt.com 页面捕获真实对话请求模板
//
// 背景:ChatGPT 已改为浏览器会话绑定鉴权(Cloudflare + oai-did 设备指纹),服务端
//   无法复用 token。aurora 的 ChatGPT 通道改走 NUC 桥浏览器通道(bridge.mjs 的
//   chatgpt adapter),由桥在已登录页面上下文 fetch /backend-api/f/conversation。
//   本脚本捕获一次**真实有效**的请求体,供 adapter 复用(像 claude 的 template)。
//
// 新版端点实测(2026-09-02):发消息前先 POST /backend-api/f/conversation/prepare
//   拿 client_prepare_state,再 POST /backend-api/f/conversation。本脚本拦截后者。
//
// 前提:chatgpt.com 标签页必须已登录且 composer 可用。否则抓不到。
// 用法: node capture-chatgpt-conv.mjs
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { cdp } from "./cdp-helper.mjs";

const ROOT = path.resolve(import.meta.dirname, "../..");
const STATE_DIR = path.join(ROOT, ".runtime", "bridge");
const OUT = path.join(STATE_DIR, "chatgpt_conv.json");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
function get(p) { return new Promise((res, rej) => { const r = http.request({ host: "127.0.0.1", port: 9222, path: p }, (x) => { let d = ""; x.on("data", (c) => (d += c)); x.on("end", () => res(d)); }); r.on("error", rej); r.end(); }); }

const targets = JSON.parse(await get("/json"));
const t = targets.find((x) => x.type === "page" && x.url.startsWith("https://chatgpt.com"));
if (!t) { console.log("✗ 无 chatgpt.com 标签页"); process.exit(1); }
const c = await cdp(t.webSocketDebuggerUrl);
await c.cmd("Network.enable", {});

const loginState = await c.cmd("Runtime.evaluate", {
  expression: `(() => { const el = document.querySelector('textarea#prompt-textarea') || document.querySelector('[contenteditable="true"]') || document.querySelector('div[role="textbox"]'); return !!el; })()`,
  returnByValue: true,
});
if (!loginState.result?.result?.value) {
  console.log("✗ chatgpt.com 未登录或 composer 不可用(页面显示登录页)。请先登录再跑。");
  c.close(); process.exit(1);
}

let captured = null;
c.on((m) => {
  if (m.method === "Network.requestWillBeSent") {
    const u = m.params?.request?.url || "";
    // 只拦真正的 conversation POST(排除 /prepare)
    if (u.includes("/backend-api/f/conversation") && !u.includes("/prepare")) {
      captured = { url: u, headers: m.params.request.headers, postData: m.params.request.postData };
      console.log("捕获到 conversation 请求:", u.slice(0, 100));
    }
  }
});

const sent = await c.cmd("Runtime.evaluate", {
  expression: `(async () => {
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
    const el = document.querySelector('textarea#prompt-textarea') || document.querySelector('[contenteditable="true"]') || document.querySelector('div[role="textbox"]');
    if (!el) return JSON.stringify({ err: "no composer" });
    el.focus();
    const proto = el instanceof HTMLTextAreaElement ? window.HTMLTextAreaElement.prototype : window.HTMLDivElement.prototype;
    const setter = Object.getOwnPropertyDescriptor(proto, "value");
    if (setter && setter.set) { setter.set.call(el, "ping"); el.dispatchEvent(new Event("input", { bubbles: true })); }
    else if (el.isContentEditable) { el.innerText = "ping"; el.dispatchEvent(new Event("input", { bubbles: true })); }
    await sleep(400);
    el.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", code: "Enter", keyCode: 13, which: 13, bubbles: true }));
    return JSON.stringify({ ok: true });
  })()`,
  awaitPromise: true, returnByValue: true,
});
console.log("发送:", sent.result?.result?.value);

for (let i = 0; i < 30 && !captured; i++) await sleep(500);
if (!captured) { console.log("✗ 未捕获到 conversation 请求"); c.close(); process.exit(1); }

let body;
try { body = JSON.parse(captured.postData); } catch (e) { console.log("✗ postData 解析失败:", e.message); c.close(); process.exit(1); }

const tpl = { ...body };
delete tpl.conversation_id;
delete tpl.parent_message_id;
if (Array.isArray(tpl.messages)) {
  tpl.messages = [{ id: "PLACEHOLDER", role: "user", content: [{ type: "text", text: "PLACEHOLDER" }] }];
}
const keepHeaders = {};
for (const k of ["accept", "content-type", "origin", "referer", "oai-language", "openai-sentinel", "priority"]) {
  if (captured.headers[k]) keepHeaders[k] = captured.headers[k];
}
tpl.headers = keepHeaders;
tpl._capturedUrl = captured.url;

fs.mkdirSync(STATE_DIR, { recursive: true });
fs.writeFileSync(OUT, JSON.stringify(tpl, null, 2));
console.log("✓ 已写模板:", OUT);
console.log("  model:", tpl.model, "| action:", tpl.action, "| client_prepare_state:", tpl.client_prepare_state);
c.close();
process.exit(0);
