// doubao-sign-server.mjs — 豆包签名服务(常驻 NUC)
// 机制: 页面 window.bdms.frontierSign(query_string) 实时生成 X-Bogus(永不过期)
// 参考: wangchuxiaoji-oss/doubao2api browser_client.py:391
// 用法: node doubao-sign-server.mjs [port]
import http from "node:http";
import { cdp } from "./cdp-helper.mjs";

const PORT = parseInt(process.argv[2] || "8791", 10);

const getJSON = (p) => new Promise((res, rej) => {
  const r = http.request({ host: "127.0.0.1", port: 9222, path: p }, (x) => {
    let d = ""; x.on("data", (c) => (d += c)); x.on("end", () => res(d));
  });
  r.on("error", rej); r.end();
});

// 保持与豆包页面的 CDP 连接(懒连接 + 断线重连)
let conn = null;
async function getConn() {
  if (conn) {
    try { await conn.cmd("Runtime.evaluate", { expression: "1+1" }); return conn; }
    catch (e) { conn = null; }
  }
  const targets = JSON.parse(await getJSON("/json"));
  const page = targets.find((t) => t.type === "page" && /doubao\.com/.test(t.url));
  if (!page) throw new Error("doubao page not found");
  conn = await cdp(page.webSocketDebuggerUrl);
  return conn;
}

// 签名: 按源码调用 frontierSign, 传入排序后的 query string
async function sign(queryString) {
  const c = await getConn();
  const expr = `(() => { try { const s = window.bdms.frontierSign(${JSON.stringify(queryString)});
    const v = (s && typeof s === "object") ? (s["X-Bogus"] || s.a_bogus || "") : String(s || "");
    return JSON.stringify({ ok: !!v, sig: v }); } catch (e) { return JSON.stringify({ ok: false, err: e.message }); } })()`;
  const r = await c.cmd("Runtime.evaluate", { expression: expr, returnByValue: true });
  const out = JSON.parse(r.result?.result?.value || "{}");
  if (!out.ok) throw new Error(out.err || "sign failed");
  return out.sig;
}

http.createServer(async (req, res) => {
  const send = (code, obj) => { res.writeHead(code, { "Content-Type": "application/json" }); res.end(JSON.stringify(obj)); };
  if (req.url === "/health") { return send(200, { ok: true, service: "doubao-sign" }); }
  if (req.method !== "POST" || req.url !== "/sign") { return send(404, { error: "not found" }); }
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", async () => {
    try {
      const { query } = JSON.parse(body || "{}");
      if (!query) return send(400, { error: "missing query" });
      const sig = await sign(query);
      send(200, { ok: true, a_bogus: sig });
    } catch (e) { send(500, { error: e.message }); }
  });
}).listen(PORT, () => console.log(`[doubao-sign] listening on ${PORT}`));
