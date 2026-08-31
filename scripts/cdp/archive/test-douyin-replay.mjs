// 用 douyin_sign 的 makeABogus 生成签名,测 doubao completion
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

// 注入 douyin_sign(若未注入)
for (const file of ["sm3.js", "utils.js", "vm_decode.js"]) {
  const code = fs.readFileSync("D:/repos/aurora/.runtime/douyin-sign/" + file, "utf8");
  await c.cmd("Runtime.evaluate", { expression: code, returnByValue: true });
}

const acct = JSON.parse(fs.readFileSync("D:/repos/aurora/.runtime/tokens/doubao_accounts.json", "utf8"))[0];
const js = `(async () => {
  try {
    // 1. query:去 a_bogus,新 web_tab_id(原序 + 排序都试)
    const params = new URLSearchParams(${JSON.stringify(acct.query)});
    params.delete('a_bogus');
    params.set('web_tab_id', crypto.randomUUID ? crypto.randomUUID() : 't');
    const entries = [...params.entries()];
    const qsRaw = entries.map(([k, v]) => k + '=' + v).join('&');
    const qsSorted = [...entries].sort((a, b) => a[0].localeCompare(b[0])).map(([k, v]) => k + '=' + v).join('&');

    // 2. makeABogus(两种顺序)
    const bRaw = makeABogus(qsRaw, 0);
    const bSorted = makeABogus(qsSorted, 0);
    const bogus = bRaw;
    const url = 'https://www.doubao.com/chat/completion?' + qsRaw + '&a_bogus=' + encodeURIComponent(bogus);

    // 3. body
    const body = JSON.parse(${JSON.stringify(acct.template)});
    body.messages = [{
      local_message_id: crypto.randomUUID ? crypto.randomUUID() : 'm1',
      content_block: [{ block_type: 10000, content: { text_block: { text: '回复ok两个字', icon_url: '', icon_url_dark: '', summary: '' }, pc_event_block: '' }, block_id: crypto.randomUUID ? crypto.randomUUID() : 'b1', parent_id: '', meta_info: [], append_fields: [] }],
      message_status: 0
    }];

    const r = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), credentials: 'include' });
    const txt = await r.text();
    const evs = [...txt.matchAll(/event: (\w+)/g)].map(m => m[1]);
    const counts = {};
    for (const e of evs) counts[e] = (counts[e] || 0) + 1;
    const tm = txt.match(/\"text\":\"([^\"]{0,50})/);
    return JSON.stringify({ status: r.status, bRaw: bRaw.length, bSorted: bSorted.length, events: counts, text: tm ? tm[1] : '', head: txt.slice(0, 100) });
  } catch (e) { return JSON.stringify({ err: String(e).slice(0, 200) }); }
})()`;
const r = await c.cmd("Runtime.evaluate", { expression: js, awaitPromise: true, returnByValue: true });
console.log("makeABogus 重放:", r.result && r.result.result && r.result.result.value);
c.close();
