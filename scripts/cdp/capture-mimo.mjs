// Hook 并保存 aistudio.xiaomimimo.com 的全部 API 请求(method/url/postData/headers + 响应)。
// 用途:逆向小米 Mimo 网页协议(chat 发消息、ASR 语音识别、认证、SSE 格式)。
// 用法: node capture-mimo.mjs [监听秒数=600]
// 输出: %TEMP%/mimo_capture_all.json(完整列表)+ %TEMP%/mimo_capture.json(最后一条)
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);

const SECONDS = parseInt(process.argv[2] || "600", 10);
const OUT = path.join(os.tmpdir(), "mimo_capture.json");
const OUT_ALL = path.join(os.tmpdir(), "mimo_capture_all.json");

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
  const page = targets.find((t) => t.type === "page" && t.url.includes("mimimo.com"));
  if (page) return page;
  await new Promise((res, rej) => {
    const req = http.request(
      { host: "127.0.0.1", port: 9222, path: "/json/new?" + encodeURIComponent("https://aistudio.xiaomimimo.com/"), method: "PUT" },
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
  return targets2.find((t) => t.type === "page" && t.url.includes("mimimo.com")) || null;
}

const page = await findOrOpen();
if (!page) {
  console.error("no mimimo page target");
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
    const staticLike = /\.(js|css|png|jpg|jpeg|svg|woff2?|ico|gif|webp|map|ttf|otf)(\?|$)/i.test(r.url);
    if (staticLike) return;
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
        lastApi.body = rb.result.body.slice(0, 4000);
        fs.writeFileSync(OUT_ALL, JSON.stringify(all, null, 2));
        console.log("  BODY head:", rb.result.body.slice(0, 250).replace(/\n/g, " | "));
      }
    }).catch(() => {});
  }
});

console.log("listening on aistudio.xiaomimimo.com for", SECONDS, "seconds...");
console.log("操作: 1) 登录并发送一条文字消息(如\"你好\"); 2) 如页面有语音输入,再录/传一段语音测 ASR");
setTimeout(() => {
  console.log("window ended, captured", apiCount, "requests ->", OUT_ALL);
  c.close();
  process.exit(0);
}, SECONDS * 1000);
