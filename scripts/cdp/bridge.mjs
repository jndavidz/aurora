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
// 调试日志开关。默认关闭:自续捕获的解析失败属预期噪音(页面上有大量非目标 RPC),
// 开着会淹没真正的错误。排查自续问题时用 BRIDGE_DEBUG=1 打开。
const DEBUG = process.env.BRIDGE_DEBUG === "1";
let lastActivity = Date.now();

// debugLog 只在 BRIDGE_DEBUG=1 时输出,用于降级"预期内的失败"。
function debugLog(...args) {
  if (DEBUG) console.error("[debug]", ...args);
}

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

// 从抓到的 StreamGenerate 请求刷新令牌(自续核心:抓自己的 fetch)。
// ⚠️ 必须做 URL 过滤:页面上除 StreamGenerate 外还有大量 Bard RPC,其中部分
// 也带 f.req 参数但结构与 StreamGenerate 不同 —— 不过滤就会对每个都抛解析
// 失败,产生无意义的 error 噪音(遗留待办 #5)。过滤后失败即代表真问题,
// 值得在 BRIDGE_DEBUG=1 下查看。与 applyClaudeCapture 的 /completion 过滤
// 保持同一写法。
function applyCapture(req) {
  if (!req.postData || !String(req.url || "").includes("StreamGenerate")) return;
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
    debugLog("[state] applyCapture failed:", e.message);
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
    // 与 applyCapture 同策略降级:自续失败不会静默 —— ready() 会因
    // template/orgId 缺失返回 false,请求阶段会正常报错,无需在此刷屏。
    debugLog("[state][claude] applyClaudeCapture failed:", e.message);
  }
}


// ─── 豆包适配器(2026-09-04)──────────────────────────────────────
// 页内 fetch 模式(同 hunyuan/claude):浏览器签名+会话由豆包前端维护,
// 桥只构造 body 并在页面上下文内发请求。会话策略:
//   - capture 钩子持续抓页面真实 completion 请求,同步最新会话状态
//     (conversation_id/section_id/last_message_index)——用户 VNC 聊天
//     推进会话时,桥的状态自动跟进(这是回声 bug 的解法:索引必须与
//     上游一致,否则上游判为重放,返回旧应答或静默忽略)
//   - 请求构造:续接当前会话(need_create=false)+全新 UUID 消息
//   - SSE 解析:patch_op 里 text_block.text 的 delta;事件流 2002=ACK/2003=REPLY_END
const DOUBAO_STATE_FILE = path.join(STATE_DIR, "doubao_session.json");
let doubaoState = {
  provider: "doubao",
  template: null,   // completion 请求体模板(来自页面真实请求)
  query: null,      // URL 查询参数串(aid/device_id/fp 等,含 msToken)
  convId: null,
  sectionId: null,
  lastMsgIdx: null,
  updatedAt: null,
};
function loadDoubaoState() {
  try {
    if (fs.existsSync(DOUBAO_STATE_FILE)) {
      doubaoState = { ...doubaoState, ...JSON.parse(fs.readFileSync(DOUBAO_STATE_FILE, "utf8")) };
      console.log("[state][doubao] loaded from", DOUBAO_STATE_FILE);
      return true;
    }
  } catch (e) { console.error("[state][doubao] load failed:", e.message); }
  return false;
}
function saveDoubaoState() {
  try {
    fs.mkdirSync(STATE_DIR, { recursive: true });
    const tmp = DOUBAO_STATE_FILE + ".tmp";
    fs.writeFileSync(tmp, JSON.stringify(doubaoState, null, 2));
    fs.renameSync(tmp, DOUBAO_STATE_FILE);
  } catch (e) { console.error("[state][doubao] save failed:", e.message); }
}
function applyDoubaoCapture(req) {
  if (!req.postData || !req.url.includes("/chat/completion")) return;
  try {
    const u = new URL(req.url);
    // a_bogus/msToken 每请求都变,不缓存;只固化稳定参数与模板
    const tmpl = JSON.parse(req.postData);
    doubaoState.template = req.postData;
    doubaoState.query = u.searchParams.toString();
    doubaoState.convId = tmpl.client_meta?.conversation_id || doubaoState.convId;
    doubaoState.sectionId = tmpl.client_meta?.last_section_id || doubaoState.sectionId;
    doubaoState.lastMsgIdx = typeof tmpl.client_meta?.last_message_index === "number" ? tmpl.client_meta.last_message_index : doubaoState.lastMsgIdx;
    doubaoState.updatedAt = new Date().toISOString();
    saveDoubaoState();
    console.log("[state][doubao] template refreshed (idx=" + doubaoState.lastMsgIdx + ")");
  } catch (e) { debugLog("[state][doubao] applyDoubaoCapture failed:", e.message); }
}
loadDoubaoState();

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

// ─── 腾讯元宝(混元)适配器(2026-08-22)──────────────────────────────────
// 背景:直连逆向(bogdanfinn TLS 指纹模拟)已风控 2 个账号 —— 本适配器用
// 真实浏览器**页内 fetch** 重放(同源自动带 cookie,无 TLS 指纹/签名问题,
// 风控暴露与真人操作一致)。流程:每次请求 create 会话 → chat 重放,
// 认证头(X-Uskey/X-HY93/X-device-id 等)会话级复用(从用户手动请求捕获一次,
// 存 STATE_DIR/yuanbao_headers.json;登录态过期需重登后重抓)。
const HUNYUAN_HEADERS_FILE = path.join(STATE_DIR, "yuanbao_headers.json");
let hunyuanCfg = null; // { headers: {...}, chatBody: {...} }
function loadHunyuanCfg() {
  try {
    if (fs.existsSync(HUNYUAN_HEADERS_FILE)) {
      hunyuanCfg = JSON.parse(fs.readFileSync(HUNYUAN_HEADERS_FILE, "utf8"));
      return true;
    }
  } catch (e) {
    console.error("[state][hunyuan] config load failed:", e.message);
  }
  return false;
}

