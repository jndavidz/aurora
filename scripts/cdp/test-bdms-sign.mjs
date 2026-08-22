// 测试 window.bdms.frontierSign 两种调用方式
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
const c = await cdp(page.webSocketDebuggerUrl);

const js = `(function(){
  const out = {};
  try {
    // 方式 A:传 query string(参考 doubao-2api)
    const q = 'aid=497858&device_platform=web&web_tab_id=' + (crypto.randomUUID ? crypto.randomUUID() : 'x');
    const rA = window.bdms.frontierSign(q);
    out.methodA = { type: typeof rA, keys: rA && typeof rA === 'object' ? Object.keys(rA) : null, a_bogus: (rA && rA.a_bogus || '').slice(0, 15), raw: String(rA).slice(0, 40) };
  } catch (e) { out.methodA = { err: String(e) }; }
  try {
    // 方式 B:传 X-MS-STUB 对象(参考 s2-chat-runtime)
    const rB = window.bdms.frontierSign({ 'X-MS-STUB': 'test' });
    out.methodB = { type: typeof rB, keys: rB && typeof rB === 'object' ? Object.keys(rB) : null, xbogus: (rB && rB['X-Bogus'] || '').slice(0, 15), raw: String(rB).slice(0, 40) };
  } catch (e) { out.methodB = { err: String(e) }; }
  return JSON.stringify(out);
})()`;
const r = await c.cmd("Runtime.evaluate", { expression: js, returnByValue: true });
console.log("测试:", r.result && r.result.result && r.result.result.value);
c.close();
