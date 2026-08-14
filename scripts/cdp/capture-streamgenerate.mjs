// Hook 并保存 gemini.google.com 的 StreamGenerate 请求(完整 url + postData + headers)。
// 用途:从一条真实请求里提取会话级令牌 f.sid / SNlM6e(见 docs/GEMINI.md §六)。
// 用法: node capture-streamgenerate.mjs [监听秒数=120]
// 输出: %TEMP%/gem_capture.json(每次命中覆盖写入)
import { pathToFileURL } from "node:url";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);

const SECONDS = parseInt(process.argv[2] || "120", 10);
const OUT = path.join(os.tmpdir(), "gem_capture.json");

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

const targets = await getJSON("/json");
const page = targets.find((t) => t.type === "page" && t.url.includes("gemini.google.com"));
if (!page) {
  console.error("no gemini page target (is gemini.google.com open in the CDP browser?)");
  process.exit(1);
}
console.log("page:", page.url.slice(0, 80));

const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Network.enable");

c.on((m) => {
  if (m.method !== "Network.requestWillBeSent") return;
  const r = m.params.request;
  if (!r || !r.url || r.url.indexOf("StreamGenerate") === -1) return;
  const fsid = (r.url.match(/f\.sid=([^&]+)/) || [])[1] || "";
  const rec = {
    ts: new Date().toISOString(),
    url: r.url,
    f_sid: fsid,
    postData: r.postData || "",
    postDataLen: (r.postData || "").length,
    headers: r.headers || {},
  };
  fs.writeFileSync(OUT, JSON.stringify(rec, null, 2));
  console.log("CAPTURED StreamGenerate");
  console.log("f.sid =", fsid);
  console.log("postData len =", rec.postDataLen);
  console.log("saved to", OUT);
});

console.log("listening for StreamGenerate for", SECONDS, "seconds...");
setTimeout(() => {
  console.log("timeout, no capture in window");
  c.close();
  process.exit(0);
}, SECONDS * 1000);