const hunyuan = {
  name: "hunyuan",
  prefix: "hunyuan-",
  pageMatch: "yuanbao.tencent.com",
  homeUrl: "https://yuanbao.tencent.com/chat/naQivTmsDa",
  ready: () => !!(hunyuanCfg && hunyuanCfg.headers && hunyuanCfg.chatBody),
  models: [
    { id: "hunyuan-hy3-chat", object: "model", owned_by: "tencent", capabilities: ["web_search", "reasoning"] },
  ],

  flatten(messages) {
    const parts = [];
    for (const m of messages || []) {
      let content = m.content;
      if (Array.isArray(content)) {
        content = content.map((c) => (c && c.text ? c.text : "")).filter(Boolean).join("\n");
      }
      if (typeof content !== "string" || !content.trim()) continue;
      if (m.role === "assistant") parts.push("元宝：" + content);
      else if (m.role === "system") parts.push("背景说明：" + content);
      else parts.push("用户：" + content);
    }
    return parts.join("\n");
  },

  // SSE 解析:内容增量 {"type":"text","msg":"..."},meta 帧 endConv/stopReason 结束;
  // [citation:N] 引用标记(可能跨帧分片)由 cleaner 缓冲丢弃(2026-08-22)。
  createParser(onDelta) {
    let buf = "";
    let citBuf = ""; // 未闭合 citation 片段缓冲
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
            if (j.type === "text" && typeof j.msg === "string" && j.msg) {
              const clean = stripCitations(citBuf + j.msg, (rest) => { citBuf = rest; });
              if (clean) {
                self.text += clean;
                if (onDelta) onDelta(clean);
              }
            } else if (j.type === "meta" && (j.stopReason === "stop" || j.endConv)) {
              self.done = true;
            } else if (j.type === "error") {
              // 记录但不中断(如 21007 重试提示),由外层判断空回复
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

// stripCitations 过滤 [citation:N] / (citation:N) 引用标记(跨帧缓冲)。
// 返回可输出的干净文本;未闭合片段存回回调。
function stripCitations(input, saveRest) {
  let work = input;
  let out = "";
  for (;;) {
    const idx = work.indexOf("citation:");
    if (idx < 0) { out += work; saveRest(""); return out; }
    // 开括号([ 或 ()紧邻 citation: 前,一并吞掉
    let start = idx;
    while (start > 0 && (work[start - 1] === "[" || work[start - 1] === "(")) start--;
    out += work.slice(0, start);
    const rest = work.slice(idx + "citation:".length);
    const closeIdx = rest.search(/[)\]]/);
    if (closeIdx >= 0) {
      work = rest.slice(closeIdx + 1);
      continue;
    }
    saveRest(work.slice(start));
    return out;
  }
}

async function executeHunyuan(entry, prompt, onDelta) {
  if (!hunyuanCfg || !hunyuanCfg.headers || !hunyuanCfg.chatBody) {
    const e = new Error("hunyuan 会话头缺失(先运行 capture-yuanbao.mjs 抓一次)");
    e.code = "no_tokens";
    throw e;
  }
  const c = entry.c;
  const parser = hunyuan.createParser(onDelta);
  // 构造 chat body(模板替换 prompt/cid)
  const chatBody = JSON.parse(JSON.stringify(hunyuanCfg.chatBody));
  chatBody.prompt = prompt;
  chatBody.displayPrompt = prompt;
  // headers:X-AgentID 动态(agentId/cid)
  const headers = { ...hunyuanCfg.headers };
  const pageJs =
    "(async () => {" +
    "try {" +
    "const H = " + JSON.stringify(headers) + ";" +
    "const cr = await fetch('https://yuanbao.tencent.com/api/user/agent/conversation/create', { method: 'POST', headers: H, body: JSON.stringify({ agentId: 'naQivTmsDa' }), credentials: 'include' });" +
    "const cj = await cr.json();" +
    "const cid = cj && cj.id;" +
    "if (!cid) return 'ERR create: ' + JSON.stringify(cj).slice(0, 300);" +
    "H['X-AgentID'] = 'naQivTmsDa/' + cid;" +
    "const cb = " + JSON.stringify(chatBody) + ";" +
    "cb.conversationId = cid;" +
    "const r = await fetch('https://yuanbao.tencent.com/api/chat/' + cid, { method: 'POST', headers: H, body: JSON.stringify(cb), credentials: 'include' });" +
    "const txt = await r.text();" +
    "return 'OK ' + txt;" +
    "} catch (e) { return 'ERR ' + (e && e.message ? e.message : e); }" +
    "})()";
  const rr = await c.cmd("Runtime.evaluate", { expression: pageJs, awaitPromise: true, returnByValue: true });
  const result = rr && rr.result && rr.result.result && rr.result.result.value;
  if (typeof result !== "string" || !result.startsWith("OK")) {
    const e = new Error("hunyuan 页内请求失败: " + result);
    e.code = "upstream_error";
    throw e;
  }
  parser.feed(result.slice(3));
  parser.flush();
  if (!parser.text) {
    const e = new Error("hunyuan 上游空回复: " + result.slice(3, 400));
    e.code = "empty_reply";
    throw e;
  }
  return parser;
}

// ─── OpenAI ChatGPT 适配器(2026-09-02)──────────────────────────────
// 背景:ChatGPT 已改为「浏览器会话绑定」鉴权(Cloudflare + oai-did 设备指纹 +
// sentinel 反自动化 header),不再提供服务端可复用的 session/access token ——
// aurora 直连 backend-api 用的 token 文件会被 403;页内手造 fetch 缺
// OpenAI-Sentinel-* 等实时 header 也被 403 "Unusual activity"(2026-09-02 抓包实测)。
// 唯一稳的通道是 UI 驱动:让已登录页面自己发消息(页面 JS 实时生成全部 header),
// 桥注入文本 + 轮询 DOM 读回复(与 gemini UI 模式同思路)。

const chatgpt = {
  name: "chatgpt",
  // 特判 gpt-5-6 / gpt-5-6-mini(aurora 把这两个模型原样转给桥)
  prefix: "gpt-",
  pageMatch: "chatgpt.com",
  homeUrl: "https://chatgpt.com/",
  // 无 capture 钩子(UI 驱动模式:页面自己完成反自动化握手,无需令牌模板)
  capture: null,
  // UI 驱动模式无需模板文件:只要页面登录即可
  ready: () => true,

  models: [
    { id: "gpt-5-6", object: "model", owned_by: "openai", capabilities: ["web_search", "reasoning", "vision"] },
    { id: "gpt-5-6-mini", object: "model", owned_by: "openai", capabilities: ["web_search", "reasoning"] },
  ],

  flatten(messages) {
    const parts = [];
    for (const m of messages || []) {
      let content = m.content;
      if (Array.isArray(content)) {
        content = content.map((c) => (c && c.text ? c.text : "")).filter(Boolean).join("\n");
      }
      if (typeof content !== "string" || !content.trim()) continue;
      if (m.role === "assistant") parts.push("ChatGPT：" + content);
      else if (m.role === "system") parts.push("系统：" + content);
      else parts.push("用户：" + content);
    }
    return parts.join("\n");
  },

  // 设定期望模型(由 handleChat 在分发时按请求 model 设置;UI 模式下仅作记录,
  // 页面实际用其当前选中的模型)
  setModel(m) { this._model = m; },
};

// 适配器注册表:model 前缀 → 适配器(加新模型只需在这里登记)

const doubao = {
  name: "doubao",
  prefix: "doubao-",
  pageMatch: "doubao.com",
  homeUrl: "https://www.doubao.com/chat/",
  capture: (req) => applyDoubaoCapture(req),
  ready: () => !!(doubaoState.template && doubaoState.query),
  models: [
    { id: "doubao-chat", object: "model", owned_by: "bytedance", capabilities: ["reasoning", "vision"] },
  ],

  flatten(messages) {
    const parts = [];
    for (const m of messages || []) {
      let content = m.content;
      if (Array.isArray(content)) content = content.map(c => (c && c.text ? c.text : "")).filter(Boolean).join(" ");
      if (typeof content !== "string" || !content.trim()) continue;
      if (m.role === "assistant") parts.push("豆包：" + content);
      else if (m.role === "system") parts.push("背景：" + content);
      else parts.push("用户：" + content);
    }
    return parts.join("\\n");
  },

  buildRequest(prompt) {
    const body = JSON.parse(doubaoState.template);
    // 续接当前会话(索引与上游一致是关键——错位即重放)
    body.client_meta.conversation_id = doubaoState.convId || "";
    body.client_meta.last_section_id = doubaoState.sectionId || "";
    body.client_meta.last_message_index = typeof doubaoState.lastMsgIdx === "number" ? doubaoState.lastMsgIdx : 0;
    body.option.need_create_conversation = false;
    body.option.click_clear_context = false;
    body.option.create_time_ms = Date.now();
    body.option.unique_key = crypto.randomUUID();
    // 全新消息(新 UUID+新文本)
    body.messages = [{
      local_message_id: crypto.randomUUID(),
      content_block: [{ block_type: 10000, content: { text_block: { text: prompt, icon_url: "", icon_url_dark: "", summary: "" }, pc_event_block: "" }, block_id: crypto.randomUUID(), parent_id: "", meta_info: [], append_fields: [] }],
      message_status: 0,
    }];
    const q = new URLSearchParams(doubaoState.query);
    q.set("a_bogus", ""); // 页面 fetch hook 会实时注入真值
    const url = "https://www.doubao.com/chat/completion?" + q.toString();
    return { url, headers: { "content-type": "application/json", "agw-js-conv": "str" }, body: JSON.stringify(body) };
  },

  // SSE: 豆包流是 patch_op 形态,text_block.text 的 append delta;事件流前缀 "event: N"
  createParser(onDelta) {
    let buf = "";
    const self = { done: false, text: "" };
    self.feed = function(chunk) {
      buf += chunk;
      // 豆包 SSE 用空行分帧,data: 行携带 JSON
      let idx;
      while ((idx = buf.indexOf("\n")) !== -1) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        const t = line.trim();
        if (t.startsWith("data: ")) {
          const payload = t.slice(6).trim();
          if (payload === "{}" || !payload) continue;
          try {
            const j = JSON.parse(payload);
            // 响应文本在 patch_op 里: patch_value.ext=提示, 响应文本在 messages 或 content
            if (j.patch_op) for (const p of j.patch_op) {
              const pv = p.patch_value || {};
              if (pv.text_block && typeof pv.text_block.text === "string") {
                self.text += pv.text_block.text;
                if (onDelta) onDelta(pv.text_block.text);
              }
            }
            if (j.messages) for (const msg of j.messages) {
              for (const cb of msg.content_block || []) {
                const tb = cb.content && cb.content.text_block;
                if (tb && typeof tb.text === "string") { self.text += tb.text; if (onDelta) onDelta(tb.text); }
              }
            }
          } catch {}
        }
      }
    };
    self.flush = function() {};
    return self;
  },
};

