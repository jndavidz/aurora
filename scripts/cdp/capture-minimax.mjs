// Hook 并保存 agent.minimaxi.com 的全部 API 请求(method/url/postData/headers + 响应状态)。
// 用途:逆向 MiniMax Agent 网页对话协议(登录态、发消息端点、认证、SSE 格式)。
// 用法: node capture-minimax.mjs [监听秒数=300]
// 输出: %TEMP%/minimax_capture_all.json(完整列表)+ %TEMP%/minimax_capture.json(最后一条)
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);

const SECONDS = parseInt(process.argv[2] || "300", 10);
const OUT = path.join(os.tmpdir(), "minimax_capture.json");
const OUT_ALL = path.join(os.tmpdir(), "minimax_capture_all.json");

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

async function findOrOpen() {
  const targets = await getJSON("/json");
  const page = targets.find((t) => t.type === "page" && t.url.includes("minimaxi.com"));
  if (page) return page;
  await new Promise((res, rej) => {
    const req = http.request(
      { host: "127.0.0.1", port: 9222, path: "/json/new?" + encodeURIComponent("https://agent.minimaxi.com/"), method: "PUT" },
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
  return targets2.find((t) => t.type === "page" && t.url.includes("minimaxi.com")) || null;
}

const page = await findOrOpen();
if (!page) {
  console.error("no agent.minimaxi.com page target");
  process.exit(1);
}
console.log("page:", page.url.slice(0, 80));

const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Network.enable");
await c.cmd("Runtime.enable");

const all = [];
let lastApi = null;
let apiCount = 0;

c.on((m) => {
  if (m.method === "Network.requestWillBeSent") {
    const r = m.params.request;
    if (!r || !r.url) return;
    const isApi = r.url.includes("/api/") || r.url.includes("minimax") || r.url.includes("minimaxi");
    // 只记非静态资源请求(过滤 js/css/png 等)
    const staticLike = /\.(js|css|png|jpg|jpeg|svg|woff2?|ico|gif|webp|map)(\?|$)/i.test(r.url);
    if (!isApi || staticLike) return;
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
    apiCount++;
    fs.writeFileSync(OUT_ALL, JSON.stringify(all, null, 2));
    fs.writeFileSync(OUT, JSON.stringify(rec, null, 2));
    console.log("REQ", r.method, r.url.slice(0, 140), "| postData len:", (r.postData || "").length);
  }
  if (m.method === "Network.responseReceived" && lastApi && m.params.requestId === lastApi.requestId) {
    lastApi.status = m.params.response.status;
    fs.writeFileSync(OUT_ALL, JSON.stringify(all, null, 2));
    console.log("  RESP status:", m.params.response.status);
    c.cmd("Network.getResponseBody", { requestId: m.params.requestId }).then((rb) => {
      if (rb.result && rb.result.body) {
        lastApi.body = rb.result.body.slice(0, 3000);
        fs.writeFileSync(OUT_ALL, JSON.stringify(all, null, 2));
        console.log("  BODY head:", rb.result.body.slice(0, 250).replace(/\n/g, " | "));
      }
    }).catch(() => {});
  }
});

console.log("listening on agent.minimaxi.com for", SECONDS, "seconds...");
console.log("请在浏览器里登录 agent.minimaxi.com 并发一条消息(如\"你好\")");
setTimeout(() => {
  console.log("window ended, captured", apiCount, "requests ->", OUT_ALL);
  c.close();
  process.exit(0);
}, SECONDS * 1000);
