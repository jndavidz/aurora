// 在真实 gemini.google.com 页面上下文里用 fetch() 复刻 StreamGenerate 请求,
// 流式读回模型回复 —— "真浏览器绕过指纹"的核心验证脚本。
//
// 前置:
//   1. CDP 浏览器(9222)已登录 gemini.google.com
//   2. 已用 capture-streamgenerate.mjs 抓过一条真实请求
//      (会话令牌缓存在 %TEMP%/gem_capture.json 与 %TEMP%/gem_parsed.json)
//
// 用法: node gemini-replay.mjs "<你的问题>"
// 原理: 请求从真实页面的 JS 上下文发出 —— 真实 cookie、真实 Origin/Referer、
//       真实浏览器指纹全部由浏览器自带,服务端看到的就是真人页面发来的请求。
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);

const PROMPT = process.argv[2] || "用一句话介绍你自己";
const TMP = os.tmpdir();
const parsed = JSON.parse(fs.readFileSync(path.join(TMP, "gem_parsed.json"), "utf8"));
const raw = JSON.parse(fs.readFileSync(path.join(TMP, "gem_capture.json"), "utf8"));

// ── 从缓存重建请求(只替换 prompt + rid) ─────────────────────────
const inner = JSON.parse(JSON.stringify(parsed.inner));
inner[0] = [PROMPT, 0, null, null, null, null, 0];
const rid = "r_" + Math.random().toString(16).slice(2, 18);
if (Array.isArray(inner[2]) && inner[2].length >= 2) inner[2][1] = rid;

const fReq = JSON.stringify([null, JSON.stringify(inner)]);
const body = "f.req=" + encodeURIComponent(fReq) + "&at=" + encodeURIComponent(parsed.summary.at);
const url =
  "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate" +
  "?bl=boq_assistant-bard-web-server_20260812.16_p0" +
  "&f.sid=" + raw.f_sid +
  "&hl=zh-CN" +
  "&_reqid=" + Math.floor(Math.random() * 1e6) +
  "&rt=c";

const headers = {
  "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
  "X-Same-Domain": "1",
  "x-goog-ext-525005358-jspb": parsed.summary.h525005358,
  "x-goog-ext-525001261-jspb": parsed.summary.h525001261,
  "x-goog-ext-73010989-jspb": parsed.summary.h73010989,
  "x-goog-ext-73010990-jspb": parsed.summary.h73010990,
};

// ── 连 CDP,找 gemini 页面 ────────────────────────────────────────
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
  console.error("no gemini page target");
  process.exit(1);
}
const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Runtime.enable");

// 流式 chunk 经 console 回传(每个 chunk 一行,node 侧重组)
let collected = "";
c.on((m) => {
  if (m.method !== "Runtime.consoleAPICalled") return;
  for (const a of m.params.args || []) {
    if (a.type === "string" && a.value && a.value.startsWith("__CHUNK__")) {
      collected += a.value.slice(9);
    }
  }
});

// 页内 fetch + ReadableStream 逐块读取
const pageJs =
  "(async () => {" +
  "const url = " + JSON.stringify(url) + ";" +
  "const headers = " + JSON.stringify(headers) + ";" +
  "const body = " + JSON.stringify(body) + ";" +
  "const resp = await fetch(url, { method: 'POST', headers: headers, body: body, credentials: 'same-origin' });" +
  "if (!resp.ok) return 'HTTP ' + resp.status;" +
  "const reader = resp.body.getReader();" +
  "const dec = new TextDecoder();" +
  "let n = 0;" +
  "for (;;) { const r = await reader.read(); if (r.done) break; const t = dec.decode(r.value, { stream: true }); n += t.length; console.log('__CHUNK__' + t); }" +
  "return 'streamed ' + n + ' chars';" +
  "})()";

const r = await c.cmd("Runtime.evaluate", {
  expression: pageJs,
  awaitPromise: true,
  returnByValue: true,
});
console.log("page fetch:", JSON.stringify(r.result.result.value));
console.log("collected bytes:", collected.length);
c.close();

// ── 解析 RPC 帧(同 internal/geminweb/client.go 的 parseRPCFrame) ──
const events = [];
for (const line of collected.split(/\r?\n/)) {
  const l = line.trim();
  if (!l.startsWith("[[")) continue;
  let frames;
  try { frames = JSON.parse(l); } catch { continue; }
  for (const fr of frames) {
    if (!Array.isArray(fr) || fr.length < 3 || fr[0] !== "wrb.fr") continue;
    let payload;
    try { payload = JSON.parse(fr[2]); } catch { continue; }
    if (!Array.isArray(payload) || payload.length < 3) continue;
    if (payload[2] && typeof payload[2] === "object" && payload[2]["44"] === true) {
      events.push({ type: "done" });
    }
    if (Array.isArray(payload[4])) {
      for (const p of payload[4]) {
        if (Array.isArray(p) && Array.isArray(p[1]) && p[1].length > 0 && typeof p[1][0] === "string") {
          events.push({ type: "text", text: p[1][0] });
        }
      }
    }
  }
}
const texts = events.filter((e) => e.type === "text").map((e) => e.text);
const done = events.some((e) => e.type === "done");
console.log("=== parse result ===");
console.log("text frames:", texts.length, "| done:", done);
if (texts.length > 0) {
  console.log("reply:", texts[texts.length - 1]);
} else {
  console.log("raw head:", collected.slice(0, 300));
}
