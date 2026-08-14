#!/usr/bin/env node
// aurora CDP 桥 —— 在真实浏览器页面上下文里执行网页逆向请求,暴露 OpenAI 兼容 API。
//
// 定位:Gemini(google.com 反爬最强)这类硬风控通道的本地执行器。
// 原理:请求从已登录真实页面的 JS 上下文用 fetch() 发出 —— cookie、TLS、指纹、
//       JS 运行时全部由浏览器自带(零模拟),流式响应经 console 逐块回传本进程,
//       再转成 OpenAI SSE 发给客户端。
//
// 特性:
//   - 零依赖(node 内置模块 + scripts/cdp/cdp-helper.mjs)
//   - 令牌自续:每次页内 fetch 都同时挂 Network 监听,抓自己的 StreamGenerate
//     请求刷新会话令牌(会话级令牌无需人工重抓,浏览器保持登录即可)
//   - 严格限频(单通道串行 + >=2.1s 间隔,对齐 docs/GEMINI.md 防封号要求)
//   - 多适配器架构:provider 以 model 前缀路由,加新模型只需新增适配器对象
//     (当前实现:gemini;预留 qianwen 等)
//
// 端点:
//   GET  /health              状态(浏览器连接/令牌/账号)
//   GET  /v1/models           模型目录
//   POST /v1/chat/completions OpenAI 兼容(stream 与非流式)
//
// 环境变量:
//   BRIDGE_PORT        监听端口(默认 8799)
//   BRIDGE_HOST        监听地址(默认 127.0.0.1;NAS 转发场景设 0.0.0.0)
//   BRIDGE_AUTH        可选鉴权 token;设置后请求须带 Authorization: Bearer <token>
//                      (局域网开放时建议设置,与 aurora 的 GEMINI_CDP_KEY 一致)
//   CDP_PORT           浏览器调试端口(默认 9222)
//   IDLE_TIMEOUT_MIN   无对话活动自动停止分钟数(默认 30;0=关闭)。停止=经 CDP
//                      Browser.close 优雅关闭整个 Chrome,再退出桥进程
//                     (/health、/v1/models 不视为活动,防监控探针阻止休眠)
//   MIN_INTERVAL_MS    限频基础间隔毫秒(默认 2000)
//   JITTER_MS          限频随机抖动上限毫秒(默认 1500;实际间隔 = 基础 + rand(0..抖动))
//
// 用法: node scripts/cdp/bridge.mjs
// 会话令牌缓存: .runtime/bridge/gemini_session.json(gitignore 已排除)
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);

const PORT = parseInt(process.env.BRIDGE_PORT || "8799", 10);
const HOST = process.env.BRIDGE_HOST || "127.0.0.1"; // 0.0.0.0 = 局域网可达(供 NAS 转发)
const AUTH = process.env.BRIDGE_AUTH || "";
const CDP_HOST = "127.0.0.1";
const CDP_PORT = parseInt(process.env.CDP_PORT || "9222", 10);
// Gemini 限频(防封号):串行 + 每请求间隔 = 基础 2s + 随机抖动 0~1.5s(更像真人节奏)。
const MIN_INTERVAL_MS = parseInt(process.env.MIN_INTERVAL_MS || "2000", 10);
const JITTER_MS = parseInt(process.env.JITTER_MS || "1500", 10);
// 无活动自动停止:仅统计对话请求(/health 与 /v1/models 不算活动,防监控探针续命)。
// 0 关闭自动停止。
const IDLE_TIMEOUT_MS = (parseInt(process.env.IDLE_TIMEOUT_MIN || "30", 10) || 0) * 60 * 1000;
let lastActivity = Date.now();

const ROOT = path.resolve(import.meta.dirname, "../..");
const STATE_DIR = path.join(ROOT, ".runtime", "bridge");
const STATE_FILE = path.join(STATE_DIR, "gemini_session.json");

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ─── 会话状态(令牌缓存) ──────────────────────────────────────────
let state = {
  provider: "gemini",
  account: null, // 登录邮箱(WIZ oPEP7c)
  at: null,      // window.WIZ_global_data.SNlM0e
  snlM6e: null,  // f.req 内层 [3] 大令牌(~2.6KB)
  fsid: null,    // URL f.sid
  bl: null,      // URL bl 构建号
  sessionUuid: null, // f.req 内层 [59]
  uuid4: null,   // f.req 内层 [4]
  headers: {},   // x-goog-ext-*-jspb 头(全部,随升级自动跟随)
  fullInner: null, // 97 字段 f.req 内层骨架(从真实请求抓取,保持最新)
  cid: null,
  rcid: null, // 上一轮回复 id(多轮续聊)
  updatedAt: null,
};

