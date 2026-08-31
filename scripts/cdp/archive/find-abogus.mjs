// 检查 bdms 完整方法 + 全量搜已加载 JS 里的 a_bogus
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

// 1. bdms 完整方法
const r1 = await c.cmd("Runtime.evaluate", {
  expression: `JSON.stringify({ own: Object.getOwnPropertyNames(window.bdms || {}), proto: Object.getOwnPropertyNames(Object.getPrototypeOf(window.bdms) || {}).slice(0,20) })`,
  returnByValue: true,
});
console.log("bdms 方法:", r1.result && r1.result.result && r1.result.result.value);

// 2. 全量搜已加载 JS 资源里的 a_bogus
const r2 = await c.cmd("Runtime.evaluate", {
  expression: `JSON.stringify(performance.getEntriesByType('resource').filter(e => e.initiatorType === 'script').map(e => e.name))`,
  returnByValue: true,
});
const urls = JSON.parse(r2.result.result.value);
const hits = [];
for (const u of urls) {
  if (!/\.js/.test(u)) continue;
  const fr = await c.cmd("Runtime.evaluate", {
    expression: `(async () => { try { const r = await fetch(${JSON.stringify(u)}); const t = await r.text(); return t; } catch (e) { return ''; } })()`,
    awaitPromise: true,
    returnByValue: true,
  });
  const code = fr.result && fr.result.result && fr.result.result.value;
  if (!code) continue;
  if (code.includes("a_bogus")) {
    hits.push({ f: u.split("/").pop().split("?")[0], len: code.length });
    const name = u.split("/").pop().split("?")[0];
    fs.writeFileSync("D:/repos/aurora/.runtime/doubao-js/" + name, code);
    process.stdout.write("!");
  }
}
console.log("\na_bogus 命中:", JSON.stringify(hits));
c.close();
