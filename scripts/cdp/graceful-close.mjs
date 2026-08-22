// scripts/cdp/graceful-close.mjs — 优雅关闭 keeper Chrome(CDP Browser.close)
//
// 铁律:禁止强杀 Chrome(会导致"恢复异常关闭"横幅 → gemini 自动化失效)。
// 本脚本走 CDP Browser.close(与 keeper idle 自动停止一致),从 WSL2 也可调用:
//   /mnt/d/PortableApps/_sys/node/node.exe /mnt/d/repos/aurora/scripts/cdp/graceful-close.mjs
// 关闭后 keeper 的下一次 /wake 会重新拉起 Chrome。
import http from "node:http";
const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);

const getJSON = (p) => new Promise((res, rej) => {
  http.get({ host: "127.0.0.1", port: 9222, path: p }, (r) => {
    let d = "";
    r.on("data", (c) => (d += c));
    r.on("end", () => { try { res(JSON.parse(d)); } catch (e) { rej(e); } });
  }).on("error", rej);
});

try {
  const ver = await getJSON("/json/version");
  const c = await cdp(ver.webSocketDebuggerUrl);
  await c.cmd("Browser.close");
  console.log("Browser.close 已发送(优雅关闭)");
  c.close();
} catch (e) {
  console.log("close err:", e.message, "(Chrome 可能未运行)");
}
