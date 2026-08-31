// 完整测试:bdms.frontierSign 生成签名 → URL 拼装 → 页面 fetch chat
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

const acct = JSON.parse(fs.readFileSync("D:/repos/aurora/.runtime/tokens/doubao_accounts.json", "utf8"))[0];
const js = `(async () => {
  try {
    // 1. 构造排序后的 query(去旧 a_bogus,新 web_tab_id)
    const params = new URLSearchParams(${JSON.stringify(acct.query)});
    params.delete('a_bogus');
    params.set('web_tab_id', crypto.randomUUID ? crypto.randomUUID() : 't');
    const sorted = [...params.entries()].sort((a, b) => a[0].localeCompare(b[0]));
    const qs = sorted.map(([k, v]) => k + '=' + v).join('&');

    // 2. 签名
    const sign = window.bdms.frontierSign(qs);
    const bogus = (sign && sign['X-Bogus']) || (sign && sign.a_bogus) || '';
    if (!bogus) return JSON.stringify({ err: 'no bogus from sign', sign: JSON.stringify(sign).slice(0, 80) });

    // 3. 拼 URL(先试 a_bogus 参数名)
    const url = 'https://www.doubao.com/chat/completion?' + qs + '&a_bogus=' + encodeURIComponent(bogus);

    // 4. body:模板结构 + 新消息(新 uuid,参考 aurora buildReqBody)
    const body = JSON.parse(${JSON.stringify(acct.template)});
    body.messages = [{
      local_message_id: crypto.randomUUID ? crypto.randomUUID() : 'm1',
      content_block: [{
        block_type: 10000,
        content: { text_block: { text: 'reply ok', icon_url: '', icon_url_dark: '', summary: '' }, pc_event_block: '' },
        block_id: crypto.randomUUID ? crypto.randomUUID() : 'b1',
        parent_id: '', meta_info: [], append_fields: []
      }],
      message_status: 0
    }];

    // 5. fetch
    const r = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), credentials: 'include' });
    const txt = await r.text();
    return JSON.stringify({ status: r.status, bogusLen: bogus.length, urlLen: url.length, head: txt.slice(0, 200) });
  } catch (e) { return JSON.stringify({ err: String(e) }); }
})()`;
const r = await c.cmd("Runtime.evaluate", { expression: js, awaitPromise: true, returnByValue: true });
console.log("完整链路:", r.result && r.result.result && r.result.result.value);
c.close();
