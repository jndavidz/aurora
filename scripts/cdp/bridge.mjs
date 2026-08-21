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
// 限频策略(用户拍板):chat 不限频(真人使用);coding 限频在 aurora 侧完成
// (GeminiCDP 等 provider 的 coding 入口带 2s+抖动)。桥默认 0 间隔、只串行。
// 若要让桥自身限频(如客户端直连桥的场景),再设 MIN_INTERVAL_MS。
const MIN_INTERVAL_MS = parseInt(process.env.MIN_INTERVAL_MS || "0", 10);
const JITTER_MS = parseInt(process.env.JITTER_MS || "0", 10);
// 无活动自动停止:仅统计对话请求(/health 与 /v1/models 不算活动,防监控探针续命)。
// 0 关闭自动停止。
const IDLE_TIMEOUT_MS = (parseInt(process.env.IDLE_TIMEOUT_MIN || "30", 10) || 0) * 60 * 1000;
// Claude 限额预警阈值(5h 窗口利用率):默认 0.8(>=80% 才在回复末尾附加提醒,不打扰日常);
// 设 0 = 每条回复都附用量小尾巴。
const CLAUDE_LIMIT_WARN = parseFloat(process.env.CLAUDE_LIMIT_WARN || "0.8");
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
    // 原子写:先写 .tmp 再 rename,避免读方(如诊断脚本)读到半截 JSON
    const tmp = STATE_FILE + ".tmp";
    fs.writeFileSync(tmp, JSON.stringify(state, null, 2));
    fs.renameSync(tmp, STATE_FILE);
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
    // cid 只在非空时更新:新会话首条消息发送时 cid 还是空的,别用它覆盖好 cid
    const newCid = Array.isArray(inner[2]) ? inner[2][0] : "";
    if (newCid) state.cid = newCid;
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

// 每 provider 一个 CDP 连接(页面 target 不同:gemini.google.com / claude.ai 等)。
// 每个连接有独立的 console chunk 收集器(进行中的请求)。
const conns = new Map(); // adapter.name -> { c, collector }

async function findTarget(match) {
  const targets = await getJSON("/json");
  return targets.find((t) => t.type === "page" && t.url.includes(match)) || null;
}

async function ensureConn(adapter) {
  const existing = conns.get(adapter.name);
  if (existing && existing.c) return existing;
  let page = await findTarget(adapter.pageMatch);
  if (!page) {
    console.log("[cdp][" + adapter.name + "] page not open, trying /json/new...");
    try { await openTarget(adapter.homeUrl); } catch (e) {
      console.error("[cdp][" + adapter.name + "] openTarget failed:", e.message);
    }
    await sleep(3000);
    page = await findTarget(adapter.pageMatch);
  }
  if (!page) return null;
  const entry = { c: null, collector: null };
  const c = await cdp(page.webSocketDebuggerUrl);
  c.on((m) => {
    if (m.__closed) { conns.delete(adapter.name); console.log("[cdp][" + adapter.name + "] connection closed"); return; }
    if (m.method === "Runtime.consoleAPICalled") {
      for (const a of m.params.args || []) {
        if (a.type === "string" && a.value && a.value.startsWith("__CHUNK__")) {
          if (entry.collector) entry.collector.buffer += a.value.slice(9);
        }
      }
    }
    if (m.method === "Network.requestWillBeSent" && m.params.request) {
      // 自续:抓 provider 自己的请求刷新令牌/模板(由各适配器 capture 钩子处理)
      if (adapter.capture) adapter.capture(m.params.request);
    }
  });
  await c.cmd("Runtime.enable");
  await c.cmd("Network.enable");
  if (adapter.onConnect) {
    try { await adapter.onConnect(c); } catch {}
  }
  entry.c = c;
  conns.set(adapter.name, entry);
  console.log("[cdp][" + adapter.name + "] connected to", page.url.slice(0, 60));
  return entry;
}