function loadState() {
  try {
    if (fs.existsSync(STATE_FILE)) {
      state = { ...state, ...JSON.parse(fs.readFileSync(STATE_FILE, "utf8")) };
      console.log("[state] loaded from", STATE_FILE);
      return true;
    }
  } catch (e) {
    console.error("[state] load failed:", e.message);
  }
  // 首次:从 capture/replay 脚本的临时缓存导入
  const p1 = path.join(os.tmpdir(), "gem_parsed.json");
  const p2 = path.join(os.tmpdir(), "gem_capture.json");
  if (fs.existsSync(p1) && fs.existsSync(p2)) {
    try {
      const parsed = JSON.parse(fs.readFileSync(p1, "utf8"));
      const raw = JSON.parse(fs.readFileSync(p2, "utf8"));
      const inner = parsed.inner;
      state = {
        ...state,
        at: parsed.summary.at,
        snlM6e: inner[3],
        fsid: raw.f_sid,
        bl: (raw.url.match(/bl=([^&]+)/) || [])[1] || null,
        sessionUuid: inner[59],
        uuid4: inner[4],
        headers: {
          "x-goog-ext-525005358-jspb": parsed.summary.h525005358,
          "x-goog-ext-525001261-jspb": parsed.summary.h525001261,
          "x-goog-ext-73010989-jspb": parsed.summary.h73010989,
          "x-goog-ext-73010990-jspb": parsed.summary.h73010990,
        },
        fullInner: inner,
        cid: Array.isArray(inner[2]) ? inner[2][0] : null,
        rcid: Array.isArray(inner[2]) ? inner[2][2] : null,
        updatedAt: new Date().toISOString(),
      };
      saveState();
      console.log("[state] imported from %TEMP% capture seeds");
      return true;
    } catch (e) {
      console.error("[state] seed import failed:", e.message);
    }
  }
  return false;
}

function saveState() {
  try {
    fs.mkdirSync(STATE_DIR, { recursive: true });
    fs.writeFileSync(STATE_FILE, JSON.stringify(state, null, 2));
  } catch (e) {
    console.error("[state] save failed:", e.message);
  }
}

// 从抓到的 StreamGenerate 请求刷新令牌(自续核心:抓自己的 fetch)
function applyCapture(req) {
  if (!req.postData) return;
  const params = new URLSearchParams(req.postData);
  const freq = params.get("f.req");
  const at = params.get("at");
  if (!freq) return;
  try {
    const outer = JSON.parse(freq);
    const inner = JSON.parse(outer[1]);
    state.at = at || state.at;
    state.snlM6e = inner[3];
    state.fsid = (req.url.match(/f\.sid=([^&]+)/) || [])[1] || state.fsid;
    state.bl = (req.url.match(/bl=([^&]+)/) || [])[1] || state.bl;
    state.fullInner = inner;
    state.cid = Array.isArray(inner[2]) ? inner[2][0] : state.cid;
    if (inner[59]) state.sessionUuid = inner[59];
    if (inner[4]) state.uuid4 = inner[4];
    // 所有 jspb 头(含未来新增)自动跟随
    for (const [k, v] of Object.entries(req.headers || {})) {
      if (/^x-goog-ext-.*-jspb$/i.test(k)) state.headers[k] = v;
    }
    state.updatedAt = new Date().toISOString();
    saveState();
    console.log("[state] tokens refreshed from live request");
  } catch (e) {
    console.error("[state] applyCapture failed:", e.message);
  }
}

// ─── CDP 连接管理 ────────────────────────────────────────────────
function getJSON(p) {
  return new Promise((res, rej) => {
    http
      .get({ host: CDP_HOST, port: CDP_PORT, path: p }, (r) => {
        let d = "";
        r.on("data", (c) => (d += c));
        r.on("end", () => {
          try { res(JSON.parse(d)); } catch (e) { rej(e); }
        });
      })
      .on("error", rej);
  });
}

