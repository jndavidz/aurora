#!/usr/bin/env node
// credential-keeper —— 凭证健康探测与告警(可靠性 D4 原型)
//
// 数据源:NAS aurora 的 /v1/health/credentials(A3 端点)。
// probe 为纯 HTTP —— 按音频保护规程不受 22:00–24:00 窗口约束,可随时执行。
//
// 职责(原型):
//   probe  拉 A3 端点 → 汇总 ChatGPT 池 + 各 provider 的状态与剩余天数
//   落盘   状态快照 /var/lib/credential-keeper/last-probe.json
//   alert  expired/critical → console.error + 可选 webhook(CREDENTIAL_KEEPER_ALERT_URL)
//          状态与上次相同则降级为"(持续)",避免 journal 刷屏
//   act    (需浏览器的重抓)原型阶段留空 —— 等 D3 通过、桥池接好后补,
//          且按音频保护规程强制排 22:00–24:00 窗口
//
// 退出码:0 = 全部 ok/warn;1 = 存在 expired/critical 告警;2 = 探测失败。
import fs from "node:fs";
import path from "node:path";

const BASE = process.env.CREDENTIAL_KEEPER_URL || "http://10.10.10.2:65432";
const TOKEN = process.env.CREDENTIAL_KEEPER_TOKEN || "david";
const STATE_DIR = process.env.CREDENTIAL_KEEPER_STATE_DIR || "/var/lib/credential-keeper";
const ALERT_URL = process.env.CREDENTIAL_KEEPER_ALERT_URL || "";
// 告警级别阈值:expired/critical 触发 ERROR+webhook;warn 仅日志。
const ALERT_LEVELS = new Set(["expired", "critical"]);
// ChatGPT 池 access token 的剩余天数分档(与 provider 的 statusFor 对齐)。
const CRIT_DAYS = 3, WARN_DAYS = 14;

const now = () => new Date().toISOString();

// ── 1. 探测 ──────────────────────────────────────────────────────────
let data;
try {
  const res = await fetch(`${BASE}/v1/health/credentials`, {
    headers: { Authorization: `Bearer ${TOKEN}` },
    signal: AbortSignal.timeout(15000),
  });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  data = await res.json();
} catch (err) {
  console.error(`[keeper][ERROR] 探测失败(${BASE}): ${err.message} —— NAS 不可达或服务未监听`);
  process.exit(2);
}

// ── 2. 汇总 ──────────────────────────────────────────────────────────
const alerts = []; // {name, level, detail}
const lines = [];

for (const p of data.providers ?? []) {
  lines.push(`  ${p.name.padEnd(10)} ${p.status.padEnd(10)} accounts=${p.accounts}${p.minRefreshExpiresAt ? `  最早过期=${p.minRefreshExpiresAt}` : ""}${p.detail ? `  [${p.detail}]` : ""}`);
  if (ALERT_LEVELS.has(p.status)) {
    alerts.push({ name: p.name, level: p.status, detail: p.minRefreshExpiresAt ? `最早过期 ${p.minRefreshExpiresAt}` : p.detail });
  }
}

const pool = data.chatgptPool ?? {};
if (pool.minAccessExpiresAt != null) {
  const days = pool.minAccessExpiresInDays ?? 0;
  let level = "ok";
  if (days < 0) level = "expired";
  else if (days < CRIT_DAYS) level = "critical";
  else if (days < WARN_DAYS) level = "warn";
  lines.push(`  ${"chatgptPool".padEnd(10)} ${level.padEnd(10)} accounts=${pool.free + pool.noauth + pool.puid + pool.temporary}(active=${pool.active})  最早过期=${pool.minAccessExpiresAt}`);
  if (ALERT_LEVELS.has(level)) {
    alerts.push({ name: "chatgptPool", level, detail: `最早过期 ${pool.minAccessExpiresAt}` });
  } else if (level === "warn") {
    console.warn(`[keeper][WARN] chatgptPool access token 距过期 ${days.toFixed(1)} 天`);
  }
}
// 被动发现链标记过期的账号(401→ReportFailure):提示"等健康检查续期"
if (pool.expired > 0) {
  lines.push(`  (池内 ${pool.expired} 个账号被标记过期,等 StartHealthCheck 续期)`);
}

console.log(`[keeper] ${now()} 探测完成:\n${lines.join("\n")}`);

// ── 3. 落盘 ──────────────────────────────────────────────────────────
fs.mkdirSync(STATE_DIR, { recursive: true });
fs.writeFileSync(path.join(STATE_DIR, "last-probe.json"), JSON.stringify({
  checkedAt: now(),
  base: BASE,
  alerts,
  data,
}, null, 2));

// ── 4. 告警(去重:状态未变则降级为持续标注)────────────────────────
const stateFile = path.join(STATE_DIR, "last-alert-state.json");
const alertKey = JSON.stringify(alerts.map((a) => `${a.name}:${a.level}`).sort());
const prev = fs.existsSync(stateFile) ? fs.readFileSync(stateFile, "utf8").trim() : "";
const changed = alertKey !== prev;
fs.writeFileSync(stateFile, alertKey);

if (alerts.length === 0) {
  console.log(`[keeper] ${now()} 全部凭证 ok/warn(见上方明细)`);
  process.exit(0);
}

for (const a of alerts) {
  const msg = `[keeper] ${a.name}: ${a.level} —— ${a.detail}${changed ? "" : " (与上次相同,持续)"}`;
  (ALERT_LEVELS.has(a.level) ? console.error : console.warn)(msg);
}

if (changed && ALERT_URL) {
  try {
    await fetch(ALERT_URL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text: `aurora 凭证告警: ${alerts.map((a) => `${a.name}=${a.level}(${a.detail})`).join("; ")}` }),
      signal: AbortSignal.timeout(10000),
    });
  } catch (err) {
    console.error(`[keeper][ERROR] webhook 发送失败: ${err.message}`);
  }
}

process.exit(alerts.length ? 1 : 0);