const adapters = [gemini, claude, hunyuan, chatgpt, doubao];

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
  if (adapter.name === "hunyuan") {
    // 元宝:页内 fetch 重放(create 会话 + chat),认证头会话级复用
    return await executeHunyuan(entry, prompt, onDelta);
  }
  if (adapter.name === "chatgpt") {
    // ChatGPT UI 驱动模式:页面自己完成 sentinel/反自动化握手(实时 header),
    // 桥只注入文本 + 轮询 DOM 读回复(页内手造 fetch 缺 sentinel header 必 403)。
    // 自愈:旧对话可能静默失败(上下文污染/限频),一次重试时先开新对话。
    return await executeChatgptUI(entry, prompt, onDelta);
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

// ─── ChatGPT UI 驱动模式(2026-09-02)───────────────────────────────
// 背景:新版 ChatGPT 的 /backend-api/f/conversation 请求需带一整套实时生成的
// 反自动化 header(Authorization JWT + OpenAI-Sentinel-Chat-Requirements-Token +
// OpenAI-Sentinel-Turnstile-Token + OpenAI-Sentinel-Proof-Token + x-conduit-token
// 等,见 2026-09-02 抓包),页内手造 fetch 缺这些 header 必被 403
// "Unusual activity"。唯一稳的通道是让页面自己发消息(UI 驱动),页面 JS 实时
// 生成全部 header;桥只负责注入文本 + 轮询 DOM 读回复(实测可行)。
// 代价:每次请求在页面产生一条可见对话;会话上下文由页面自行维护(多轮无需拍平,
// prompt 只发最后一条用户消息,同 gemini UI 模式)。
const CHATGPT_ASSISTANT_SEL = '[data-message-author-role="assistant"]';
const CHATGPT_COMPOSER_SEL = 'textarea#prompt-textarea, [contenteditable="true"], div[role="textbox"]';

// chatgpt 页面 CDP 连接缓存:省去每请求的 /json 枚举 + WebSocket 握手(~100-350ms)。
// 仍是"专用独立连接"(每请求串行独占使用,非共享 entry.c),不违背事件时序隔离原则;
// 每次取用前 ping(3s 超时 —— Chrome 重启后旧 WS 会静默挂起,ping 无超时会堵死
// 串行 enqueue 队列,实测踩坑)+ 校验 URL 仍在 chatgpt.com,失效/被导航走则重建。
let chatgptConn = null;
function withTimeout(p, ms, tag) {
  let timer;
  return Promise.race([
    p,
    new Promise((_, rej) => { timer = setTimeout(() => rej(new Error(tag + " timeout " + ms + "ms")), ms); }),
  ]).finally(() => clearTimeout(timer));
}
async function getChatgptConn() {
  if (chatgptConn) {
    try {
      const u = await withTimeout(
        chatgptConn.ws.cmd("Runtime.evaluate", { expression: "location.href", returnByValue: true }),
        3000, "conn ping"
      );
      const href = (u && u.result && u.result.result && u.result.result.value) || "";
      if (typeof href === "string" && href.includes("chatgpt.com")) return chatgptConn.ws;
    } catch {}
    try { chatgptConn.ws.close(); } catch {}
    chatgptConn = null;
  }
  const page = await findTarget(chatgpt.pageMatch);
  if (!page) {
    const e = new Error("chatgpt 页面未打开(请先在 CDP 浏览器登录 chatgpt.com)");
    e.code = "no_browser";
    throw e;
  }
  const ws = await cdp(page.webSocketDebuggerUrl);
  // 焦点仿真:让页面始终认为自身有焦点,规避 Chrome 对后台 tab 的 timer/渲染节流
  // (实测空闲 20s+ 后首次 Runtime.evaluate 排队数秒,input 阶段被拖到 15s+)。
  await ws.cmd("Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {});
  chatgptConn = { ws };
  return ws;
}

async function executeChatgptUI(entry, prompt, onDelta) {
  // 用专用 CDP 连接执行 UI 操作:共享连接上的事件风暴/监听器会干扰 Input
  // 事件时序(同 gemini UI 模式的实测教训)。连接缓存复用(见 getChatgptConn)。
  const c = await getChatgptConn();

  // 首轮直接在当前对话里发;若静默失败(旧对话上下文污染/限频导致服务端不回复),
  // 自愈:导航开新对话重发一次(实测新对话稳定,旧对话偶发静默失败)。
  // 仅对话类模型(gpt-5-6 / gpt-5-6-mini / auto)启用结构化卡片清洗;gpt-coding 等
  // 编程通道原样保留代码/链接/artifact,不做任何清洗(避免误删代码运行结果里的
  // 复制按钮、引用链接等 UI 元素)。
  const doClean = chatgptShouldClean();
  if (!doClean) {
    // coding 通道:每请求导航新对话。页面历史会累积(含此前失败的拒绝样本),
    // 模型看到历史里"assistant 从不调用工具"的模式会被锚定继续拒绝(实测 pi
    // 重发同任务即复现);新对话彻底清零。对话通道(gpt-5-6)保留页面上下文不动。
    try {
      await c.cmd("Page.navigate", { url: chatgpt.homeUrl });
      await sleep(9000);
    } catch (e) { console.log("[chatgpt-ui] coding new-conversation navigate failed:", e.message); }
  }
  let text = await chatgptSendOnce(c, prompt, onDelta, doClean);
  if (text === null) {
    console.log("[chatgpt-ui] silent failure, self-heal: open new conversation");
    try {
      await c.cmd("Page.navigate", { url: chatgpt.homeUrl });
      await sleep(9000); // 等新对话加载 + composer 就绪
    } catch (e) { console.log("[chatgpt-ui] navigate failed:", e.message); }
    text = await chatgptSendOnce(c, prompt, onDelta, doClean);
  }
  if (text === null || !text.trim()) {
    const e = new Error("ChatGPT 无回复(可能被限频/风控,请检查页面 UI 状态)");
    e.code = "empty_reply";
    throw e;
  }
  if (doClean) {
    // 最终清洗:剥掉天气/搜索卡片的 UI 噪声(recharts 曲线、+1/Give feedback、域名
    // 链接、逐日温度表),只留自然语言摘要。仅对对话类模型生效。
    const cleaned = await cleanChatgptText(c);
    const finalText = cleaned && cleaned.trim() ? cleaned : text; // 清洗异常时回退原文本
    const self = { done: true, text: finalText };
    return self;
  }
  // 编程通道:原样返回(含代码/链接/artifact)
  const self = { done: true, text };
  return self;
}

// chatgptShouldClean:对话类模型(gpt-5-6 / gpt-5-6-mini / auto / auto-*)启用卡片清洗;
// gpt-coding 等编程通道不清洗(保留代码块/链接/artifact)。model 可能带 -chat 后缀。
function chatgptShouldClean() {
  const m = (chatgpt._model || "").toLowerCase();
  if (/coding/.test(m)) return false; // gpt-coding / gpt-coding-chat → 不清洗
  return true;                        // gpt-5-6 / gpt-5-6-mini / auto → 清洗
}

// chatgptSendOnce:在已就绪的 page 连接上注入 prompt + 轮询读回复。
// doClean=true 时轮询读取已清洗文本(对话类模型);false 时读原始 innerText(编程通道)。
// 返回文本(成功)或 null(消息已发出但服务端静默无回复 → 交给外层自愈)。
async function chatgptSendOnce(c, prompt, onDelta, doClean) {
  // 0) 页面带前台(失焦时 Input 事件不进编辑器,gemini 同坑)
  await c.cmd("Page.bringToFront").catch(() => {});

  // 1) 等 composer 可用并拿到坐标(回答生成期间不可用,轮询等待)
  const t0 = Date.now();
  let pos = null;
  for (let i = 0; i < 90; i++) {
    try {
      const r = await c.cmd("Runtime.evaluate", {
        expression: "(function(){ var el = document.querySelector('" + CHATGPT_COMPOSER_SEL + "'); if (!el) return 'NONE'; var b = el.getBoundingClientRect(); if (b.width === 0 || b.height === 0) return 'NONE'; return JSON.stringify({ x: b.x + b.width / 2, y: b.y + b.height / 2 }); })()",
        returnByValue: true,
      });
      const v = r.result && r.result.result && r.result.result.value;
      if (v && v !== "NONE") { pos = JSON.parse(v); break; }
    } catch {}
    await sleep(1000);
  }
  if (!pos) {
    const e = new Error("ChatGPT composer 不可用(请检查页面登录状态)");
    e.code = "upstream_error";
    throw e;
  }
  const tComposer = Date.now();
  if (tComposer - t0 > 500) console.log("[chatgpt-ui] composer wait", (tComposer - t0) + "ms");

  // 2) 清空 → 聚焦 → insertText → 验证,**最多 3 轮重插**。实测后台 tab 节流下
  //    React 调度延迟,单次 insertText 可能完全丢失(composer=0,2026-09-02 pi 实测
  //    send phase 34s 且首轮输入全空);重插循环是唯一可靠解,每轮都有长度校验兜底。
  const t0c = Date.now();
  const norm = (s) => (s || "").replace(/\s+/g, "");
  const targetLen = norm(prompt).length;
  let got = "";
  for (let attempt = 0; attempt < 3; attempt++) {
    await c.cmd("Runtime.evaluate", {
      expression: "(function(){ var el = document.querySelector('" + CHATGPT_COMPOSER_SEL + "'); if (!el) return 'NO'; el.focus(); document.execCommand('selectAll', false, null); document.execCommand('delete', false, null); return 'ok'; })()",
      returnByValue: true,
    });
    await sleep(120);
    await c.cmd("Input.dispatchMouseEvent", { type: "mousePressed", x: pos.x, y: pos.y, button: "left", clickCount: 1 });
    await c.cmd("Input.dispatchMouseEvent", { type: "mouseReleased", x: pos.x, y: pos.y, button: "left", clickCount: 1 });
    await sleep(100);
    await c.cmd("Input.insertText", { text: prompt });

    // 3) 验证输入生效:轮询式(通常首查即中 ~10ms)。比对归一化长度 —— 只查非空
    //    会让"部分插入/截断"溜过去(实测 pi 大 prompt 时模型收不到完整工具指令而拒绝)。
    for (let i = 0; i < 6; i++) {
      await sleep(i === 0 ? 60 : 90);
      const chk = await c.cmd("Runtime.evaluate", {
        expression: "(function(){ var el = document.querySelector('" + CHATGPT_COMPOSER_SEL + "'); return (el && (el.innerText || el.value)) || ''; })()",
        returnByValue: true,
      });
      got = (chk.result && chk.result.result && chk.result.result.value) || "";
      if (norm(got).length >= targetLen) break;
    }
    if (norm(got).length >= targetLen) break;
    console.log("[chatgpt-ui] input attempt " + (attempt + 1) + " insufficient (composer " + got.length + "/" + prompt.length + "), re-inserting");
    await sleep(400); // 给页面喘息(节流恢复)后重插
  }
  console.log("[chatgpt-ui] input check: prompt=" + prompt.length + " chars, composer=" + got.length + " chars, " + (Date.now() - t0c) + "ms");
  if (!norm(got).length) {
    const e = new Error("ChatGPT 输入未生效(composer 为空,已重试 3 轮)");
    e.code = "upstream_error";
    throw e;
  }
  if (norm(got).length < targetLen * 0.98) {
    const e = new Error("ChatGPT 输入被截断(prompt " + prompt.length + " 字符,composer 仅 " + got.length + ")");
    e.code = "upstream_error";
    throw e;
  }

  // 4) 发送:优先点击发送按钮(实测 Enter 在新版 ChatGPT 不触发发送 —— 文本已入
  //    React 状态、send-button 已出现,但 keydown Enter 被当作换行);按钮缺失时
  //    fallback CDP 真实 Enter(isTrusted=true;keyDown 必须带 text:'\r')
  let clicked = false;
  for (let i = 0; i < 10; i++) {
    const br = await c.cmd("Runtime.evaluate", {
      expression: "(function(){ var b = document.querySelector('button[data-testid=\"send-button\"]'); if (!b || b.disabled || b.getAttribute('aria-disabled') === 'true') return 'NONE'; var r = b.getBoundingClientRect(); if (r.width === 0) return 'NONE'; return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 }); })()",
      returnByValue: true,
    });
    const bv = br.result && br.result.result && br.result.result.value;
    if (bv && bv !== "NONE") {
      const p = JSON.parse(bv);
      await c.cmd("Input.dispatchMouseEvent", { type: "mousePressed", x: p.x, y: p.y, button: "left", clickCount: 1 });
      await c.cmd("Input.dispatchMouseEvent", { type: "mouseReleased", x: p.x, y: p.y, button: "left", clickCount: 1 });
      clicked = true;
      console.log("[chatgpt-ui] send-button clicked at", Math.round(p.x), Math.round(p.y), "| send phase", (Date.now() - t0) + "ms");
      break;
    }
    await sleep(300);
  }
  if (!clicked) {
    await c.cmd("Input.dispatchKeyEvent", { type: "keyDown", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, text: "\r" });
    await sleep(100);
    await c.cmd("Input.dispatchKeyEvent", { type: "keyUp", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13 });
    console.log("[chatgpt-ui] send-button not found, Enter fallback");
  }

  // 5) 轮询 DOM 读本次回复。
  // 关键实测(2026-09-02 探针):ChatGPT 页面在发送新消息时会**折叠/复用消息节点**,
  // 导致 assistantCount 跳变、最后一条 assistant 节点在过渡期短暂显示上一条的残影。
  // 若每轮把"最后节点 innerText"累加,会把上一条残影拼进本条 → 串台/截断(实测发
  // "天气"拿到上一条"你好"的残影)。
  // 修复:用「停止按钮」作为生成结束信号(stopBtn=true=生成中,false=本轮结束),
  // **只在生成结束后读取一次完整 lastA**;过渡态(stopBtn=true)不读,彻底避开串台。
  // 同时每轮不再调用 cleanChatgptText(仅在结束时读一次),降低开销、缩短延迟。
  const promptHead = prompt.slice(0, 30);
  let text = "";
  let started = false;
  let silentSecs = 0; // 消息已发出但 assistant 持续为空 → 静默失败计时
  for (let i = 0; i < 150; i++) { // 最长 150s
    await sleep(1000);
    const r = await c.cmd("Runtime.evaluate", {
      expression: "(function(){" +
        "var a = document.querySelectorAll('" + CHATGPT_ASSISTANT_SEL + "');" +
        "var u = document.querySelectorAll('[data-message-author-role=\"user\"]');" +
        "var lastU = u.length ? (u[u.length - 1].innerText || '') : '';" +
        "var lastA = a.length ? (a[a.length - 1].innerText || '') : '';" +
        "var stopBtn = !!document.querySelector('button[aria-label*=\"Stop\"]');" +
        "return JSON.stringify({ lastU: lastU, lastA: lastA, stopBtn: stopBtn });" +
        "})()",
      returnByValue: true,
    });
    const v = r && r.result && r.result.result && r.result.result.value;
    if (!v) continue;
    let j;
    try { j = JSON.parse(v); } catch { continue; }
    if (i === 0) console.log("[chatgpt-ui] polling started, promptHead=" + JSON.stringify(promptHead));
    if (i > 0 && i % 30 === 0 && !text && !started) console.log("[chatgpt-ui] still waiting, sec=" + i, "silent=" + silentSecs);
    // 新一轮已开始:最后一条 user 消息是本次 prompt(确保读到的是本轮回复)
    if (!started && j.lastU && j.lastU.indexOf(promptHead) !== -1) started = true;
    if (!started) continue;
    // 还在生成(stopBtn=true)→ 等,不读(避免抓到上一条残影/过渡态)
    if (j.stopBtn) { silentSecs = 0; continue; }
    // 生成结束(stopBtn=false)→ 本轮回复已稳定,读取一次完整文本
    // doClean=true:已清洗文本(对话类,卡片噪声剥除);false:DOM→markdown 还原
    // (coding:``` 围栏还原保证 FenceParser 解析工具调用,其余原样保留)
    const finalRaw = doClean ? (await cleanChatgptText(c)) : ((await extractChatgptRaw(c)) || "");
    if (finalRaw && finalRaw.trim()) {
      text = finalRaw.trim();
      if (onDelta) onDelta(text);
      break;
    }
    // stopBtn 已消失但文本仍空:可能渲染滞后,再等几轮(累计静默)
    silentSecs++;
    if (silentSecs >= 70) { console.log("[chatgpt-ui] silent no-reply detected, aborting this attempt"); return null; }
  }
  if (!text || !text.trim()) return null;
  return text;
}

// extractChatgptRaw:coding 通道专用 —— 把最后一跳 assistant 节点做「DOM → markdown 还原」。
// 背景:aurora coding 协议是 ```json 围栏(glmBuildInstructions),但 ChatGPT 前端把围栏
// 渲染成 <pre><code>,innerText 拿不到 ``` 反引号 → aurora 的 FenceParser 失效(实测轮1:
// 模型明明遵守协议输出了围栏 JSON,返回后却成 "JSON\n{...}" 裸文本)。
// 还原规则(实测 2026-09-02 DOM 探针):
//   - 只取最外层 <pre>(ChatGPT 代码块有嵌套 pre,querySelectorAll 会命中多层导致重复);
//   - pre.innerText 首行若是已知语言名(json/python/...)则作为围栏语言(渲染时语言标签
//     进了 pre 内部首行),其余行为代码体;
//   - 其余文本原样保留(coding 原样保留原则:不删链接/按钮/任何内容);
//   - 块级元素(P/DIV/LI/H1-6/TR/BR)边界补换行,贴近 innerText 语义。
async function extractChatgptRaw(c) {
  const EV = "(function(){" +
    "var a = document.querySelectorAll('" + CHATGPT_ASSISTANT_SEL + "');" +
    "var root = a.length ? a[a.length - 1] : null;" +
    "if (!root) return JSON.stringify({ text: '' });" +
    "var LANGS = {}; 'json python python3 py javascript js typescript ts bash sh shell zsh html css sql go rust java kotlin c cpp csharp cs yaml yml toml ini diff markdown md xml lua perl ruby php swift dart scala r matlab dockerfile makefile powershell ps1 text'.split(' ').forEach(function(x){LANGS[x]=1;});" +
    "var out = [];" +
    "function blockNewline(el) { var t = (el.tagName || '').toUpperCase(); return (t==='P'||t==='DIV'||t==='LI'||t==='TR'||t==='BR'||/^H[1-6]$/.test(t)); }" +
    "function walk(n) {" +
    "  if (n.nodeType === 3) { out.push(n.nodeValue || ''); return; }" +
    "  if (n.nodeType !== 1) return;" +
    "  var tag = (n.tagName || '').toUpperCase();" +
    "  if (tag === 'PRE') {" +
    "    if (n.offsetParent === null) return;" + // 隐藏渲染副本(display:none,不进 innerText 但被选择器命中)
    "    if (n.parentElement && n.parentElement.closest && n.parentElement.closest('pre')) return;" + // 嵌套子 pre 跳过(外层已整体围栏化)
    "    var txt = n.innerText || n.textContent || '';" +
    "    var lines = txt.split('\\n');" +
    "    var lang = '';" +
    "    if (lines.length > 1 && LANGS[lines[0].trim().toLowerCase()]) { lang = lines[0].trim().toLowerCase(); lines = lines.slice(1); }" +
    "    out.push('\\n```' + lang + '\\n' + lines.join('\\n') + '\\n```\\n');" +
    "    return;" + // 不递归 → 嵌套子 pre 跳过
    "  }" +
    "  for (var i = 0; i < n.childNodes.length; i++) walk(n.childNodes[i]);" +
    "  if (blockNewline(n)) out.push('\\n');" +
    "}" +
    "walk(root);" +
    "var txt = out.join('').replace(/\\n{3,}/g, '\\n\\n').trim();" +
    "return JSON.stringify({ text: txt });" +
    "})()";
  try {
    const r = await c.cmd("Runtime.evaluate", { expression: EV, returnByValue: true });
    const v = r && r.result && r.result.result && r.result.result.value;
    const j = v ? JSON.parse(v) : null;
    return (j && j.text) || "";
  } catch (e) {
    return "";
  }
}

// cleanChatgptText:把 ChatGPT 最后一跳 assistant 节点的 innerText 清洗成可读文本。
// 背景:ChatGPT 的天气/搜索等工具返回的是**带交互控件的富卡片**(recharts 温度曲线、
// `+1`/`Give feedback` 按钮、域名链接、逐日预报数字表),这些被拍平成 innerText 后变成
// 噪声(实测"问济南天气"会吐出东京天气卡片碎片 + recharts 曲线)。语音/小爱场景(TTS 朗读)
// 与聊天页都不该出现这些 UI 碎片。
// 清洗策略(DOM 级,对编程场景零误伤):
//   - 跳过 class 含 recharts / _Card / _Box / IndicatorWrapper 的容器(天气/搜索 App 卡片);
//   - 代码块 <pre>/<code> 原样保留(Programming/MCP 场景要靠它);
//   - 卡片前的自然语言摘要(如"北京今天晴,28℃")位于这些容器之外,自然保留。
// 仅对 chatgpt 通道生效;gemini/claude 走各自解析器不受影响。
async function cleanChatgptText(c) {
  const EV = "(() => {" +
    "var a = document.querySelectorAll('" + CHATGPT_ASSISTANT_SEL + "');" +
    "var root = a.length ? a[a.length - 1] : null;" +
    "if (!root) return JSON.stringify({ text: '' });" +
    "var SKIP = /recharts|IndicatorWrapper|_Card|_Box|temperature|forecast|weather-widget|weather-app/i;" +
    "var out = [];" +
    "function walk(n) {" +
    "  if (n.nodeType === 3) { var t = n.nodeValue || ''; if (t.trim()) out.push(t); return; }" +
    "  if (n.nodeType !== 1) return;" +
    "  var tag = (n.tagName || '').toUpperCase();" +
    "  if (tag === 'PRE' || tag === 'CODE') { out.push(n.innerText || n.textContent || ''); return; }" +
    "  if (tag === 'A') return;" +   // 跳过链接(天气/搜索卡片里的媒体域名如 thepaper.cn/bjnews 都是 <a>,语音场景无需朗读)
    "  if (tag === 'BUTTON' || tag === 'SVG') return;" +  // 跳过按钮/图标(如 +1 / Give feedback / 分享图标)
    "  var cls = n.getAttribute('class') || '';" +
    "  if (SKIP.test(cls)) return;" +                         // 跳过天气/搜索卡片容器
    "  for (var i = 0; i < n.childNodes.length; i++) walk(n.childNodes[i]);" +
    "}" +
    "walk(root);" +
    "var txt = out.join('\\n').replace(/\\n{3,}/g, '\\n\\n').trim();" +
    "return JSON.stringify({ text: txt });" +
    "})()";
  try {
    const r = await c.cmd("Runtime.evaluate", { expression: EV, returnByValue: true });
    const v = r && r.result && r.result.result && r.result.result.value;
    const j = v ? JSON.parse(v) : null;
    let txt = (j && j.text) || "";
    if (!txt) return "";
    // 兜底正则清洗(DOM 排除可能漏掉卡片外的小 UI 元素:域名链接、+1、Give feedback)
    // 注意:只剥明确的 UI 噪声词,不碰代码/JSON(编程场景靠前面的 DOM 排除已保留 <pre>/<code>)。
    txt = txt
      .replace(/\b(japan\.travel|chinanews\.com\.cn|bjnews\.com\.cn|give feedback)\b/gi, " ")
      // "+1" 在 DOM 里常拆成 "+" 与 "1" 两个文本节点(中间夹换行),用 [\s]* 跨越
      .replace(/\+\s*1\b/g, " ")
      .replace(/[A-Za-z0-9._-]+\.(com|cn|net|org|travel)(\/[^\s]*)?/g, (m) =>
        /(japan\.travel|chinanews|bjnews)/i.test(m) ? " " : m)
      .replace(/\n{3,}/g, "\n\n")
      .replace(/[ \t]{2,}/g, " ")
      .trim();
    return txt;
  } catch (e) {
    return "";
  }
}

// ─── Gemini UI 注入模式(2026-08-21)───────────────────────────────
// 背景:8/19 前端升级后 at 令牌(fsec 格式)变成一次性 —— 捕获-复用被服务端拒绝
// (1097),自造 at 也 400。绕过方案:不动 fetch,改让页面 UI 自己发消息
// (页面 JS 实时生成有效 at),桥监听 StreamGenerate 响应并解析文本。
// 代价:每次请求在页面产生一条可见对话;会话上下文由页面自行维护(多轮无需拍平)。
async function executeGeminiUI(entry, prompt, onDelta) {
  // 用独立 CDP 连接执行 UI 操作与响应监听:共享连接上的事件风暴/监听器
  // (applyCapture 等)会干扰 Input 事件时序(实测共享连接 Enter 不发送,独立连接成功)
  const page = await findTarget(gemini.pageMatch);
  if (!page) { const e = new Error("gemini 页面未打开"); e.code = "no_browser"; throw e; }
  const c = await cdp(page.webSocketDebuggerUrl);
  await c.cmd("Network.enable");
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
        // 不打印(事件风暴);仅匹配 targetReqId 时处理
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
  // 窗口失焦时 Enter 的字符输入(text:"\r")不进编辑器 → 消息永远不提交
  // (2026-08-31 实测:Xvfb 桌面长时间无交互后必现,表现为"Enter sent 但无回复")。
  // 输入前强制把 Gemini 页面带前台。对 claude 通道无影响(模板请求不走页面 UI)。
  await c.cmd("Page.bringToFront").catch(() => {});
  // 等输入框可用(回答生成期间输入框不可用,轮询等待)
  let pos = null;
  for (let i = 0; i < 90; i++) {
    try {
      const r = await c.cmd("Runtime.evaluate", {
        expression: `(function(){
          const el = document.querySelector('.ql-editor') || document.querySelector('[contenteditable="true"]');
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

  // 清空输入框残留:execCommand selectAll+delete(可靠;CDP Ctrl+A 依赖焦点,残留时失效)
  await c.cmd("Runtime.evaluate", {
    expression: `(function(){ const el = document.querySelector('.ql-editor') || document.querySelector('[contenteditable="true"]'); if (!el) return 'NO'; el.focus(); document.execCommand('selectAll', false, null); document.execCommand('delete', false, null); return 'ok'; })()`,
    returnByValue: true,
  });
  await sleep(400);

  await c.cmd("Input.dispatchMouseEvent", { type: "mousePressed", x: pos.x, y: pos.y, button: "left", clickCount: 1 });
  await c.cmd("Input.dispatchMouseEvent", { type: "mouseReleased", x: pos.x, y: pos.y, button: "left", clickCount: 1 });
  await sleep(400);
  // 插入文本(CDP insertText 实测与 JS click 组合可用;SendButtonDesync 是页面偶发,
  // 由外层 reload 重试兜底)
  await c.cmd("Input.insertText", { text });
  await sleep(500);

  // 验证输入生效
  const chk = await c.cmd("Runtime.evaluate", {
    expression: `(function(){ const el = document.querySelector('.ql-editor') || document.querySelector('[contenteditable="true"]'); return (el && (el.innerText || el.value)) || ''; })()`,
    returnByValue: true,
  });
  const got = chk.result && chk.result.result && chk.result.result.value;
  console.log("[gemini-ui] input content:", JSON.stringify(got).slice(0, 60));
  if (!got || !got.trim()) { console.log("[gemini-ui] input empty"); return false; }

  // 发送:CDP 真实 Enter 键(实测 JS click 被页面 isTrusted 拦截 [SendButtonDesync];
  // CDP 键盘事件 isTrusted=true 有效;keyDown 必须带 text:"\r" 触发字符输入,否则不发送)
  // 注意:发送成功后输入框**可能残留**(实测 Gemini 偶发不清空,但消息已发出、响应正常)
  // —— 不能以"输入框清空"判定成败(会误判 desync → reload 循环),改由
  // executeGeminiUI 的响应监听判定,这里 Enter 后直接返回。
  await c.cmd("Input.dispatchKeyEvent", { type: "keyDown", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, text: "\r" });
  await sleep(100);
  await c.cmd("Input.dispatchKeyEvent", { type: "keyUp", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13 });
  console.log("[gemini-ui] Enter sent");
  return true;
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
  // ChatGPT 浏览器通道:记录本次请求的模型(gpt-5-6 / gpt-5-6-mini),executeChatgpt 用它覆盖模板 model
  if (adapter.name === "chatgpt") adapter.setModel(model);
  // 回显给调用方的 model 名要去掉桥内部用的 -chat 后缀(aurora 侧加的标识)
  const outModel = adapter.name === "chatgpt" ? model.replace(/-chat$/, "") : model;
  // Gemini / ChatGPT UI 模式:页面自维护会话,只发最后一条用户消息(不能拍平历史,否则与页面上下文重复)
  const prompt = (adapter.name === "gemini" || adapter.name === "chatgpt")
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
        id, object: "chat.completion", created, model: outModel,
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
          id, object: "chat.completion.chunk", created, model: outModel,
          choices: [{ index: 0, delta: { content: delta }, finish_reason: null }],
        });
      })
    );
    sse({ id, object: "chat.completion.chunk", created, model: outModel, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] });
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
    providers.chatgpt = {
      mode: "ui-driven (DOM poll)",
      model: chatgpt._model || null,
      connected: !!conns.get("chatgpt"),
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
const hasHunyuan = loadHunyuanCfg();
server.listen(PORT, HOST, async () => {
  console.log("[bridge] listening on http://" + HOST + ":" + PORT);
  console.log("[bridge] auth:", AUTH ? "enabled" : "disabled (localhost only)");
  console.log("[bridge] tokens:", hasTokens ? "loaded" : "MISSING (run capture-streamgenerate.mjs once)");
  console.log("[bridge] claude:", hasClaude ? "loaded" : "MISSING (run capture-claude.mjs once)");
  console.log("[bridge] hunyuan:", hasHunyuan ? "loaded" : "MISSING (run capture-yuanbao.mjs once)");
  console.log("[bridge] chatgpt: ui-driven mode (needs logged-in chatgpt.com tab; no template needed)");
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
