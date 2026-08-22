// scripts/cdp/doubao-hook.mjs — 常驻捕获豆包 completion 真实请求(hook fetch)
//
// 方案 B:页面内自动签名 —— hook 页面 fetch,捕获最新 completion 请求的
// URL(query 含 a_bogus 192 + 参数集)与 body(模板),更新 doubao_accounts.json,
// aurora 直连自动读取新模板(a_bogus 绑定参数集,复用捕获的完整 URL,只改 prompt)。
// 页面任意一次真实对话即自动刷新(无需手动 capture-doubao)。
//
// 用法(Windows node): node.exe D:/repos/aurora/scripts/cdp/doubao-hook.mjs
import http from "node:http";
import fs from "node:fs";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const OUT = "D:/repos/aurora/.runtime/tokens/doubao_accounts.json";

const getJSON = (p) => new Promise((res, rej) => {
  http.get({ host: "127.0.0.1", port: 9222, path: p }, (r) => {
    let d = "";
    r.on("data", (c) => (d += c));
    r.on("end", () => { try { res(JSON.parse(d)); } catch (e) { rej(e); } });
  }).on("error", rej);
});

function findPage() {
  return getJSON("/json").then((ts) => ts.find((t) => t.type === "page" && /doubao/.test(t.url)));
}

const HOOK_JS = `(() => {
  const orig = window.__dbHook && window.__dbHook.origFetch ? window.__dbHook.origFetch : window.fetch;
  const prevLatest = window.__dbHook && window.__dbHook.latest ? window.__dbHook.latest : null;
  window.__dbHook = { latest: prevLatest, origFetch: orig, count: 0 };
  window.fetch = function(...args) {
    const u = typeof args[0] === 'string' ? args[0] : (args[0] && args[0].url) || '';
    if (String(u).includes('/chat/completion')) {
      let body = null;
      let headers = null;
      try {
        const b = args[1] && args[1].body;
        if (typeof b === 'string') body = JSON.parse(b);
        else if (b instanceof URLSearchParams) body = Object.fromEntries(b);
        const h = args[1] && args[1].headers;
        if (h) headers = h instanceof Headers ? Object.fromEntries(h.entries()) : (typeof h === 'object' ? h : null);
      } catch (e) {}
      let full = String(u);
      try { full = new URL(String(u), location.href).href; } catch (e) {}
      window.__dbHook.latest = { url: full, body, headers, ts: Date.now() };
      window.__dbHook.count++;
    }
    return orig.apply(this, args);
  };
  return 'rebuilt';
})()`;

async function injectHook(c) {
  const r = await c.cmd("Runtime.evaluate", { expression: HOOK_JS, returnByValue: true });
  return r.result && r.result.result && r.result.result.value;
}

async function saveCapture(c, url, body) {
  try {
    const u = new URL(url);
    const q = {};
    for (const [k, v] of u.searchParams) q[k] = v;
    const aBogus = q.a_bogus || "";
    // cookie
    const gr = await c.cmd("Network.getCookies", { urls: ["https://www.doubao.com/"] });
    const cookies = (gr.result.cookies || []).map((x) => `${x.name}=${x.value}`).join("; ");
    const acct = {
      cookie: cookies,
      aid: q.aid || "",
      device_id: q.device_id || "",
      fp: q.fp || "",
      ms_token: q.msToken || "",
      a_bogus: aBogus,
      web_id: q.web_id || "",
      tea_uuid: q.tea_uuid || "",
      web_tab_id: q.web_tab_id || "",
      bot_id: (body && body.client_meta && body.client_meta.bot_id) || "",
      version: q.doubao_pc_version || q.pc_version || "",
      query: u.searchParams.toString(),
      template: JSON.stringify(body || {}),
    };
    fs.mkdirSync(path.dirname(OUT), { recursive: true });
    // 合并:保留旧文件的其它信息(如多条账号),更新第一条
    let list = [];
    try { list = JSON.parse(fs.readFileSync(OUT, "utf8")); } catch (e) {}
    if (!Array.isArray(list)) list = [];
    list[0] = acct;
    fs.writeFileSync(OUT, JSON.stringify(list, null, 2));
    console.log(`[doubao-hook] 捕获更新: a_bogus len=${aBogus.length} web_tab_id=${(q.web_tab_id || "").slice(0, 8)} body len=${(body && JSON.stringify(body).length) || 0} @${new Date().toLocaleTimeString()}`);
  } catch (e) {
    console.log("[doubao-hook] 保存失败:", e.message);
  }
}

async function main() {
console.log("[doubao-hook] 启动...");
let page = await findPage();
if (!page) { console.log("no doubao page"); process.exit(1); }
let c = await cdp(page.webSocketDebuggerUrl);
await c.cmd("Network.enable");
// 文档级注入(页面加载前执行,SDK 初始化时即拿到包装 fetch——晚注入对已缓存 fetch 引用的 SDK 无效)
await c.cmd("Page.enable").catch(() => {});
await c.cmd("Page.addScriptToEvaluateOnNewDocument", { source: HOOK_JS }).catch((e) => console.log("[doubao-hook] 文档注入失败:", e.message));
console.log("[doubao-hook] 已连接:", page.url.slice(0, 60));

let lastCapture = 0;
// 每 3s 检查一次捕获(页面刷新后 hook 丢失,重新注入)
while (true) {
  try {
    const inj = await injectHook(c);
    if (inj !== "already") console.log("[doubao-hook] hook 注入:", inj);
    const r = await c.cmd("Runtime.evaluate", {
      expression: `JSON.stringify(window.__dbHook ? window.__dbHook.latest : null)`,
      returnByValue: true,
    });
    const latest = JSON.parse(r.result && r.result.result && r.result.result.value || "null");
    if (latest && latest.ts > lastCapture && typeof latest.url === "string" && latest.url.startsWith("http")) {
      lastCapture = latest.ts;
      await saveCapture(c, latest.url, latest.body);
    }
  } catch (e) {
    // 页面/CDP 断开:重新连接
    try { c.close(); } catch (e2) {}
    await sleep(5000);
    page = await findPage();
    if (page) {
      try { c = await cdp(page.webSocketDebuggerUrl); await c.cmd("Network.enable"); console.log("[doubao-hook] 重连:", page.url.slice(0, 50)); } catch (e2) {}
    }
  }
  await sleep(3000);
}
}
main();
