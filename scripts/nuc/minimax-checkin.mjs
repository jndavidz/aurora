// minimax-checkin.mjs — MiniMax 每日签到(纯 Node,无浏览器依赖)· NUC 版
//
// 背景:MiniMax 对话消耗 Token Plan 积分(耗尽报 2056)。网页有每日签到:
//   GET  /minimax-cloud/api/v1/signin/status   签到面板(7 天连续签到,400~1000 积分/天)
//   POST /minimax-cloud/api/v1/signin/claim    签到提交(幂等:已签过不重复发放)
// 积分 30 天有效,仅限 MiniMax Code 使用。
// 认证:token(URL 参数 + header)+ x-signature = md5(ts + salt + body),与对话接口同签名体系。
//
// 部署:NUC /opt/credential-keeper/(systemd timer 每日 09:00+rand30min,
// 在 token-harvester 08:15 之后,用当日提取的新鲜 token)。
// token 来源:harvester 落盘的本地 state;兜底从 NAS 部署区拉。
// 权威副本:scripts/nuc/minimax-checkin.mjs(改先改仓库再同步)。
// 用法: node minimax-checkin.mjs [token文件路径]
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

const SALT = "I*7Cf%WZ#S&%1RlZJ&C2";
const DEVICE_ID = "92622880";
const USER_ID = "544661156126703622";
import { execSync } from "node:child_process";
const STATE_FILE = "/opt/credential-keeper/state/minimax_tokens.txt";
function resolveTokenFile() {
  if (process.argv[2]) return process.argv[2];
  try { if (fs.statSync(STATE_FILE).size > 0) return STATE_FILE; } catch {}
  // 兜底:从 NAS 部署区拉一份到 state
  try {
    execSync(`ssh -o BatchMode=yes zxsadmin@10.10.10.2 "cat /volume2/docker/aurora/tokens/minimax_tokens.txt" > ${STATE_FILE}`, { timeout: 20000, shell: "/bin/bash" });
    return STATE_FILE;
  } catch { return STATE_FILE; }
}
const TOKEN_FILE = resolveTokenFile();

const token = fs.readFileSync(TOKEN_FILE, "utf8").trim().split(/\r?\n/)[0];
if (!token) { console.log("[checkin] no token"); process.exit(1); }

const sign = (ts, body) => crypto.createHash("md5").update(`${ts}${SALT}${body}`).digest("hex");

function commonQuery() {
  const q = new URLSearchParams({
    device_platform: "web", biz_id: "3", app_id: "3001", version_code: "22201",
    unix: String(Date.now()), timezone_offset: "28800", sys_language: "zh", lang: "zh",
    uuid: crypto.randomUUID(), device_id: DEVICE_ID, os_name: "Windows", browser_name: "Chrome",
    device_memory: "16", cpu_core_num: "8", browser_language: "zh-CN", browser_platform: "Win32",
    user_id: USER_ID, screen_width: "1920", screen_height: "1080", token, client: "web", region: "cn",
  });
  return q.toString();
}

async function api(method, apiPath, bodyObj) {
  const body = bodyObj ? JSON.stringify(bodyObj) : "";
  const ts = Math.floor(Date.now() / 1000);
  const url = "https://agent.minimaxi.com" + apiPath + "?" + commonQuery();
  const r = await fetch(url, {
    method,
    headers: {
      "Content-Type": "application/json", token,
      "x-timestamp": String(ts), "x-signature": sign(ts, body),
      Origin: "https://agent.minimaxi.com", Referer: "https://agent.minimaxi.com/",
      "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
    },
    ...(method === "GET" ? {} : { body }),
  });
  const txt = await r.text();
  let json = null;
  try { json = JSON.parse(txt); } catch {}
  return { status: r.status, json, raw: txt };
}

const st = await api("GET", "/minimax-cloud/api/v1/signin/status", null);
if (st.json && st.json.base_resp && st.json.base_resp.status_code === 0) {
  const days = st.json.data?.days || [];
  const today = days.find((d) => d.is_today);
  console.log("[checkin] today:", today ? `day${today.day_no} points=${today.points} status=${today.status}` : "n/a");
} else {
  console.log("[checkin] status query failed:", st.raw.slice(0, 200));
}

const cl = await api("POST", "/minimax-cloud/api/v1/signin/claim", {});
if (cl.json && cl.json.base_resp && cl.json.base_resp.status_code === 0) {
  const d = cl.json.data || {};
  if (d.claim_result !== undefined) {
    console.log(`[checkin] ✅ 签到成功: day${d.day_no} +${d.points} 积分 (expire ${new Date(d.expire_at_ms).toISOString()})`);
  } else {
    console.log("[checkin] 已签到过(幂等),今日积分:", d.points || "?");
  }
  process.exit(0);
} else {
  console.log("[checkin] ❌ 签到失败:", cl.raw.slice(0, 250));
  process.exit(1);
}
