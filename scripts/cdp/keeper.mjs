#!/usr/bin/env node
// scripts/cdp/keeper.mjs — Gemini CDP 唤醒守护(常驻,资源极小 ~20MB)。
//
// 定位:aurora(NAS)收到 gemini 请求而 PC 桥不在线时,POST /wake 到此守护,
// 守护拉起 Chrome + 桥并等桥就绪后返回;aurora 再重试转发 —— 全自动,无需手动脚本。
// 配合桥的 30 分钟无活动自动停止,形成"用则醒、闲则眠"的闭环。
//
// 实测前提:Chrome 完全关闭再重启后,磁盘里缓存的会话令牌仍有效
// (Chrome 会恢复原标签页,Google 服务端会话不因浏览器重启失效),
// 因此唤醒后第一个请求即可直接成功,无需任何人工发消息。
//
// 端点:
//   GET  /health   守护自身状态 + 桥/Chrome 是否在线
//   POST /wake     唤醒(幂等;已在线的立即返回)
//
// 环境变量:
//   KEEPER_PORT        监听端口(默认 8798;aurora 的 GEMINI_CDP_WAKE_PORT 对应)
//   KEEPER_TOKEN       可选鉴权;设置后须带 Authorization: Bearer <token>
//   KEEPER_CHROME      Chrome for Testing 路径(默认家庭 PC 路径,办公室按需覆盖)
//   KEEPER_PROFILE     独立 profile 路径
//   KEEPER_BRIDGE      桥脚本路径
//   KEEPER_MAX_WAIT_MS 拉起后最长等待毫秒(默认 60000)
//
// 开机自启(无需管理员,Windows):
//   schtasks /Create /F /TN "aurora-cdp-keeper" /SC ONLOGON /RL LIMITED /TR "\"<node.exe>\" \"<keeper.mjs>\""
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { spawn } from "node:child_process";

const KEEPER_PORT = parseInt(process.env.KEEPER_PORT || "8798", 10);
const KEEPER_TOKEN = process.env.KEEPER_TOKEN || "";
const CHROME = process.env.KEEPER_CHROME || "D:\\PortableApps\\_net\\Chrome for Testing\\chrome.exe";
const PROFILE = process.env.KEEPER_PROFILE || "D:\\PortableApps\\_net\\chrome-cdp\\profile";
const BRIDGE = process.env.KEEPER_BRIDGE || "D:\\repos\\aurora\\scripts\\cdp\\bridge.mjs";
const CDP_PORT = parseInt(process.env.CDP_PORT || "9222", 10);
const BRIDGE_PORT = parseInt(process.env.BRIDGE_PORT || "8799", 10);
const MAX_WAIT_MS = parseInt(process.env.KEEPER_MAX_WAIT_MS || "60000", 10);
const LOG_FILE = process.env.KEEPER_LOG || path.join(path.dirname(new URL(import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, "$1")), "../../.runtime/bridge/keeper.log");

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function log(msg) {
  const line = "[" + new Date().toISOString() + "] [keeper] " + msg;
  console.log(line);
  try {
    fs.mkdirSync(path.dirname(LOG_FILE), { recursive: true });
    fs.appendFileSync(LOG_FILE, line + "\n");
  } catch {}
}

function probe(port, pathName) {
  return new Promise((resolve) => {
    const req = http.request(
      { host: "127.0.0.1", port, path: pathName, method: "GET", timeout: 1500 },
      (res) => {
        res.resume();
        resolve(res.statusCode === 200);
      }
    );
    req.on("error", () => resolve(false));
    req.on("timeout", () => {
      req.destroy();
      resolve(false);
    });
    req.end();
  });
}
const chromeUp = () => probe(CDP_PORT, "/json/version");
const bridgeUp = () => probe(BRIDGE_PORT, "/health");

function spawnDetached(exe, args, env) {
  const logFd = fs.openSync(LOG_FILE, "a");
  const child = spawn(exe, args, { detached: true, stdio: ["ignore", logFd, logFd], env: env || process.env });
  child.unref();
  fs.closeSync(logFd);
}

const chromeArgs = [
  "--remote-debugging-port=" + CDP_PORT,
  "--user-data-dir=" + PROFILE,
  "--disable-extensions", "--disable-sync", "--disable-background-networking",
  "--disable-component-update", "--disable-default-apps", "--disable-notifications",
  "--no-first-run", "--no-default-browser-check", "--mute-audio",
  "--disable-background-mode", "--renderer-process-limit=4",
  "--disk-cache-size=104857600", "--disable-crash-reporter",
  "--noerrdialogs", "--disable-logging",
  // 异常关闭保底:自动恢复上次会话(保住 gemini 标签页与令牌),且不弹"要恢复页面吗"对话框。
  // 正常路径(桥的 30 分钟自动停止)走 CDP Browser.close 优雅关闭,会话/令牌本就不丢。
  "--restore-last-session", "--disable-session-crashed-bubble",
];

let waking = false;

async function wake() {
  while (waking) await sleep(300); // 单飞:并发唤醒合并为一次
  waking = true;
  try {
    if (await bridgeUp()) {
      log("wake: bridge already up");
      return { status: "already_up" };
    }
    if (!(await chromeUp())) {
      log("wake: starting Chrome...");
      spawnDetached(CHROME, chromeArgs);
      for (let i = 0; i < 40; i++) {
        if (await chromeUp()) break;
        await sleep(250);
      }
      if (!(await chromeUp())) log("wake: WARN Chrome CDP not ready");
    }
    if (!(await bridgeUp())) {
      log("wake: starting bridge...");
      spawnDetached(process.execPath, [BRIDGE], {
        ...process.env,
        BRIDGE_HOST: process.env.BRIDGE_HOST || "0.0.0.0",
      });
    }
    const t0 = Date.now();
    while (Date.now() - t0 < MAX_WAIT_MS) {
      if (await bridgeUp()) {
        log("wake: ready in " + (Date.now() - t0) + "ms");
        return { status: "ready", waitedMs: Date.now() - t0 };
      }
      await sleep(500);
    }
    log("wake: TIMEOUT after " + MAX_WAIT_MS + "ms");
    return { status: "timeout" };
  } finally {
    waking = false;
  }
}

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, "http://localhost");
  if (KEEPER_TOKEN && req.headers.authorization !== "Bearer " + KEEPER_TOKEN) {
    res.writeHead(401, { "Content-Type": "text/plain" });
    res.end("unauthorized");
    return;
  }
  if (u.pathname === "/health" && req.method === "GET") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ ok: true, bridgeUp: await bridgeUp(), chromeUp: await chromeUp() }));
    return;
  }
  if (u.pathname === "/wake" && req.method === "POST") {
    const r = await wake();
    res.writeHead(r.status === "timeout" ? 504 : 200, { "Content-Type": "application/json" });
    res.end(JSON.stringify(r));
    return;
  }
  res.writeHead(404, { "Content-Type": "text/plain" });
  res.end("not found");
});

server.listen(KEEPER_PORT, "0.0.0.0", () => {
  log("listening on 0.0.0.0:" + KEEPER_PORT + " (auth: " + (KEEPER_TOKEN ? "enabled" : "disabled") + ")");
});
