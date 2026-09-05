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
const OUT = process.env.DBHOOK_OUT || "/tmp/doubao_accounts.json";

const getJSON = (p) => new Promise((res, rej) => {
  http.get({ host: "127.0.0.1", port: 9222, path: p }, (r) => {
    let d = "";
    r.on("data", (c) => (d += c));
    r.on("end", () => { try { res(JSON.parse(d)); } catch (e) { rej(e); } });
  }).on("error", rej);
});

function findAllPages() {
  return getJSON("/json").then((ts) => ts.filter((t) => t.type === "page" && /doubao/.test(t.url)));
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
      ...(body ? { template: JSON.stringify(body) } : {}),
    };
    fs.mkdirSync(path.dirname(OUT), { recursive: true });
    // 合并:保留旧文件的其它信息(如多条账号),更新第一条
    let list = [];
    try { list = JSON.parse(fs.readFileSync(OUT, "utf8")); } catch (e) {}
    if (!Array.isArray(list)) list = [];
    // merge:Network 通道无 postData 时保留旧 template(a_bogus/msToken 仍更新)
    list[0] = { ...(list[0] || {}), ...acct };
    fs.writeFileSync(OUT, JSON.stringify(list, null, 2));
    // 同步 NAS(aurora 直连读 NAS 文件;ssh 免密,Windows OpenSSH)
    let pushed = false;
    try {
      const { execSync } = await import("node:child_process");
      execSync(`cat ${OUT} | ssh -o BatchMode=yes zxsadmin@10.10.10.2 "cat > /volume2/docker/aurora/tokens/doubao_accounts.json"`, { stdio: "ignore", timeout: 20000, shell: "/bin/bash" });
      pushed = true;
    } catch (pe) {
      console.log("[doubao-hook] push 失败:", String(pe.message).slice(0, 60));
    }
    console.log(`[doubao-hook] 捕获更新${pushed ? "+push" : "(本机)"}: a_bogus len=${aBogus.length} web_tab_id=${(q.web_tab_id || "").slice(0, 8)} @${new Date().toLocaleTimeString()}`);
  } catch (e) {
    console.log("[doubao-hook] 保存失败:", e.message);
  }
}

async function main() {
console.log("[doubao-hook] 启动...");
let conns = []; // [{ url, c }]
let lastCapture = 0;

async function ensureConnections() {
  const pages = await findAllPages();
  // 关闭已消失的连接
  conns = conns.filter((conn) => pages.some((p) => p.url === conn.url));
  // 新页面建立连接
  for (const p of pages) {
    if (!conns.some((conn) => conn.url === p.url)) {
      try {
        const c = await cdp(p.webSocketDebuggerUrl);
        await c.cmd("Network.enable").catch(() => {});
        await c.cmd("Page.enable").catch(() => {});
        await c.cmd("Page.addScriptToEvaluateOnNewDocument", { source: HOOK_JS }).catch(() => {});
        await injectHook(c).catch(() => {}); // 对已加载页面立即注入一次
        // Network 事件监听(主通道): 豆包前端可能走 XHR 而非 fetch,hook 抓不到
        c.on((m) => {
          if (m.method !== "Network.requestWillBeSent") return;
          const req = m.params && m.params.request;
          if (!req || !req.url || !req.url.includes("/chat/completion")) return;
          latestNetReq = { url: req.url, postData: req.postData || "", ts: Date.now() };
        });
        conns.push({ url: p.url, c });
        console.log("[doubao-hook] 连接:", p.url.slice(0, 60));
      } catch (e) {}
    }
  }
}

while (true) {
  try {
    await ensureConnections();
    for (const conn of conns) {
      try {
        const r = await conn.c.cmd("Runtime.evaluate", {
          expression: `JSON.stringify(window.__dbHook ? window.__dbHook.latest : null)`,
          returnByValue: true,
        });
        const latest = JSON.parse(r.result && r.result.result && r.result.result.value || "null");
        if (latest && latest.ts > lastCapture && typeof latest.url === "string" && latest.url.startsWith("http")) {
          // a_bogus 非空校验:空的参数集无价值且会覆盖 NAS 上有效凭据
          const ab = new URL(latest.url).searchParams.get("a_bogus") || "";
          if (ab.length < 50) { console.log("[doubao-hook] skip: a_bogus empty(len=" + ab.length + ")"); continue; }
          lastCapture = latest.ts;
          await saveCapture(conn.c, latest.url, latest.body);
        }
      } catch (e) {}
    }
  } catch (e) {
    await sleep(5000);
  }
  await sleep(3000);
}
}
main();
