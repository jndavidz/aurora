// 在 doubao 页面注入 douyin_sign 算法,生成 a_bogus 对比
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

// 注入 douyin_sign 代码(sm3 + utils + vm_decode)
const inject = async (file) => {
  const code = fs.readFileSync("D:/repos/aurora/.runtime/douyin-sign/" + file, "utf8");
  const r = await c.cmd("Runtime.evaluate", { expression: code, returnByValue: true });
  if (r.exceptionDetails) console.log(file, "注入异常:", JSON.stringify(r.exceptionDetails.exception).slice(0, 100));
  else console.log(file, "注入 OK");
};
await inject("sm3.js");
await inject("utils.js");
await inject("vm_decode.js");

// 用 doubao 真实参数调用 makeABogus
const acct = JSON.parse(fs.readFileSync("D:/repos/aurora/.runtime/tokens/doubao_accounts.json", "utf8"))[0];
const js = `(function(){
  try {
    const q = ${JSON.stringify(acct.query)};
    const out = makeABogus(q, 0);
    return JSON.stringify({ len: out.length, head: out.slice(0, 20), type: typeof out });
  } catch (e) { return JSON.stringify({ err: String(e).slice(0, 150) }); }
})()`;
const r = await c.cmd("Runtime.evaluate", { expression: js, returnByValue: true });
console.log("makeABogus(doubao):", r.result && r.result.result && r.result.result.value);
c.close();
