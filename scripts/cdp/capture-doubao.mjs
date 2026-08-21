// scripts/cdp/capture-doubao.mjs — 捕获豆包最新 completion 请求参数(会话级签名)
//
// 背景:a_bogus 是"URL 参数集"的签名 —— 改 prompt 无碍,但参数集
// (web_tab_id 会话级/msToken/pc_version 等)变了就报 "common invalid param"。
// 因此必须**从页面最近一次真实 completion 请求**提取参数集,写 doubao_accounts.json,
// aurora 每次请求重读该文件(见 doubaoweb/provider 改动)。
// 前提:小号浏览器已登录 www.doubao.com,页面有对话活动(发一条消息即触发捕获)。
//
// 用法: node capture-doubao.mjs [--push] [--listen 秒数]
import http from "node:http";
import fs from "node:fs";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const PUSH = process.argv.includes("--push");
const LISTEN = parseInt(process.argv[process.argv.indexOf("--listen") + 1] || "30", 10);
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
  await sleep(10000);
  const t2 = await getJSON("/json");
  page = t2.find((t) => t.type === "page" && /doubao\.com/.test(t.url));
}
if (!page) { console.log("no doubao page"); process.exit(1); }
console.log("page:", page.url.slice(0, 100));
const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Network.enable");

async function writeFile(content) {
  const files = [path.join(TOKEN_DIR, "doubao_accounts.json")];
  try { fs.mkdirSync(DRIVE_DIR, { recursive: true }); files.push(path.join(DRIVE_DIR, "doubao_accounts.json")); } catch {}
  for (const f of files) fs.writeFileSync(f, content + "\n");
  if (PUSH) {
    const { execSync } = await import("node:child_process");
    try {
      execSync('scp -o BatchMode=yes -o ConnectTimeout=10 "D:/repos/aurora/.runtime/tokens/doubao_accounts.json" zxsadmin@10.10.10.2:/volume2/docker/aurora/tokens/', { stdio: "ignore" });
      console.log("pushed to NAS");
    } catch (e) { console.log("push failed:", e.message); }
  }
}

async function captureFromReq(url, postData) {
  const u = new URL(url);
  const p = u.searchParams;
  const gr = await c.cmd("Network.getCookies", { urls: ["https://www.doubao.com/"] });
  const cookieStr = gr.result.cookies.map((x) => `${x.name}=${x.value}`).join("; ");
  const ver = p.get("pc_version") || p.get("doubao_pc_version") || "3.32.61";
  const entry = {
    cookie: cookieStr,
    aid: p.get("aid") || "",
    device_id: p.get("device_id") || "",
    fp: p.get("fp") || "",
    ms_token: p.get("msToken") || p.get("ms_token") || "",
    a_bogus: p.get("a_bogus") || "",
    web_id: p.get("web_id") || "",
    tea_uuid: p.get("tea_uuid") || "",
    web_tab_id: p.get("web_tab_id") || "",
    bot_id: p.get("bot_id") || "",
    version: ver,
    // query 与 template:a_bogus 绑定 URL 参数 + body 的 conversation 字段,
    // 必须整段复用捕获值(改 prompt 文本即可,其余原样)。
    query: u.searchParams.toString(),
    template: postData || "",
  };
  const missing = ["aid", "device_id", "a_bogus", "web_id", "web_tab_id"].filter((k) => !entry[k]);
  if (missing.length) { console.log("缺参数:", missing.join(",")); return; }
  if (!entry.template) { console.log("缺 template(postData)"); return; }
  const content = JSON.stringify([entry], null, 2);
  await writeFile(content);
  console.log("✅ 捕获完成: a_bogus len=" + entry.a_bogus.length, "web_tab_id=" + entry.web_tab_id.slice(0, 12), "version=" + ver, "template len=" + entry.template.length);
}

c.on((m) => {
  if (m.method !== "Network.requestWillBeSent") return;
  const r = m.params.request;
  if (!r || !r.url) return;
  if (r.url.includes("/chat/completion") || r.url.includes("completion?")) {
    console.log("捕获到 completion 请求:", r.url.slice(0, 100));
    captureFromReq(r.url, r.postData || "").catch((e) => console.log("capture err:", e.message));
  }
});

console.log("监听 " + LISTEN + "s 内的 completion 请求...");
await sleep(LISTEN * 1000);
console.log("done(若未捕获:在页面发一条消息触发)");
c.close();
