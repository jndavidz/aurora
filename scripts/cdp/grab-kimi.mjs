// 抓取 kimi refresh_token(页面 localStorage)→ 写文件
import http from "node:http";
import fs from "node:fs";
const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);
const getJSON = (p) => new Promise((res, rej) => {
  http.get({ host: "127.0.0.1", port: 9222, path: p }, (r) => {
    let d = "";
    r.on("data", (c) => (d += c));
    r.on("end", () => { try { res(JSON.parse(d)); } catch (e) { rej(e); } });
  }).on("error", rej);
});
const targets = await getJSON("/json");
const page = targets.find((t) => t.type === "page" && /kimi\.com/.test(t.url));
if (!page) { console.log("no kimi page"); process.exit(1); }
const c = await cdp(page.webSocketDebuggerUrl);
const r = await c.cmd("Runtime.evaluate", {
  expression: `localStorage.getItem('refresh_token') || ''`,
  returnByValue: true,
});
const token = r.result && r.result.result && r.result.result.value;
if (!token || token.split(".").length !== 3) { console.log("无有效 refresh_token(未登录?)"); process.exit(1); }
try {
  const p = JSON.parse(Buffer.from(token.split(".")[1], "base64url").toString());
  console.log("exp:", new Date(p.exp * 1000).toISOString(), "剩余:", Math.round((p.exp * 1000 - Date.now()) / 86400000) + "天");
} catch {}
fs.writeFileSync("D:/repos/aurora/.runtime/tokens/kimi_tokens.txt", token + "\n");
console.log("kimi_tokens.txt 已更新(len=" + token.length + ")");
c.close();