function openTarget(url) {
  return new Promise((res, rej) => {
    const req = http.request(
      { host: CDP_HOST, port: CDP_PORT, path: "/json/new?" + encodeURIComponent(url), method: "PUT" },
      (r) => {
        let d = "";
        r.on("data", (c) => (d += c));
        r.on("end", () => {
          try { res(JSON.parse(d)); } catch (e) { rej(e); }
        });
      }
    );
    req.on("error", rej);
    req.end();
  });
}

let conn = null;
let currentCollector = null; // 进行中请求的 console chunk 收集器

async function findTarget() {
  const targets = await getJSON("/json");
  return targets.find((t) => t.type === "page" && t.url.includes("gemini.google.com")) || null;
}

async function ensureConn() {
  if (conn) return conn;
  let page = await findTarget();
  if (!page) {
    console.log("[cdp] gemini page not open, trying /json/new...");
    try { await openTarget("https://gemini.google.com/app"); } catch (e) {
      console.error("[cdp] openTarget failed:", e.message);
    }
    await sleep(3000);
    page = await findTarget();
  }
  if (!page) return null;
  const c = await cdp(page.webSocketDebuggerUrl);
  c.on((m) => {
    if (m.__closed) { conn = null; console.log("[cdp] connection closed"); return; }
    if (m.method === "Runtime.consoleAPICalled") {
      for (const a of m.params.args || []) {
        if (a.type === "string" && a.value && a.value.startsWith("__CHUNK__")) {
          if (currentCollector) currentCollector.buffer += a.value.slice(9);
        }
      }
    }
    if (m.method === "Network.requestWillBeSent" && m.params.request) {
      if ((m.params.request.url || "").indexOf("StreamGenerate") !== -1) {
        applyCapture(m.params.request);
      }
    }
  });
  await c.cmd("Runtime.enable");
  await c.cmd("Network.enable");
  // 顺手读登录邮箱
  try {
    const r = await c.cmd("Runtime.evaluate", {
      expression: '(window.WIZ_global_data && window.WIZ_global_data.oPEP7c) ? window.WIZ_global_data.oPEP7c : ""',
      returnByValue: true,
    });
    const v = r && r.result && r.result.result && r.result.result.value;
    if (v) { state.account = v; saveState(); }
  } catch {}
  conn = c;
  console.log("[cdp] connected to", page.url.slice(0, 60));
  return c;
}

