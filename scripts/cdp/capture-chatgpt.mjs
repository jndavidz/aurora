// scripts/cdp/capture-chatgpt.mjs — 抓取 chatgpt.com 登录态凭证
//
// 前提:小号浏览器已登录 chatgpt.com(用户手动)。本脚本:
//   1. 唤醒 Chrome,打开 chatgpt.com;
//   2. 页面上下文 fetch /api/auth/session 拿 accessToken(实测 ~90 天有效)
//      + 抓 __Secure-next-auth.session-token cookie;
//   3. 回写 .runtime/tokens/{access,session}_tokens.txt(仓库 + Drive 区);
//   4. 加 --push 直接 scp 到 NAS 容器挂载目录。
// 注意:2026-08 起 aurora 的 session→access exchange 链路(bogdanfinn)会被
// ChatGPT 判无效(token_expired),直接抓页面 accessToken 最可靠。
//
// 用法: node capture-chatgpt.mjs [--push]
import http from "node:http";
import fs from "node:fs";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const PUSH = process.argv.includes("--push");
const TOKEN_DIR = "D:/repos/aurora/.runtime/tokens";
const DRIVE_DIR = "D:/dev/apps/aurora/.runtime/tokens";
const SITE_URL = "https://chatgpt.com/";

function httpReq(port, method, p) {
  return new Promise((resolve, reject) => {
    const r = http.request({ host: "127.0.0.1", port, path: p, method }, (res) => {
      let d = "";
      res.on("data", (c) => (d += c));
      res.on("end", () => resolve({ status: res.statusCode, body: d }));
    });
    r.on("error", reject);
    r.end();
  });
}
async function getJSON(port, p) { return JSON.parse((await httpReq(port, "GET", p)).body); }

try { await httpReq(8798, "POST", "/wake"); } catch (e) { console.log("wake fail:", e.message); }
for (let i = 0; i < 40; i++) { try { await getJSON(9222, "/json/version"); break; } catch { await sleep(2000); } }

const targets = await getJSON(9222, "/json");
let t = targets.find((x) => x.type === "page" && x.url.startsWith(SITE_URL));
if (!t) {
  const r = await httpReq(9222, "PUT", "/json/new?" + encodeURIComponent(SITE_URL));
  t = JSON.parse(r.body);
}
const c = await cdp(t.webSocketDebuggerUrl);

// 等页面加载完成
for (let i = 0; i < 25; i++) {
  try {
    const r = await c.cmd("Runtime.evaluate", { expression: "document.readyState", returnByValue: true });
    if (r.result && r.result.result && r.result.result.value === "complete") break;
  } catch { }
  await sleep(2000);
}

// 页面上下文 fetch /api/auth/session(自动带 cookie,同源)
let at = null, atExp = null;
for (let i = 0; i < 10; i++) {
  try {
    const r = await c.cmd("Runtime.evaluate", {
      expression: `(async () => { try { const r = await fetch('/api/auth/session', { credentials: 'include' }); const j = await r.json(); return JSON.stringify({ at: j.accessToken || '', expires: j.expires || null }); } catch (e) { return JSON.stringify({ at: '', expires: null }); } })()`,
      awaitPromise: true,
      returnByValue: true,
    });
    const v = r.result && r.result.result && r.result.result.value;
    if (v) {
      const j = JSON.parse(v);
      if (j.at && j.at.length > 20) { at = j.at; atExp = j.expires; break; }
    }
  } catch { }
  await sleep(2000);
}

// session token cookie
const gr = await c.cmd("Network.getCookies", { urls: [SITE_URL] });
const session = gr.result.cookies.find((x) => x.name === "__Secure-next-auth.session-token");
const sess = session && session.value.length > 20 ? session.value : null;

console.log("accessToken:", at ? "✓" : "✗ 未登录?");
console.log("access expires:", atExp || "(未知)");
console.log("session-token:", sess ? "✓" : "✗");

function write(name, content) {
  if (!content) return;
  const files = [path.join(TOKEN_DIR, name)];
  try { fs.mkdirSync(DRIVE_DIR, { recursive: true }); files.push(path.join(DRIVE_DIR, name)); } catch { }
  for (const f of files) fs.writeFileSync(f, content + "\n");
  console.log("written:", name);
}
if (at) write("access_tokens.txt", at);
if (sess) write("session_tokens.txt", sess);

if (PUSH && (at || sess)) {
  const { execSync } = await import("node:child_process");
  for (const f of ["access_tokens.txt", "session_tokens.txt"]) {
    if (!fs.existsSync(path.join(TOKEN_DIR, f))) continue;
    try {
      execSync(`scp -o BatchMode=yes -o ConnectTimeout=10 "${path.join(TOKEN_DIR, f)}" zxsadmin@10.10.10.2:/volume2/docker/aurora/tokens/`, { stdio: "inherit" });
      console.log("pushed:", f);
    } catch (e) { console.log("push failed:", f, e.message); }
  }
}
c.close();
console.log(at ? "capture done" : "capture FAILED (未登录?)");
process.exit(at ? 0 : 1);
