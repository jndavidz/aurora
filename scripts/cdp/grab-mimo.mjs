// 抓取 mimo 认证:ph(cookie)+ 完整 cookie 串(含 .xiaomi.com)
// mimo 2026-08-23 改版:ph 动态(URL 参数),认证需完整登录 cookie。
// 用法(Windows node): node.exe D:/repos/aurora/scripts/cdp/grab-mimo.mjs
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
const page = targets.find((t) => t.type === "page" && /mimo/.test(t.url));
if (!page) { console.log("no mimo page"); process.exit(1); }
const c = await cdp(page.webSocketDebuggerUrl);
const gr = await c.cmd("Network.getAllCookies");
const all = gr.result.cookies || [];
const rel = all.filter((ck) => /xiaomimimo|xiaomi/i.test(ck.domain));
const phCk = all.find((x) => x.name === "xiaomichatbot_ph");
const ph = phCk ? phCk.value : "";
if (!ph) { console.log("cookie 中无 ph(未登录?)"); process.exit(1); }
// 完整 cookie 串(ph 也在其中,Go 端 Cookie 头带上无害)
const cookieStr = rel.map((x) => `${x.name}=${x.value}`).join("; ");
// 文件格式:mimoweb 从串提取 ph(URL 参数),整个串作 Cookie 头
const line = cookieStr;
fs.writeFileSync("D:/repos/aurora/.runtime/tokens/mimo_tokens.txt", line + "\n");
console.log("mimo token 已写: ph len=" + ph.length + " head=" + ph.slice(0, 8) + " cookie len=" + cookieStr.length + " cookies=" + rel.length);
c.close();