// ─── Gemini 适配器 ───────────────────────────────────────────────
const gemini = {
  prefix: "gemini-",
  models: [
    { id: "gemini-3-flash-chat", object: "model", owned_by: "google", capabilities: ["web_search", "reasoning", "vision"] },
  ],

  flatten(messages) {
    const parts = [];
    for (const m of messages || []) {
      let content = m.content;
      if (Array.isArray(content)) {
        content = content.map((c) => (c && c.text ? c.text : "")).filter(Boolean).join("\n");
      }
      if (typeof content !== "string" || !content.trim()) continue;
      if (m.role === "assistant") parts.push("Gemini：" + content);
      else if (m.role === "system") parts.push("背景说明：" + content);
      else parts.push("用户：" + content);
    }
    return parts.join("\n");
  },

  buildRequest(prompt) {
    const inner = JSON.parse(JSON.stringify(state.fullInner));
    inner[0] = [prompt, 0, null, null, null, null, 0];
    const rid = "r_" + crypto.randomBytes(8).toString("hex");
    inner[2][1] = rid;
    if (state.rcid) inner[2][2] = state.rcid;
    const fReq = JSON.stringify([null, JSON.stringify(inner)]);
    const body = "f.req=" + encodeURIComponent(fReq) + "&at=" + encodeURIComponent(state.at);
    const url =
      "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate" +
      "?bl=" + state.bl +
      "&f.sid=" + state.fsid +
      "&hl=zh-CN" +
      "&_reqid=" + Math.floor(Math.random() * 1e6) +
      "&rt=c";
    const headers = {
      "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
      "X-Same-Domain": "1",
      ...state.headers,
    };
    return { url, headers, body };
  },

  // 增量 RPC 帧解析器(对齐 internal/geminweb/client.go 的 parseRPCFrame)
  createParser(onDelta) {
    let buf = "";
    let lastText = "";
    const self = {
      done: false,
      text: "",
      feed(chunk) {
        buf += chunk;
        let idx;
        while ((idx = buf.indexOf("\n")) !== -1) {
          const line = buf.slice(0, idx).trim();
          buf = buf.slice(idx + 1);
          if (line.startsWith("[[")) self.parseLine(line);
        }
      },
      flush() {
        if (buf.trim().startsWith("[[")) self.parseLine(buf.trim());
      },
      parseLine(line) {
        let frames;
        try { frames = JSON.parse(line); } catch { return; }
        for (const fr of frames) {
          if (!Array.isArray(fr) || fr.length < 3 || fr[0] !== "wrb.fr") continue;
          let payload;
          try { payload = JSON.parse(fr[2]); } catch { continue; }
          if (!Array.isArray(payload) || payload.length < 3) continue;
          if (payload[2] && typeof payload[2] === "object" && payload[2]["44"] === true) {
            self.done = true;
          }
          if (Array.isArray(payload[4])) {
            for (const p of payload[4]) {
              // 本轮回复的 rc_id 在文本帧的 [0](格式 "rc_..."),不是 data[1][1]
              // (后者是请求 rid 的回显)。rcid 用于多轮续聊,格式错会报 BardErrorInfo 1157。
              if (Array.isArray(p) && typeof p[0] === "string" && p[0].startsWith("rc_")) {
                state.rcid = p[0];
                saveState();
              }
              if (Array.isArray(p) && Array.isArray(p[1]) && p[1].length > 0 && typeof p[1][0] === "string") {
                let text = p[1][0];
                // 剥离 card_content 引用占位符(同 geminweb sanitizeText)
                const lines = text.split("\n").filter(
                  (l) => !/^https?:\/\/googleusercontent\.com\/card_content\//.test(l.trim())
                );
                text = lines.join("\n").replace(/https?:\/\/googleusercontent\.com\/card_content\//g, "");
                if (text.length < lastText.length || !text.startsWith(lastText)) {
                  lastText = text;
                  self.text = text;
                  if (onDelta) onDelta(text);
                  continue;
                }
                const delta = text.slice(lastText.length);
                lastText = text;
                self.text = text;
                if (delta && onDelta) onDelta(delta);
              }
            }
          }
        }
      },
    };
    return self;
  },
};

// 适配器注册表:model 前缀 → 适配器(加新模型只需在这里登记)
const adapters = [gemini];

function resolveAdapter(model) {
  return adapters.find((a) => model.startsWith(a.prefix)) || null;
}

// ─── 限频队列(串行 + >=2.1s 间隔) ────────────────────────────────
const queue = [];
let processing = false;
let lastReqAt = 0;

function enqueue(fn) {
  return new Promise((resolve, reject) => {
    queue.push({ fn, resolve, reject });
    pump();
  });
}

async function pump() {
  if (processing || queue.length === 0) return;
  processing = true;
  const job = queue.shift();
  try {
    // 间隔 = 基础 + 随机抖动(0..JITTER),比固定间隔更接近真人使用节奏
    const target = MIN_INTERVAL_MS + Math.floor(Math.random() * (JITTER_MS + 1));
    const wait = target - (Date.now() - lastReqAt);
    if (wait > 0) await sleep(wait);
    job.resolve(await job.fn());
    lastReqAt = Date.now();
  } catch (e) {
    job.reject(e);
  }
  processing = false;
  pump();
}

// ─── 执行层:页内 fetch → 流式回传 ────────────────────────────────
async function execute(prompt, onDelta) {
  const c = await ensureConn();
  if (!c) {
    const e = new Error("gemini 页面未打开(请先启动 CDP 浏览器并登录 gemini.google.com)");
    e.code = "no_browser";
    throw e;
  }
  if (!state.at || !state.snlM6e || !state.fsid) {
    const e = new Error("会话令牌缺失(先跑 capture-streamgenerate.mjs 抓一次)");
    e.code = "no_tokens";
    throw e;
  }
  const { url, headers, body } = gemini.buildRequest(prompt);
  const parser = gemini.createParser(onDelta);
  const collector = { buffer: "" };
  currentCollector = collector;

  const pageJs =
    "(async () => {" +
    "try {" +
    "const resp = await fetch(" + JSON.stringify(url) + ", { method: 'POST', headers: " + JSON.stringify(headers) + ", body: " + JSON.stringify(body) + ", credentials: 'same-origin' });" +
    "if (!resp.ok) return 'HTTP ' + resp.status;" +
    "const reader = resp.body.getReader();" +
    "const dec = new TextDecoder();" +
    "let n = 0;" +
    "for (;;) { const r = await reader.read(); if (r.done) break; const t = dec.decode(r.value, { stream: true }); n += t.length; console.log('__CHUNK__' + t); }" +
    "return 'OK ' + n;" +
    "} catch (e) { return 'ERR ' + (e && e.message ? e.message : e); }" +
    "})()";

  let result;
  try {
    const r = await c.cmd("Runtime.evaluate", { expression: pageJs, awaitPromise: true, returnByValue: true });
    result = r && r.result && r.result.result && r.result.result.value;
  } finally {
    currentCollector = null;
  }
  parser.feed(collector.buffer);
  parser.flush();
  if (typeof result !== "string" || !result.startsWith("OK")) {
    const e = new Error("页内请求失败: " + result);
    e.code = "upstream_error";
    throw e;
  }
  // 令牌失效识别(BardErrorInfo 1096/1157 等):指引用户手动发一条消息即可自愈
  const errMatch = collector.buffer.match(/BardErrorInfo"?\s*,\s*\[?(\d+)/);
  if (!parser.text && errMatch) {
    const e = new Error(
      "会话令牌已失效(BardErrorInfo " + errMatch[1] + ")。" +
      "浏览器页面还开着:在页面上手动发任意一条消息,桥的 Network 监听会自动刷新令牌,然后重试即可;" +
      "浏览器重启过:先确认 gemini.google.com 仍是登录态,再手动发一条消息。"
    );
    e.code = "token_stale";
    throw e;
  }
  if (!parser.text) {
    const e = new Error("上游空回复(可能被限频或令牌失效), 原始响应头部: " + collector.buffer.slice(0, 200));
    e.code = "empty_reply";
    throw e;
  }
  return parser;
}

// ─── HTTP 服务 ───────────────────────────────────────────────────
const CORS = {
  "Access-Control-Allow-Origin": "*",
  "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
  "Access-Control-Allow-Headers": "Authorization, Content-Type",
};

function sendJSON(res, code, obj) {
  const body = JSON.stringify(obj);
  res.writeHead(code, { "Content-Type": "application/json; charset=utf-8", ...CORS });
  res.end(body);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    let d = "";
    let size = 0;
    req.on("data", (c) => {
      size += c.length;
      if (size > 1024 * 1024) { reject(new Error("body too large")); req.destroy(); return; }
      d += c;
    });
    req.on("end", () => resolve(d));
    req.on("error", reject);
  });
}

