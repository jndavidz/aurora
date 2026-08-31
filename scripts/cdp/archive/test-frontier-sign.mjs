// 验证 doubao 页面的 window.byted_acrawler.frontierSign(签名函数)
import http from "node:http";
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
if (!page) { console.log("no doubao page"); process.exit(1); }
const c = await cdp(page.webSocketDebuggerUrl);
const r = await c.cmd("Runtime.evaluate", {
  expression: `JSON.stringify({
    hasAcrawler: typeof window.byted_acrawler !== 'undefined',
    hasFrontierSign: !!(window.byted_acrawler && typeof window.byted_acrawler.frontierSign === 'function'),
    acrawlerKeys: window.byted_acrawler ? Object.keys(window.byted_acrawler).slice(0, 10) : []
  })`,
  returnByValue: true,
});
console.log("检查:", r.result && r.result.result && r.result.result.value);

// 实际调用测试(用页面当前参数)
const r2 = await c.cmd("Runtime.evaluate", {
  expression: `(function(){
    try {
      const q = 'aid=497858&device_platform=web&web_tab_id=' + (crypto.randomUUID ? crypto.randomUUID() : 'test') + '&test=1';
      const s = window.byted_acrawler.frontierSign(q);
      return JSON.stringify({ ok: true, type: typeof s, keys: s && typeof s === 'object' ? Object.keys(s) : null, abogus: (s && s.a_bogus || '').slice(0, 20), raw: String(s).slice(0, 60) });
    } catch (e) { return JSON.stringify({ ok: false, err: String(e) }); }
  })()`,
  returnByValue: true,
});
console.log("签名调用:", r2.result && r2.result.result && r2.result.result.value);
c.close();
