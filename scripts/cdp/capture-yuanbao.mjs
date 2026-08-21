// scripts/cdp/capture-yuanbao.mjs — 抓取元宝(混元)会话认证头 + chat body 模板
//
// 背景:混元直连逆向已风控 2 个账号,改用真实浏览器页内 fetch 重放(桥适配器)。
// 认证头(X-Uskey/X-HY93/X-device-id 等)会话级复用 —— 从用户手动发的一条消息
// 的真实请求里捕获一次,存 .runtime/bridge/yuanbao_headers.json。
// 登录态过期(接口返回 23000)时:浏览器重新登录后重跑本脚本。
//
// 用法: node capture-yuanbao.mjs [监听秒数=120]
import http from "node:http";
import fs from "node:fs";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const SECONDS = parseInt(process.argv[2] || "120", 10);
const OUT = "D:/repos/aurora/.runtime/bridge/yuanbao_headers.json";
const AGENT_ID = "naQivTmsDa";

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
const page = targets.find((t) => t.type === "page" && /yuanbao\.tencent\.com/.test(t.url));
if (!page) { console.log("no yuanbao page(请先打开并登录 yuanbao.tencent.com)"); process.exit(1); }
console.log("page:", page.url.slice(0, 90));
const c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Network.enable");

// 捕获 chat 请求的 headers + postData(会话级认证头)
c.on((m) => {
  if (m.method !== "Network.requestWillBeSent") return;
  const r = m.params.request;
  if (!r || !r.url || !r.url.includes("/api/chat/") || !r.postData) return;
  console.log("★ chat 请求捕获:", r.url.slice(0, 70));
  try {
    const chatBody = JSON.parse(r.postData);
    // 保留认证头(过滤浏览器自动头)
    const keep = [
      "X-Uskey", "X-Instance-ID", "X-Event-Input-Type", "Content-Type", "x-commit-tag",
      "chat_version", "X-Input-Type", "X-Bus-Params-Md5", "X-Web-Third-Source", "X-HY93",
      "X-device-id", "X-AgentID", "X-Trid-Channel", "X-Platform", "X-Source", "X-Timestamp",
      "X-HY92", "X-webdriver", "X-ybuitest", "X-WebVersion", "X-Language", "X-Requested-With",
      "X-Exp-Params", "X-os_version", "X-HY106", "x-web-ch-id", "X-Traceparent",
      "Referer", "User-Agent", "Accept-Language", "Accept",
    ];
    const headers = {};
    for (const k of keep) {
      const v = r.headers[k];
      if (v !== undefined) headers[k] = v;
    }
    // 固定动态值(不随会话变化的部分;X-AgentID 由桥按 cid 覆盖)
    headers["X-AgentID"] = AGENT_ID + "/";
    const cfg = { headers, chatBody };
    fs.mkdirSync(path.dirname(OUT), { recursive: true });
    fs.writeFileSync(OUT, JSON.stringify(cfg, null, 2));
    console.log("✅ 已保存:", OUT);
    console.log("  X-Uskey len:", headers["X-Uskey"]?.length, "| chatModelId:", chatBody.chatModelId, "| conversationId:", chatBody.conversationId);
    c.close();
    process.exit(0);
  } catch (e) {
    console.log("解析失败:", e.message);
  }
});

console.log("监听 " + SECONDS + "s... 请现在在元宝页面发一条消息");
await sleep(SECONDS * 1000);
console.log("超时未捕获(在页面发一条消息触发 chat 请求)");
c.close();