async function handleChat(res, body) {
  lastActivity = Date.now(); // 对话请求才算活动
  const model = body.model || "gemini-3-flash-chat";
  const adapter = resolveAdapter(model);
  if (!adapter) {
    sendJSON(res, 400, { error: { message: "unknown model: " + model, type: "invalid_request_error" } });
    return;
  }
  const prompt = adapter.flatten(body.messages);
  if (!prompt) {
    sendJSON(res, 400, { error: { message: "no message content", type: "invalid_request_error" } });
    return;
  }
  const stream = body.stream !== false;
  const id = "chatcmpl-" + crypto.randomBytes(6).toString("hex");
  const created = Math.floor(Date.now() / 1000);

  if (!stream) {
    try {
      const parser = await enqueue(() => execute(prompt, null));
      sendJSON(res, 200, {
        id, object: "chat.completion", created, model,
        choices: [{ index: 0, message: { role: "assistant", content: parser.text }, finish_reason: parser.done ? "stop" : "length" }],
        usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
      });
    } catch (e) {
      sendJSON(res, e.code === "no_browser" ? 503 : 502, { error: { message: e.message, type: e.code || "upstream_error" } });
    }
    return;
  }

  res.writeHead(200, {
    "Content-Type": "text/event-stream; charset=utf-8",
    "Cache-Control": "no-cache",
    Connection: "keep-alive",
    ...CORS,
  });
  const sse = (obj) => {
    try { res.write("data: " + JSON.stringify(obj) + "\n\n"); } catch {}
  };
  try {
    await enqueue(() =>
      execute(prompt, (delta) => {
        sse({
          id, object: "chat.completion.chunk", created, model,
          choices: [{ index: 0, delta: { content: delta }, finish_reason: null }],
        });
      })
    );
    sse({ id, object: "chat.completion.chunk", created, model, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] });
    res.write("data: [DONE]\n\n");
  } catch (e) {
    sse({ error: { message: e.message, type: e.code || "upstream_error" } });
  }
  try { res.end(); } catch {}
}

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, "http://localhost");
  if (req.method === "OPTIONS") {
    res.writeHead(204, CORS);
    res.end();
    return;
  }
  if (AUTH && req.headers.authorization !== "Bearer " + AUTH) {
    sendJSON(res, 401, { error: { message: "unauthorized", type: "authentication_error" } });
    return;
  }
  if (u.pathname === "/health" && req.method === "GET") {
    sendJSON(res, 200, {
      ok: true,
      browser: conn ? { connected: true } : { connected: false },
      provider: {
        name: "gemini",
        account: state.account,
        tokens: {
          at: !!state.at,
          snlM6eLen: (state.snlM6e || "").length,
          fsid: !!state.fsid,
          updatedAt: state.updatedAt,
        },
      },
    });
    return;
  }
  if (u.pathname === "/v1/models" && req.method === "GET") {
    sendJSON(res, 200, { object: "list", data: adapters.flatMap((a) => a.models) });
    return;
  }
  if (u.pathname === "/v1/chat/completions" && req.method === "POST") {
    try {
      const raw = await readBody(req);
      const body = JSON.parse((raw || "{}").replace(/^\uFEFF/, ""));
      await handleChat(res, body);
    } catch (e) {
      sendJSON(res, 400, { error: { message: "bad request: " + e.message, type: "invalid_request_error" } });
    }
    return;
  }
  sendJSON(res, 404, { error: { message: "not found", type: "invalid_request_error" } });
});