// ─── Gemini 适配器 ───────────────────────────────────────────────
const gemini = {
  name: "gemini",
  prefix: "gemini-",
  pageMatch: "gemini.google.com",
  homeUrl: "https://gemini.google.com/app",
  capture: (req) => applyCapture(req),
  ready: () => !!(state.at && state.snlM6e && state.fsid),
  onConnect: async (c) => {
    // 顺手读登录邮箱
    try {
      const r = await c.cmd("Runtime.evaluate", {
        expression: '(window.WIZ_global_data && window.WIZ_global_data.oPEP7c) ? window.WIZ_global_data.oPEP7c : ""',
        returnByValue: true,
      });
      const v = r && r.result && r.result.result && r.result.result.value;
      if (v) { state.account = v; saveState(); }
    } catch {}
  },
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
    // cid 空 = 新会话首轮(rcid 必须一起置空,否则服务端报 [13]/1157);
    // 响应帧会返回新 cid,解析器学会后下一轮自动续聊。
    inner[2][0] = state.cid || "";
    inner[2][2] = state.cid && state.rcid ? state.rcid : "";
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
          // 响应帧 data[1] = [cid, rid]:新会话首轮时服务端在此返回 cid,学会它
          // (否则 cid 一直是空的,后续请求报 [13])。
          if (Array.isArray(payload[1]) && typeof payload[1][0] === "string" && payload[1][0]) {
            state.cid = payload[1][0];
            saveState();
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

// ─── Claude 状态与适配器(claude.ai,2026-08-14 抓包) ───────────────
// 协议比 Gemini 简单:认证纯 cookie(无会话令牌折腾);发消息 =
// POST /api/organizations/{org}/chat_conversations/{convId}/completion
// 请求体为抓包模板(26 个前端内置工具),每轮只替换 prompt + turn_message_uuids,
// convId 每轮新生成(多轮上下文靠全量拍平 prompt,不依赖服务端会话)。
// 响应 SSE:content_block_delta(text_delta) 为正文增量,message_stop 结束。
const CLAUDE_STATE_FILE = path.join(STATE_DIR, "claude_session.json");

let claudeState = {
  provider: "claude",
  orgId: null,      // /api/organizations/{orgId}
  template: null,   // completion 请求体模板(完整 26 tools)
  headers: {},      // anthropic-* 客户端头(device-id/anonymous-id/sha/build)
  limits: null,     // 限额:{fiveHUtil, fiveHResetsAt}(来自 message_delta 的 windows.5h)
  updatedAt: null,
};

function loadClaudeState() {
  try {
    if (fs.existsSync(CLAUDE_STATE_FILE)) {
      claudeState = { ...claudeState, ...JSON.parse(fs.readFileSync(CLAUDE_STATE_FILE, "utf8")) };
      console.log("[state][claude] loaded from", CLAUDE_STATE_FILE);
      return true;
    }
  } catch (e) {
    console.error("[state][claude] load failed:", e.message);
  }
  // 首次:从 capture-claude.mjs 的临时缓存导入
  const p = path.join(os.tmpdir(), "claude_capture_all.json");
  if (fs.existsSync(p)) {
    try {
      const all = JSON.parse(fs.readFileSync(p, "utf8"));
      const cap = all.find((r) => r.url.includes("/completion") && r.postData);
      if (!cap) return false;
      const orgId = (cap.url.match(/organizations\/([0-9a-f-]+)/) || [])[1];
      const headers = {};
      for (const k of ["anthropic-device-id", "anthropic-anonymous-id", "anthropic-client-sha", "anthropic-client-build", "anthropic-client-platform", "anthropic-client-version"]) {
        if (cap.headers[k]) headers[k] = cap.headers[k];
      }
      claudeState = { ...claudeState, orgId, template: JSON.parse(cap.postData), headers, updatedAt: new Date().toISOString() };
      saveClaudeState();
      console.log("[state][claude] imported from %TEMP% capture");
      return true;
    } catch (e) {
      console.error("[state][claude] seed import failed:", e.message);
    }
  }
  return false;
}

function saveClaudeState() {
  try {
    fs.mkdirSync(STATE_DIR, { recursive: true });
    const tmp = CLAUDE_STATE_FILE + ".tmp";
    fs.writeFileSync(tmp, JSON.stringify(claudeState, null, 2));
    fs.renameSync(tmp, CLAUDE_STATE_FILE);
  } catch (e) {
    console.error("[state][claude] save failed:", e.message);
  }
}

// 从抓到的 completion 请求自续模板(抓自己的 fetch,保持协议最新)
function applyClaudeCapture(req) {
  if (!req.postData || !req.url.includes("/completion")) return;
  try {
    claudeState.template = JSON.parse(req.postData);
    claudeState.orgId = (req.url.match(/organizations\/([0-9a-f-]+)/) || [])[1] || claudeState.orgId;
    for (const k of ["anthropic-device-id", "anthropic-anonymous-id", "anthropic-client-sha", "anthropic-client-build", "anthropic-client-platform", "anthropic-client-version"]) {
      if (req.headers[k]) claudeState.headers[k] = req.headers[k];
    }
    claudeState.updatedAt = new Date().toISOString();
    saveClaudeState();
    console.log("[state][claude] template refreshed from live request");
  } catch (e) {
    console.error("[state][claude] applyClaudeCapture failed:", e.message);
  }
}

const claude = {
  name: "claude",
  prefix: "claude-",
  pageMatch: "claude.ai",
  homeUrl: "https://claude.ai/new",
  capture: (req) => applyClaudeCapture(req),
  ready: () => !!(claudeState.orgId && claudeState.template),
  models: [
    { id: "claude-sonnet-5-chat", object: "model", owned_by: "anthropic", capabilities: ["web_search", "reasoning", "vision"] },
  ],

  flatten(messages) {
    const parts = [];
    for (const m of messages || []) {
      let content = m.content;
      if (Array.isArray(content)) {
        content = content.map((c) => (c && c.text ? c.text : "")).filter(Boolean).join("\n");
      }
      if (typeof content !== "string" || !content.trim()) continue;
      if (m.role === "assistant") parts.push("Claude：" + content);
      else if (m.role === "system") parts.push("背景说明：" + content);
      else parts.push("用户：" + content);
    }
    return parts.join("\n");
  },

  buildRequest(prompt) {
    const body = JSON.parse(JSON.stringify(claudeState.template));
    body.prompt = prompt;
    body.turn_message_uuids = {
      human_message_uuid: crypto.randomUUID(),
      assistant_message_uuid: crypto.randomUUID(),
    };
    // 每轮新 convId:多轮上下文靠全量拍平 prompt,不依赖服务端会话历史
    const convId = crypto.randomUUID();
    const url = "https://claude.ai/api/organizations/" + claudeState.orgId + "/chat_conversations/" + convId + "/completion";
    const headers = {
      accept: "text/event-stream",
      "Content-Type": "application/json",
      ...claudeState.headers,
    };
    return { url, headers, body: JSON.stringify(body) };
  },

  // 增量 SSE 解析:content_block_delta(text_delta) 为正文,message_stop 结束;
  // message_delta 携带限额(windows.5h)信息,顺手解析存档 + 超阈值预警
  createParser(onDelta) {
    let buf = "";
    const self = {
      done: false,
      text: "",
      feed(chunk) {
        buf += chunk;
        let idx;
        while ((idx = buf.indexOf("\n")) !== -1) {
          const line = buf.slice(0, idx);
          buf = buf.slice(idx + 1);
          const t = line.trim();
          if (!t.startsWith("data:")) continue;
          const payload = t.slice(5).trim();
          if (payload === "[DONE]") { self.done = true; continue; }
          try {
            const j = JSON.parse(payload);
            if (j.type === "content_block_delta" && j.delta && j.delta.type === "text_delta") {
              self.text += j.delta.text;
              if (onDelta) onDelta(j.delta.text);
            }
            if (j.type === "message_limit" && j.message_limit) {
              // 限额在独立的 message_limit 事件里:windows.5h.{utilization,resets_at},
              // resolved.limit.percent 直接给当前用量百分数
              const w5 = j.message_limit.windows && j.message_limit.windows["5h"];
              if (w5 && typeof w5.utilization === "number") {
                claudeState.limits = {
                  fiveHUtil: w5.utilization,
                  fiveHResetsAt: w5.resets_at || 0,
                  fiveHPercent: (j.message_limit.resolved && j.message_limit.resolved.limit && j.message_limit.resolved.limit.percent) || null,
                };
                claudeState.updatedAt = new Date().toISOString();
                saveClaudeState();
                console.log("[claude] 5h limit:", Math.round(claudeState.limits.fiveHUtil * 100) + "% used");
              }
            }
            if (j.type === "message_stop") {
              self.done = true;
              // 限额预警:5 小时窗口用量 >= 阈值(默认 80%)时,在回复末尾附加提示
              if (claudeState.limits && claudeState.limits.fiveHUtil >= CLAUDE_LIMIT_WARN) {
                const warn =
                  "\n\n⚠️ Claude 5小时限额已用 " + Math.round(claudeState.limits.fiveHUtil * 100) +
                  "%,重置于 " + new Date(claudeState.limits.fiveHResetsAt * 1000).toTimeString().slice(0, 5) +
                  "(再过 " + Math.round(claudeState.limits.fiveHUtil * 100) + "% 将触发限制)";
                self.text += warn;
                if (onDelta) onDelta(warn);
              }
            }
          } catch {}
        }
      },
      flush() {
        if (buf.trim().startsWith("data:")) self.feed(buf.trim() + "\n");
      },
    };
    return self;
  },
};

// 适配器注册表:model 前缀 → 适配器(加新模型只需在这里登记)
const adapters = [gemini, claude];

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
async function execute(adapter, prompt, onDelta) {
  const entry = await ensureConn(adapter);
  if (!entry) {
    const e = new Error(adapter.name + " 页面未打开(请先启动 CDP 浏览器并登录 " + adapter.pageMatch + ")");
    e.code = "no_browser";
    throw e;
  }
  if (adapter.name === "gemini") {
    // Gemini UI 模式:页面自己发消息(JS 实时生成 at),不需要令牌模板
    return await executeGeminiUI(entry, prompt, onDelta);
  }
  if (!adapter.ready()) {
    const e = new Error(adapter.name + " 会话模板缺失(先跑 capture-" + adapter.name + ".mjs 抓一次)");
    e.code = "no_tokens";
    throw e;
  }

  const { url, headers, body } = adapter.buildRequest(prompt);
  const parser = adapter.createParser(onDelta);
  const collector = { buffer: "" };
  entry.collector = collector;

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
    const r = await entry.c.cmd("Runtime.evaluate", { expression: pageJs, awaitPromise: true, returnByValue: true });
    result = r && r.result && r.result.result && r.result.result.value;
  } finally {
    entry.collector = null;
  }
  parser.feed(collector.buffer);
  parser.flush();
  if (typeof result !== "string" || !result.startsWith("OK")) {
    const e = new Error("页内请求失败: " + result);
    e.code = "upstream_error";
    throw e;
  }
  if (adapter.name === "gemini") {
    // 会话损坏识别(gemini 专属):三种信号任意其一即判定 ——
    //   1. BardErrorInfo(NNNN)          —— 已知会话错误(1096/1157/1060 等)
    //   2. wrb.fr 错误帧 [...,[N]]       —— 服务端拒绝帧(实测错误码 [13],
    //      页面导航/刷新/崩溃后整套令牌(at/SNlM6e/f.sid)随页面实例轮换而失效)
    // 自愈(实测规律):令牌只在"页面实例切换"时轮换 —— 新会话首条消息、刷新、重启都会
    // 切换实例;而在**已有会话里续发消息**不会切换(URL 不变)。因此恢复只需:
    // 在浏览器当前会话里**再发一条消息**(若刚发过首条,再发第二条),桥的 Network
    // 监听自动捕获当前实例令牌,之后请求即恢复。注意:桥不能自动 reload ——
    // reload 会再次切换实例,反而破坏恢复路径。
    const errMatch = collector.buffer.match(/BardErrorInfo"?\s*,\s*\[?(\d+)/);
    const errFrame = collector.buffer.match(/wrb\.fr",null,null,null,null,\[(\d+)\]/);
    if (!parser.text && (errMatch || errFrame)) {
      const code = errMatch ? errMatch[1] : errFrame[1];
      const e = new Error(
        "会话令牌已失效(错误码 " + code + ")。自愈只需一步:" +
        "在浏览器当前会话里发任意一条消息(如\"你好\";若刚发过首条消息,请再发一条)," +
        "桥会自动捕获当前实例的令牌,然后重试即可。" +
        "(Chrome 窗口平时驻留后台,若看不到窗口,运行 show-gemini.ps1 或 POST http://127.0.0.1:8798/show 拉回屏幕)"
      );
      e.code = "token_stale";
      throw e;
    }
  }
  if (!parser.text) {
    const e = new Error("上游空回复(可能被限频或令牌失效), 原始响应头部: " + collector.buffer.slice(0, 1500));
    e.code = "empty_reply";
    throw e;
  }
  return parser;
}

// ─── Gemini UI 注入模式(2026-08-21)───────────────────────────────
// 背景:8/19 前端升级后 at 令牌(fsec 格式)变成一次性 —— 捕获-复用被服务端拒绝
// (1097),自造 at 也 400。绕过方案:不动 fetch,改让页面 UI 自己发消息
// (页面 JS 实时生成有效 at),桥监听 StreamGenerate 响应并解析文本。
// 代价:每次请求在页面产生一条可见对话;会话上下文由页面自行维护(多轮无需拍平)。
async function executeGeminiUI(entry, prompt, onDelta) {
  const c = entry.c;
  const parser = gemini.createParser(onDelta);

  // 1. 挂一次性响应收集器:等 StreamGenerate 的 loadingFinished → getResponseBody
  let resolveBody, rejectBody;
  const bodyP = new Promise((res, rej) => { resolveBody = res; rejectBody = rej; });
  let targetReqId = null;
  let timer = null;
  let captured = false;
  const onMsg = (m) => {
    try {
      if (m.method === "Network.responseReceived") {
        const url = (m.params.response || {}).url || "";
        if (url.includes("StreamGenerate")) {
          targetReqId = m.params.requestId;
          console.log("[gemini-ui] StreamGenerate responseReceived reqId=" + targetReqId);
        }
      }
      if (m.method === "Network.loadingFinished") {
        console.log("[gemini-ui] loadingFinished reqId=" + m.params.requestId + " target=" + targetReqId + " captured=" + captured);
      }
      if (m.method === "Network.loadingFinished" && m.params.requestId === targetReqId && !captured) {
        captured = true;
        clearTimeout(timer);
        c.off(onMsg);
        c.cmd("Network.getResponseBody", { requestId: targetReqId })
          .then((b) => resolveBody((b.result && b.result.body) || ""))
          .catch((e) => rejectBody(e));
      }
    } catch {}
  };
  c.on(onMsg);

  // 2. UI 输入并发送
  console.log("[gemini-ui] waiting input...");
  try {
    await geminiUIInput(c, prompt);
  } catch (e) {
    c.off(onMsg);
    console.log("[gemini-ui] input failed:", e.message);
    throw e;
  }
  console.log("[gemini-ui] sent, waiting response...");

  // 3. 等响应(超时 120s)
  let body;
  try {
    body = await Promise.race([
      bodyP,
      new Promise((_, rej) => { timer = setTimeout(() => rej(new Error("gemini UI 响应超时(120s)")), 120000); }),
    ]);
  } catch (e) {
    c.off(onMsg);
    e.code = "upstream_error";
    throw e;
  }
  clearTimeout(timer);

  // 4. 解析
  parser.feed(body);
  parser.flush();

  // 5. 错误识别(与 fetch 模式同规则)
  const errMatch = body.match(/BardErrorInfo"?\s*,\s*\[?(\d+)/);
  const errFrame = body.match(/wrb\.fr",null,null,null,null,\[(\d+)\]/);
  if (!parser.text && (errMatch || errFrame)) {
    const code = errMatch ? errMatch[1] : errFrame[1];
    const e = new Error("Gemini 会话错误(错误码 " + code + ")。请在浏览器 gemini 页面确认登录状态后重试。");
    e.code = "token_stale";
    throw e;
  }
  if (!parser.text) {
    const e = new Error("gemini 上游空回复: " + body.slice(0, 600));
    e.code = "empty_reply";
    throw e;
  }
  return parser;
}

// geminiUIInput:聚焦输入框 → 插入文本 → 点发送按钮(失败自动 reload 重试一次)
async function geminiUIInput(c, text) {
  for (let attempt = 0; attempt < 2; attempt++) {
    if (attempt > 0) {
      console.log("[gemini-ui] reload page and retry...");
      try { await c.cmd("Page.reload", { ignoreCache: true }); } catch {}
      await sleep(12000);
    }
    const ok = await geminiUIInputOnce(c, text);
    if (ok) return;
  }
  const e = new Error("gemini UI 发送失败(输入框/发送按钮异常)");
  e.code = "upstream_error";
  throw e;
}

async function geminiUIInputOnce(c, text) {
  // 等输入框可用(回答生成期间输入框不可用,轮询等待)
  let pos = null;
  for (let i = 0; i < 90; i++) {
    try {
      const r = await c.cmd("Runtime.evaluate", {
        expression: `(function(){
          const el = document.querySelector('.ql-editor') || document.querySelector('textarea') || document.querySelector('[contenteditable="true"]');
          if (!el) return 'NONE';
          const b = el.getBoundingClientRect();
          if (b.width === 0 || b.height === 0) return 'NONE';
          return JSON.stringify({ x: b.x + b.width / 2, y: b.y + b.height / 2 });
        })()`,
        returnByValue: true,
      });
      const v = r.result && r.result.result && r.result.result.value;
      if (v && v !== "NONE") { pos = JSON.parse(v); break; }
    } catch {}
    await sleep(1000);
  }
  if (!pos) { console.log("[gemini-ui] input not ready"); return false; }
  console.log("[gemini-ui] input ready at", pos.x, pos.y);

  // 若有"停止回答"按钮(页面卡在生成中),reload 更可靠(点击实测无效)
  const stop = await c.cmd("Runtime.evaluate", {
    expression: `(function(){
      const b = [...document.querySelectorAll('button')].find(x => /stop|停止/i.test(x.getAttribute('aria-label') || x.title || ''));
      return b ? 'YES' : 'NO';
    })()`,
    returnByValue: true,
  });
  if (stop.result && stop.result.result && stop.result.result.value === "YES") {
    console.log("[gemini-ui] page busy (stop btn), return false for reload");
    return false;
  }

  // 清空输入框残留(真实按键 Ctrl+A + Delete,避免 execCommand 破坏 Quill 状态)
  await c.cmd("Input.dispatchKeyEvent", { type: "keyDown", key: "a", code: "KeyA", windowsVirtualKeyCode: 65, modifiers: 2 });
  await c.cmd("Input.dispatchKeyEvent", { type: "keyUp", key: "a", code: "KeyA", windowsVirtualKeyCode: 65, modifiers: 2 });
  await c.cmd("Input.dispatchKeyEvent", { type: "keyDown", key: "Delete", code: "Delete", windowsVirtualKeyCode: 46 });
  await c.cmd("Input.dispatchKeyEvent", { type: "keyUp", key: "Delete", code: "Delete", windowsVirtualKeyCode: 46 });
  await sleep(400);

  await c.cmd("Input.dispatchMouseEvent", { type: "mousePressed", x: pos.x, y: pos.y, button: "left", clickCount: 1 });
  await c.cmd("Input.dispatchMouseEvent", { type: "mouseReleased", x: pos.x, y: pos.y, button: "left", clickCount: 1 });
  await sleep(400);
  await c.cmd("Input.insertText", { text });
  await sleep(500);

  // 验证输入生效
  const chk = await c.cmd("Runtime.evaluate", {
    expression: `(function(){ const el = document.querySelector('.ql-editor') || document.querySelector('textarea'); return (el && (el.innerText || el.value)) || ''; })()`,
    returnByValue: true,
  });
  const got = chk.result && chk.result.result && chk.result.result.value;
  console.log("[gemini-ui] input content:", JSON.stringify(got).slice(0, 60));
  if (!got || !got.trim()) { console.log("[gemini-ui] input empty"); return false; }

  // 点发送按钮(实测 CDP 鼠标事件对 Angular 无效,须用 JS click();页面回答中时按钮显示"停止回答",轮询等待)
  for (let i = 0; i < 20; i++) {
    const btn = await c.cmd("Runtime.evaluate", {
      expression: `(function(){
        const b = [...document.querySelectorAll('button')].find(x => /send|发送/i.test(x.getAttribute('aria-label') || x.title || ''));
        if (!b || b.disabled || b.getAttribute('aria-disabled') === 'true') return 'NONE';
        b.click();
        return 'CLICKED';
      })()`,
      returnByValue: true,
    });
    const bv = btn.result && btn.result.result && btn.result.result.value;
    if (bv === "CLICKED") {
      console.log("[gemini-ui] clicked send (js click)");
      return true;
    }
    await sleep(1000);
  }
  console.log("[gemini-ui] send btn not found");
  return false;
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
  // Gemini UI 模式:页面自维护会话,只发最后一条用户消息(不能拍平历史,否则与页面上下文重复)
  const prompt = adapter.name === "gemini"
    ? (() => {
        const last = [...(body.messages || [])].reverse().find((m) => m.role === "user");
        const c = last && last.content;
        return Array.isArray(c) ? c.map((x) => (x && x.text ? x.text : "")).filter(Boolean).join("\n") : (typeof c === "string" ? c : "");
      })()
    : adapter.flatten(body.messages);
  if (!prompt) {
    sendJSON(res, 400, { error: { message: "no message content", type: "invalid_request_error" } });
    return;
  }
  const stream = body.stream !== false;
  const id = "chatcmpl-" + crypto.randomBytes(6).toString("hex");
  const created = Math.floor(Date.now() / 1000);

  if (!stream) {
    try {
      const parser = await enqueue(() => execute(adapter, prompt, null));
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
      execute(adapter, prompt, (delta) => {
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
    const providers = {};
    providers.gemini = {
      account: state.account,
      tokens: { at: !!state.at, snlM6eLen: (state.snlM6e || "").length, fsid: !!state.fsid, updatedAt: state.updatedAt },
      connected: !!conns.get("gemini"),
    };
    providers.claude = {
      tokens: { orgId: !!claudeState.orgId, template: !!claudeState.template, updatedAt: claudeState.updatedAt },
      limits: claudeState.limits,
      connected: !!conns.get("claude"),
    };
    sendJSON(res, 200, { ok: true, providers });
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
  for (const entry of conns.values()) {
    try { entry.c.close(); } catch {}
  }
  process.exit(0);
});

// ─── 启动 ────────────────────────────────────────────────────────
const hasTokens = loadState();
const hasClaude = loadClaudeState();
server.listen(PORT, HOST, async () => {
  console.log("[bridge] listening on http://" + HOST + ":" + PORT);
  console.log("[bridge] auth:", AUTH ? "enabled" : "disabled (localhost only)");
  console.log("[bridge] tokens:", hasTokens ? "loaded" : "MISSING (run capture-streamgenerate.mjs once)");
  console.log("[bridge] claude:", hasClaude ? "loaded" : "MISSING (run capture-claude.mjs once)");
  console.log("[bridge] idle auto-stop:", IDLE_TIMEOUT_MS > 0 ? Math.round(IDLE_TIMEOUT_MS / 60000) + "min" : "disabled");
  console.log("[bridge] rate limit:", MIN_INTERVAL_MS > 0 ? (MIN_INTERVAL_MS + "ms + jitter 0-" + JITTER_MS + "ms") : "disabled (chat free; coding limited by aurora)");
  const e1 = await ensureConn(gemini).catch(() => null);
  console.log("[bridge] gemini browser:", e1 ? "connected" : "NOT connected");
  if (e1) console.log("[bridge] gemini account:", state.account || "unknown");
});

// 无活动自动停止:超时后经 CDP 优雅关闭整个 Chrome,再退出桥进程。
// 配合 Chrome 的 --disable-background-mode(关窗即全退),两者同时下班。
if (IDLE_TIMEOUT_MS > 0) {
  setInterval(() => {
    if (processing || queue.length > 0) return;
    if (Date.now() - lastActivity < IDLE_TIMEOUT_MS) return;
    console.log("[idle] no chat activity for " + Math.round(IDLE_TIMEOUT_MS / 60000) + " min, stopping Chrome + bridge");
    (async () => {
      const anyConn = [...conns.values()][0];
      try { if (anyConn) await anyConn.c.cmd("Browser.close"); } catch {}
      for (const entry of conns.values()) {
        try { entry.c.close(); } catch {}
      }
      setTimeout(() => process.exit(0), 500);
    })();
  }, 30000).unref();
}
