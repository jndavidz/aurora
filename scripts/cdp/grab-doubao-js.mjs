// 抓 doubao 页面已加载 JS,搜 a_bogus/frontierSign 签名代码
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
const page = targets.find((t) => t.type === "page" && /doubao/.test(t.url));
const c = await cdp(page.webSocketDebuggerUrl);
const r = await c.cmd("Runtime.evaluate", {
  expression: `JSON.stringify(performance.getEntriesByType('resource').filter(e => e.initiatorType === 'script').map(e => e.name))`,
  returnByValue: true,
});
const urls = JSON.parse(r.result.result.value);
console.log("JS 资源数:", urls.length);

const hits = [];
for (let i = 0; i < urls.length; i++) {
  const u = urls[i];
  if (!/\.js/.test(u)) continue;
  const fr = await c.cmd("Runtime.evaluate", {
    expression: `(async () => { try { const r = await fetch(${JSON.stringify(u)}); const t = await r.text(); return t; } catch (e) { return ''; } })()`,
    awaitPromise: true,
    returnByValue: true,
  });
  const code = fr.result && fr.result.result && fr.result.result.value;
  if (!code) continue;
  if (/frontierSign|byted_acrawler/.test(code)) {
    hits.push({ url: u, len: code.length, marker: "frontierSign" });
  } else if (/a_bogus/.test(code)) {
    hits.push({ url: u, len: code.length, marker: "a_bogus" });
  }
  // 存文件供后续分析
  const name = u.split("/").pop().split("?")[0];
  if (hits.length && hits[hits.length - 1].url === u) {
    fs.writeFileSync("D:/repos/aurora/.runtime/doubao-js/" + name, code);
  }
  process.stdout.write(".");
}
console.log("\n命中:", JSON.stringify(hits.map((h) => ({ f: h.url.split("/").pop(), len: h.len, m: h.marker }))));
c.close();
