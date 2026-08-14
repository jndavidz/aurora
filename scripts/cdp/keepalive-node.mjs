// scripts/cdp/keepalive-node.mjs — 桥通道登录态保活(纯 node,供任务计划调用)
// 流程: 唤醒守护 → 等桥就绪 → 各桥 provider 发一条消息(活动=滚动续期)
//       → CDP Browser.close 优雅关闭 Chrome(桥进程由 keepalive.ps1 收尾或
//         自然 idle-stop)。
// 用法: node keepalive-node.mjs
import http from "node:http";

const KEEPER = { host: "127.0.0.1", port: 8798 };
const BRIDGE = { host: "127.0.0.1", port: 8799 };
const CDP = { host: "127.0.0.1", port: 9222 };
const MODELS = ["gemini-3-flash-chat", "claude-sonnet-5-chat"];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 保活问候池:每次随机一条,避免固定话术。都要求极短回复,省配额。
const GREETINGS = [
  "例行检查,请只回复 ok",
  "ping, please reply with just \"ok\"",
  "keepalive check, answer in one word",
  "你好,请回复两个字:在线",
  "quick status ping, respond with \"ok\" only",
  "例行保活,一句话回复即可",
  "hello, just say hi back",
  "连线确认,请回复 ok",
  "daily health check, reply ok",
  "在吗?回个 ok 就行",
  "sync check, one-word answer please",
  "例行心跳,请回复:正常",
];
const pickGreeting = () => GREETINGS[Math.floor(Math.random() * GREETINGS.length)];

function req(options, body) {
  return new Promise((resolve, reject) => {
    const r = http.request(options, (res) => {
      let d = "";
      res.on("data", (c) => (d += c));
      res.on("end", () => resolve({ status: res.statusCode, body: d }));
    });
    r.on("error", reject);
    if (body) r.write(body);
    r.end();
  });
}

async function main() {
  console.log("[keepalive]", new Date().toISOString(), "start");
  // 1. 唤醒
  try {
    const w = await req({ ...KEEPER, path: "/wake", method: "POST" }, null);
    console.log("[keepalive] wake:", w.status, w.body.slice(0, 100));
  } catch (e) {
    console.log("[keepalive] wake failed (keeper down?):", e.message);
    process.exitCode = 1;
    return;
  }
  // 2. 等桥就绪
  let ok = false;
  for (let i = 0; i < 40; i++) {
    try {
      const h = await req({ ...BRIDGE, path: "/health", method: "GET" }, null);
      if (h.status === 200 && h.body.includes('"ok":true')) { ok = true; break; }
    } catch {}
    await sleep(2000);
  }
  if (!ok) { console.log("[keepalive] bridge not ready"); process.exitCode = 1; return; }
  console.log("[keepalive] bridge ready");
  // 3. 各 provider 发消息(活动=续期)
  const health = JSON.parse((await req({ ...BRIDGE, path: "/health", method: "GET" }, null)).body);
  let anyFail = false;
  for (const m of MODELS) {
    const prov = m.startsWith("gemini-") ? "gemini" : "claude";
    const t = health.providers && health.providers[prov] && health.providers[prov].tokens;
    const hasToken = t && (t.at || t.orgId);
    if (!hasToken) { console.log("[keepalive]", m, "skip (no tokens)"); continue; }
    const body = JSON.stringify({ model: m, messages: [{ role: "user", content: pickGreeting() }], stream: false });
    try {
      const r = await req({ ...BRIDGE, path: "/v1/chat/completions", method: "POST", headers: { "Content-Type": "application/json" } }, body);
      const bad = r.body.includes('"error"');
      if (bad) anyFail = true;
      console.log("[keepalive]", m, bad ? "FAILED: " + r.body.slice(0, 150) : "OK");
    } catch (e) {
      anyFail = true;
      console.log("[keepalive]", m, "ERR:", e.message);
    }
    await sleep(3000);
  }
  // 4. 优雅关闭 Chrome
  await sleep(5000);
  try {
    const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);
    const targets = await new Promise((resolve, reject) => {
      http.get({ host: CDP.host, port: CDP.port, path: "/json" }, (res) => {
        let d = "";
        res.on("data", (c) => (d += c));
        res.on("end", () => { try { resolve(JSON.parse(d)); } catch (e) { reject(e); } });
      }).on("error", reject);
    });
    const page = targets.find((t) => t.type === "page");
    if (page) {
      const c = await cdp(page.webSocketDebuggerUrl);
      await c.cmd("Browser.close");
      console.log("[keepalive] Browser.close sent");
      c.close();
    }
  } catch (e) {
    console.log("[keepalive] close err:", e.message);
  }
  if (anyFail) process.exitCode = 1;
  console.log("[keepalive] done");
}

main();
