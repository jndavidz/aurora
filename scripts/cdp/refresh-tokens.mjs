// scripts/cdp/refresh-tokens.mjs — 每周保活窗口代取 MiniMax/Mimo 凭证
//
// 背景:两站登录态寿命短(Mimo cookie 固定 30 天;MiniMax JWT 38 天 / 登录
// cookie 最长 60 天),凭证只在浏览器登录态活着时"重新登录"才刷新,无法像
// Gemini/Claude 那样全自动。本脚本做半自动代取:每周唤醒 Chrome,从页面读
// 新鲜凭证回写 token 文件(仓库 + Drive 同步区 → NAS)。登录态死亡(WARN)
// 时需人工在小号浏览器重新登录一次,之后本脚本自动接管。
//
// 输出:全部成功 exit 0;任一站失败 exit 1(次日保活窗口自动重试)。
// 由 keepalive-daily.ps1 调用,先于 keepalive-node.mjs(Browser.close 关浏览器)。
import http from "node:http";
import fs from "node:fs";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);

const KEEPER_PORT = 8798;
const CDP_PORT = 9222;
const TOKEN_DIR = "D:/repos/aurora/.runtime/tokens";
const DRIVE_DIR = "D:/dev/apps/aurora/.runtime/tokens";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function httpReq(port, method, p, body) {
  return new Promise((resolve, reject) => {
    const r = http.request({ host: "127.0.0.1", port, path: p, method }, (res) => {
      let d = "";
      res.on("data", (c) => (d += c));
      res.on("end", () => resolve({ status: res.statusCode, body: d }));
    });
    r.on("error", reject);
    if (body) r.write(body);
    r.end();
  });
}

async function getJSON(port, p) {
  const r = await httpReq(port, "GET", p);
  return JSON.parse(r.body);
}

async function wake() {
  try {
    const w = await httpReq(KEEPER_PORT, "POST", "/wake");
    console.log("[refresh] wake:", w.status, w.body.slice(0, 80));
  } catch (e) {
    console.log("[refresh] wake failed:", e.message);
    process.exitCode = 1;
  }
}

async function waitCdp() {
  for (let i = 0; i < 40; i++) {
    try { await getJSON(CDP_PORT, "/json/version"); return true; } catch { }
    await sleep(2000);
  }
  return false;
}

// 找/开指定站点的页面 target(Chrome 刚恢复标签页时轮询等待)
async function ensurePage(url) {
  for (let i = 0; i < 25; i++) {
    try {
      const targets = await getJSON(CDP_PORT, "/json");
      const t = targets.find((x) => x.type === "page" && x.url.startsWith(url));
      if (t) return t;
    } catch { }
    await sleep(2000);
  }
  const r = await httpReq(CDP_PORT, "PUT", "/json/new?" + encodeURIComponent(url));
  const t = JSON.parse(r.body);
  console.log("[refresh] opened new page:", url);
  return t;
}

async function refreshMinimax() {
  const url = "https://agent.minimaxi.com/";
  const t = await ensurePage(url);
  const c = await cdp(t.webSocketDebuggerUrl);
  let token = null;
  for (let i = 0; i < 25; i++) {
    try {
      const r = await c.cmd("Runtime.evaluate", { expression: "localStorage.getItem('_token')", returnByValue: true });
      token = r.result && r.result.result && r.result.result.value;
    } catch { }
    if (token) break;
    await sleep(2000);
  }
  if (!token) { console.log("[refresh] minimax: FAIL no token in localStorage"); c.close(); return false; }
  let exp = 0;
  try { exp = JSON.parse(Buffer.from(token.split(".")[1], "base64url").toString()).exp; } catch { }
  if (!exp || exp * 1000 <= Date.now()) { console.log("[refresh] minimax: FAIL token expired (exp=" + exp + ")"); c.close(); return false; }
  const days = Math.round((exp * 1000 - Date.now()) / 86400000);
  writeToken("minimax_tokens.txt", token);
  console.log("[refresh] minimax: OK 剩余 " + days + " 天" + (days <= 14 ? "  <<< 请登录刷新!" : ""));
  c.close();
  return true;
}

async function refreshMimo() {
  const url = "https://aistudio.xiaomimimo.com/";
  const t = await ensurePage(url);
  const c = await cdp(t.webSocketDebuggerUrl);
  let cookies = [];
  try {
    const gr = await c.cmd("Network.getCookies", { urls: [url] });
    cookies = gr.result.cookies;
  } catch (e) {
    console.log("[refresh] mimo: FAIL getCookies:", e.message);
    c.close();
    return false;
  }
  const get = (n) => cookies.find((x) => x.name === n);
  const ph = get("xiaomichatbot_ph");
  const st = get("xiaomichatbot_serviceToken");
  const uid = get("userId");
  if (!ph || !st) { console.log("[refresh] mimo: FAIL missing cookies (logged out?)"); c.close(); return false; }
  if (ph.expires !== -1 && ph.expires * 1000 <= Date.now()) { console.log("[refresh] mimo: FAIL cookie expired"); c.close(); return false; }
  const days = ph.expires === -1 ? null : Math.round((ph.expires * 1000 - Date.now()) / 86400000);
  const line = `xiaomichatbot_ph="${ph.value}"; xiaomichatbot_serviceToken="${st.value}"; userId=${uid ? uid.value : ""}`;
  writeToken("mimo_tokens.txt", line);
  console.log("[refresh] mimo: OK 剩余 " + (days === null ? "会话" : days + " 天") + (days !== null && days <= 14 ? "  <<< 请登录刷新!" : ""));
  c.close();
  return true;
}

function writeToken(name, content) {
  fs.writeFileSync(path.join(TOKEN_DIR, name), content + "\n");
  try {
    fs.mkdirSync(DRIVE_DIR, { recursive: true });
    fs.writeFileSync(path.join(DRIVE_DIR, name), content + "\n");
    console.log("[refresh]", name, "written (repo + Drive)");
  } catch (e) {
    console.log("[refresh]", name, "Drive write failed:", e.message);
  }
}

console.log("[refresh]", new Date().toISOString(), "start");
await wake();
if (!(await waitCdp())) { console.log("[refresh] CDP not ready"); process.exit(1); }
const ok1 = await refreshMinimax();
const ok2 = await refreshMimo();
process.exit(ok1 && ok2 ? 0 : 1);
