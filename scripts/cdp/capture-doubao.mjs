// scripts/cdp/capture-doubao.mjs — 抓取豆包(doubao.com)凭证
//
// 前提:小号浏览器已登录 www.doubao.com(用户手动)。
// 从页面已发出的 API 请求 URL 解析签名参数(cookie 直接读)。
// 注意:a_bogus 是分钟级签名 —— 抓取后仅短时可用,豆包定位为低频备用,
// 用前现抓。新版页面请求不含 fp/ms_token(老格式才需要),可留空。
//
// 用法: node capture-doubao.mjs [--push]
import http from "node:http";
import fs from "node:fs";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const PUSH = process.argv.includes("--push");
const TOKEN_DIR = "D:/repos/aurora/.runtime/tokens";
const DRIVE_DIR = "D:/dev/apps/aurora/.runtime/tokens";

function getJSON(p) {
  return new Promise((res, rej) => {
    http.get({ host: "127.0.0.1", port: 9222, path: p }, (r) => {
      let d = "";
      r.on("data", (c) => (d += c));
      r.on("end", () => { try { res(JSON.parse(d)); } catch (e) { rej(e); } });
    }).on("error", rej);
  });
}

const targets = await getJSON("/json");
let page = targets.find((t) => t.type === "page" && /doubao\.com/.test(t.url));
if (!page) {
  await new Promise((res) => {
    http.request({ host: "127.0.0.1", port: 9222, path: "/json/new?" + encodeURIComponent("https://www.doubao.com/"), method: "PUT" }, (r) => { r.on("end", res); r.resume(); }).end();
  });
  await sleep(6000);
  const t2 = await getJSON("/json");
  page = t2.find((t) => t.type === "page" && /doubao\.com/.test(t.url));
}
if (!page) { console.log("no doubao page"); process.exit(1); }
console.log("page:", page.url.slice(0, 100));

const c = await cdp(page.webSocketDebuggerUrl);

const r = await c.cmd("Runtime.evaluate", {
  expression: `JSON.stringify(performance.getEntriesByType('resource').map(e => e.name).filter(u => /doubao\\.com/.test(u) && u.includes('?')).slice(0, 300))`,
  returnByValue: true,
});
const urls = JSON.parse(r.result.result.value);

const KEYS = ["aid", "device_id", "fp", "ms_token", "a_bogus", "web_id", "tea_uuid", "web_tab_id", "bot_id", "version", "pc_version", "doubao_pc_version"];
const hit = {};
for (const u of urls) {
  try {
    const p = new URL(u);
    for (const k of KEYS) {
      if (!hit[k]) { const v = p.searchParams.get(k); if (v && v !== "0") hit[k] = v; }
    }
  } catch { }
}
const need = ["a_bogus", "aid", "device_id", "web_id"].filter((k) => !hit[k]);
console.log("params:", Object.keys(hit).join(",") || "NONE");
console.log(need.length ? "missing: " + need.join(",") + " (在页面发一条消息后再跑)" : "params complete");

const gr = await c.cmd("Network.getCookies", { urls: ["https://www.doubao.com/"] });
const cookieStr = gr.result.cookies.map((x) => `${x.name}=${x.value}`).join("; ");
console.log("cookie len:", cookieStr.length);

if (hit.a_bogus && hit.aid && cookieStr) {
  const entry = {
    cookie: cookieStr,
    aid: hit.aid || "",
    device_id: hit.device_id || "",
    fp: hit.fp || "",
    ms_token: hit.ms_token || "",
    a_bogus: hit.a_bogus || "",
    web_id: hit.web_id || "",
    tea_uuid: hit.tea_uuid || "",
    web_tab_id: hit.web_tab_id || "",
    bot_id: hit.bot_id || "",
    version: hit.version || hit.pc_version || hit.doubao_pc_version || "",
  };
  const content = JSON.stringify([entry], null, 2);
  const files = [path.join(TOKEN_DIR, "doubao_accounts.json")];
  try { fs.mkdirSync(DRIVE_DIR, { recursive: true }); files.push(path.join(DRIVE_DIR, "doubao_accounts.json")); } catch { }
  for (const f of files) fs.writeFileSync(f, content + "\n");
  console.log("written doubao_accounts.json");
  if (PUSH) {
    const { execSync } = await import("node:child_process");
    try {
      execSync('scp -o BatchMode=yes -o ConnectTimeout=10 "D:/repos/aurora/.runtime/tokens/doubao_accounts.json" zxsadmin@10.10.10.2:/volume2/docker/aurora/tokens/', { stdio: "inherit" });
      console.log("pushed to NAS");
    } catch (e) { console.log("push failed:", e.message); }
  }
} else {
  console.log("capture incomplete; rerun after sending a message in the doubao page");
}
c.close();
