// Hook 并保存 claude.ai 的全部 /api/ 请求(method/url/postData/headers + 响应状态)。
// 用途:逆向 claude.ai 网页对话协议(会话创建、消息发送端点、认证头、SSE 格式)。
// 用法: node capture-claude.mjs [监听秒数=240]
// 输出: %TEMP%/claude_capture.json(每次命中覆盖;完整列表存 %TEMP%/claude_capture_all.json)
import { pathToFileURL } from "node:url";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);

const SECONDS = parseInt(process.argv[2] || "240", 10);
const OUT = path.join(os.tmpdir(), "claude_capture.json");
const OUT_ALL = path.join(os.tmpdir(), "claude_capture_all.json");

function getJSON(p) {
  return new Promise((res, rej) => {
    http.get({ host: "127.0.0.1", port: 9222, path: p }, (r) => {
      let d = "";
      r.on("data", (c) => (d += c));
      r.on("end", () => {
        try { res(JSON.parse(d)); } catch (e) { rej(e); }
      });
    }).on("error", rej);
  });
}

// 找 claude.ai 页面;没有则打开
async function findOrOpen() {
  const targets = await getJSON("/json");
  const page = targets.find((t) => t.type === "page" && t.url.includes("claude.ai"));
  if (page) return page;
  await new Promise((res, rej) => {
    const req = http.request(
      { host: "127.0.0.1", port: 9222, path: "/json/new?" + encodeURIComponent("https://claude.ai/"), method: "PUT" },
      (r) => {
        let d = "";
        r.on("data", (c) => (d += c));
        r.on("end", () => {
          try { res(JSON.parse(d)); } catch (e) { rej(e); }
        });
      }
    );
    req.on("error", rej);
    req.end();
  });
  await new Promise((r) => setTimeout(r, 4000));
  const targets2 = await getJSON("/json");
  return targets2.find((t) => t.type === "page" && t.url.includes("claude.ai")) || null;
}

const page = await findOrOpen();
if (!page) {
  console.error("no claude.ai page target");
  process.exit(1);
}
console.log("page:", page.url.slice(0, 80));

const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Network.enable");
await c.cmd("Runtime.enable");

const all = [];
let lastApi = null;

c.on((m) => {
  if (m.method === "Network.requestWillBeSent") {
    const r = m.params.request;
    if (!r || !r.url || !r.url.includes("claude.ai/api")) return;
    const rec = {
      ts: new Date().toISOString(),
      method: r.method,
      url: r.url,
      postData: r.postData || "",
      headers: r.headers || {},
      requestId: m.params.requestId,
    };
    lastApi = rec;
    all.push(rec);
    fs.writeFileSync(OUT_ALL, JSON.stringify(all, null, 2));
    fs.writeFileSync(OUT, JSON.stringify(rec, null, 2));
    console.log("REQ", r.method, r.url.slice(0, 120), "| postData len:", (r.postData || "").length);
  }
  if (m.method === "Network.responseReceived" && lastApi && m.params.requestId === lastApi.requestId) {
    lastApi.status = m.params.response.status;
    fs.writeFileSync(OUT_ALL, JSON.stringify(all, null, 2));
    console.log("RESP status:", m.params.response.status);
    // 尝试抓响应体(流式 SSE 可能拿不到,试试)
    c.cmd("Network.getResponseBody", { requestId: m.params.requestId }).then((rb) => {
      if (rb.result && rb.result.body) {
        lastApi.body = rb.result.body.slice(0, 4000);
        fs.writeFileSync(OUT_ALL, JSON.stringify(all, null, 2));
        console.log("BODY head:", rb.result.body.slice(0, 200).replace(/\n/g, " | "));
      }
    }).catch(() => {});
  }
});

console.log("listening on claude.ai/api for", SECONDS, "seconds...");
console.log("请在浏览器里登录 claude.ai 并发一条消息(如\"你好\")");
setTimeout(() => {
  console.log("window ended, captured", all.length, "api requests ->", OUT_ALL);
  c.close();
  process.exit(0);
}, SECONDS * 1000);