process.on("SIGINT", () => {
  console.log("\n[bridge] shutting down");
  if (conn) try { conn.close(); } catch {}
  process.exit(0);
});

// ─── 启动 ────────────────────────────────────────────────────────
const hasTokens = loadState();
server.listen(PORT, HOST, async () => {
  console.log("[bridge] listening on http://" + HOST + ":" + PORT);
  console.log("[bridge] auth:", AUTH ? "enabled" : "disabled (localhost only)");
  console.log("[bridge] tokens:", hasTokens ? "loaded" : "MISSING (run capture-streamgenerate.mjs once)");
  console.log("[bridge] idle auto-stop:", IDLE_TIMEOUT_MS > 0 ? Math.round(IDLE_TIMEOUT_MS / 60000) + "min" : "disabled");
  console.log("[bridge] rate limit:", MIN_INTERVAL_MS + "ms + jitter 0-" + JITTER_MS + "ms");
  const c = await ensureConn().catch(() => null);
  console.log("[bridge] browser:", c ? "connected" : "NOT connected");
  console.log("[bridge] account:", state.account || "unknown");
});

// 无活动自动停止:超时后经 CDP 优雅关闭整个 Chrome,再退出桥进程。
// 配合 Chrome 的 --disable-background-mode(关窗即全退),两者同时下班。
if (IDLE_TIMEOUT_MS > 0) {
  setInterval(() => {
    if (processing || queue.length > 0) return;
    if (Date.now() - lastActivity < IDLE_TIMEOUT_MS) return;
    console.log("[idle] no chat activity for " + Math.round(IDLE_TIMEOUT_MS / 60000) + " min, stopping Chrome + bridge");
    (async () => {
      try { if (conn) await conn.cmd("Browser.close"); } catch {}
      try { if (conn) conn.close(); } catch {}
      setTimeout(() => process.exit(0), 500);
    })();
  }, 30000).unref();
}
