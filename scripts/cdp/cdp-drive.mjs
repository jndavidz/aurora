#!/usr/bin/env node
// 通用 CDP 抓取 CLI：导航 → 等待 → 提取文本 / 截图
// 用法:
//   node cdp-drive.mjs <url> [--selector .RichContent-inner] [--out out.txt]
//        [--shot shot.png] [--wait-ms 8000] [--title] [--max-chars 60000]
//        [--host 127.0.0.1] [--port 9222]
// 默认行为: 依次尝试常见正文选择器; 找不到则取 body.innerText; 输出到 --out 或 stdout
// 平台: Windows 侧 node.exe 跑(直连 127.0.0.1:9222); 或 WSL 侧跑加 --host <宿主IP>
//       (启动浏览器: bash start-chrome-cdp.sh start)
import { cdp } from "./cdp-helper.mjs";
import http from "node:http";
import fs from "node:fs";

// ---- 参数解析 ----
const args = process.argv.slice(2);
const url = args.find((a) => a.startsWith("http"));
const opt = { host: "127.0.0.1", port: 9222, waitMs: 8000, maxChars: 60000, selectors: [] };
for (let i = 0; i < args.length; i++) {
  const a = args[i];
  const next = () => args[++i];
  if (a === "--selector") opt.selectors.push(next());
  else if (a === "--out") opt.out = next();
  else if (a === "--shot") opt.shot = next();
  else if (a === "--wait-ms") opt.waitMs = parseInt(next(), 10);
  else if (a === "--host") opt.host = next();
  else if (a === "--port") opt.port = parseInt(next(), 10);
  else if (a === "--title") opt.title = true;
  else if (a === "--max-chars") opt.maxChars = parseInt(next(), 10);
}
if (!url) {
  console.error("用法: node cdp-drive.mjs <url> [--selector .x] [--out f.txt] [--shot f.png] [--wait-ms N] [--host IP] [--port N] [--title] [--max-chars N]");
  process.exit(1);
}

// ---- CDP 基础设施 ----
function getJSON(p) {
  return new Promise((res, rej) => {
    http.get({ host: opt.host, port: opt.port, path: p }, (r) => {
      let d = "";
      r.on("data", (c) => (d += c));
      r.on("end", () => { try { res(JSON.parse(d)); } catch (e) { rej(e); } });
    }).on("error", rej);
  });
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 找已有 page; 没有则新建
let targets = await getJSON("/json");
let page = targets.find((t) => t.type === "page" && t.url !== "about:blank") || targets.find((t) => t.type === "page");
if (!page) {
  const ver = await getJSON("/json/version");
  const c0 = await cdp(ver.webSocketDebuggerUrl);
  const { result } = await c0.cmd("Target.createTarget", { url: "about:blank" });
  c0.close();
  await sleep(1200);
  targets = await getJSON("/json");
  page = targets.find((t) => t.id === result.targetId);
}
if (!page) { console.error("无可用 page target, 先启动浏览器: bash start-chrome-cdp.sh start"); process.exit(1); }
console.error("PAGE:", page.url.slice(0, 100));

const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Page.enable");
await c.cmd("Runtime.enable");

// 导航 + 等待
const loadP = c.waitEvent("Page.loadEventFired", 40000);
await c.cmd("Page.navigate", { url });
try { await loadP; } catch {}
await sleep(opt.waitMs);

// 提取文本(选择器回退链)
const fallback = [".RichContent-inner", ".RichText", ".AnswerCard", "article", ".Post-RichTextContainer", ".markdown-body", "#content", ".article-content"];
const selectors = opt.selectors.length ? opt.selectors : fallback;
let text = "";
for (const sel of selectors) {
  const r = await c.cmd("Runtime.evaluate", {
    expression: `(() => { const el = document.querySelector(${JSON.stringify(sel)}); return el ? el.innerText.slice(0, ${opt.maxChars}) : null; })()`,
    returnByValue: true,
  });
  const v = r.result?.result?.value;
  if (v && v.length > 100) { text = `[${sel}]\n${v}`; break; }
}
if (!text) {
  const r = await c.cmd("Runtime.evaluate", {
    expression: `document.body.innerText.slice(0, ${opt.maxChars})`,
    returnByValue: true,
  });
  text = "[body]\n" + (r.result?.result?.value || "");
}
const titleR = await c.cmd("Runtime.evaluate", { expression: "document.title", returnByValue: true });
const title = titleR.result?.result?.value || "";
const finalText = (opt.title ? `TITLE: ${title}\n\n` : "") + text;

if (opt.out) {
  fs.writeFileSync(opt.out, finalText, "utf8");
  console.log("SAVED", opt.out, finalText.length, "chars");
} else {
  console.log(finalText);
}

// 截图
if (opt.shot) {
  const s = await c.cmd("Page.captureScreenshot", { format: "png" });
  if (s.result?.data) {
    fs.writeFileSync(opt.shot, Buffer.from(s.result.data, "base64"));
    console.log("SHOT", opt.shot);
  } else {
    console.error("截图失败:", s.error?.message || "no data");
  }
}
c.close();
