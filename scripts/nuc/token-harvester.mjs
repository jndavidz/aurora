// token-harvester.mjs — NUC 统一凭证提取器(G1 act/publish,2026-09-05)
//
// 架构事实(2026-09-05 确认):各模型登录态权威在 NUC Chrome
// (/opt/chrome-cdp/profile)。本脚本经 CDP(127.0.0.1:9222)从页面
// localStorage / cookie 提取最新凭证,推 NAS 部署区,配合 aurora E3
// 热加载免重启生效。
//
// 站点范围(有意排除,勿随手加回):
//   - 豆包:通道冻结(2026-09-05 拍板),doubao-hook 捕获通路保留但不运行
//   - GLM/Kimi:容器读 tokens-state(rw)进程 refresh 自愈,文件推送无效
//   - Gemini/Claude:桥体系,不经 token 文件
//   - ChatGPT:2026-09-05 并入(页面 fetch /api/auth/session 提取 access+session,
//     覆盖单账号池文件;当前池即单账号,多账号化时需改多行合并逻辑)
//
// 幂等:提取值与 NAS 部署区现值 md5 对比,变化才推。
// 凭证红线:日志只记 OK/FAIL + 长度/天数,绝不打印凭证内容。
import http from "node:http";
import { execSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";

const { cdp } = await import(new URL("./cdp-helper.mjs", import.meta.url).href);

const CDP_PORT = 9222;
const NAS = "zxsadmin@10.10.10.2";
const DST = "/volume2/docker/aurora/tokens";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// cdp.cmd 超时包装(零依赖 cdp-helper 无超时,getAllCookies 挂起会卡死整轮)
const cmdT = (c, method, params, ms = 20000) =>
  Promise.race([
    c.cmd(method, params),
    new Promise((_, rej) => setTimeout(() => rej(new Error(`${method} timeout ${ms}ms`)), ms)),
  ]);

// 每站提取规则:
//   kind=local   读 localStorage(键按序试)
//   kind=cookies Network.getAllCookies 过滤域,按 assemble() 组装行
//   (grok 格式特殊:每行 uid|cookie串,uid = x-userid cookie 值)
// deepseek(2026-09-05 三次修正):网页版改版——localStorage userToken 现存
// JSON 包裹 {"value":"<64字符token>","__version":"1"},裸发整个字符串即上游
// invalid token(前两轮"游客票"误判实为未解包)。jsonField: "value" 解包。
const SITES = [
  { name: "deepseek", url: "https://chat.deepseek.com/", kind: "local", keys: ["userToken"], file: "deepseek_tokens.txt", jsonField: "value" },
  { name: "minimax", url: "https://agent.minimaxi.com/", kind: "local", keys: ["_token"], file: "minimax_tokens.txt", jwt: true },
  { name: "mimo", url: "https://aistudio.xiaomimimo.com/", kind: "cookies", domain: /xiaomimimo|xiaomi/i, require: "xiaomichatbot_ph", file: "mimo_tokens.txt",
    assemble: (rel) => rel.map((x) => `${x.name}=${x.value}`).join("; ") },
  { name: "qianwen", url: "https://www.qianwen.com/", kind: "cookies", domain: /qianwen/i, require: "tongyi_sso_ticket", file: "qianwen_tokens.txt",
    assemble: (rel) => rel.map((x) => `${x.name}=${x.value}`).join("; ") },
  { name: "grok", url: "https://grok.com/", kind: "cookies", domain: /grok\.com/i, require: "sso", file: "grok_cookies.txt",
    assemble: (rel) => {
      const uid = rel.find((x) => x.name === "x-userid");
      const cookie = rel.map((x) => `${x.name}=${x.value}`).join("; ");
      return (uid ? uid.value : "") + "|" + cookie;
    } },
  { name: "chatgpt", url: "https://chatgpt.com/", kind: "chatgpt",
    files: { access: "access_tokens.txt", session: "session_tokens.txt" } },
];

function httpReq(port, method, p) {
  return new Promise((resolve, reject) => {
    const r = http.request({ host: "127.0.0.1", port, path: p, method }, (res) => {
      let d = "";
      res.on("data", (c) => (d += c));
      res.on("end", () => resolve({ status: res.statusCode, body: d }));
    });
    r.on("error", reject);
    r.end();
  });
}

// 找/开站点标签(CDP 恢复标签页时轮询;PUT /json/new 对齐新 Chrome)
async function ensurePage(url) {
  for (let i = 0; i < 25; i++) {
    try {
      const targets = JSON.parse((await httpReq(CDP_PORT, "GET", "/json")).body);
      const t = targets.find((x) => x.type === "page" && x.url.startsWith(url));
      if (t) return t;
    } catch { }
    await sleep(2000);
  }
  const r = await httpReq(CDP_PORT, "PUT", "/json/new?" + encodeURIComponent(url));
  const t = JSON.parse(r.body);
  await sleep(8000); // 等页面加载 + localStorage 写入
  return t;
}

async function readLocal(c, keys) {
  for (const k of keys) {
    try {
      const r = await cmdT(c, "Runtime.evaluate", {
        expression: `localStorage.getItem(${JSON.stringify(k)})`,
        returnByValue: true,
      });
      const v = r.result && r.result.result && r.result.result.value;
      if (v) return String(v);
    } catch { }
  }
  return null;
}

function validJWT(token) {
  try {
    const payload = JSON.parse(Buffer.from(token.split(".")[1], "base64url").toString());
    return !payload.exp || payload.exp * 1000 > Date.now();
  } catch {
    return false;
  }
}

// harvestSite 返回 {文件名: 内容} 映射(多数站单文件;chatgpt 双文件)。
async function harvestSite(site) {
  const t = await ensurePage(site.url);
  const c = await cdp(t.webSocketDebuggerUrl);
  try {
    if (site.kind === "chatgpt") {
      // 页面上下文 fetch /api/auth/session(同源带 cookie)拿 accessToken
      // (session→access exchange 已失效,页面直取最可靠);session-token 走 cookie。
      const r = await cmdT(c, "Runtime.evaluate", {
        expression: `(async () => { try { const r = await fetch('/api/auth/session', { credentials: 'include' }); const j = await r.json(); return JSON.stringify({ at: j.accessToken || '' }); } catch (e) { return JSON.stringify({ at: '' }); } })()`,
        returnByValue: true,
        awaitPromise: true,
      }, 25000);
      const at = (JSON.parse(r.result.result.value) || {}).at || "";
      if (!at) throw new Error("no accessToken (logged out?)");
      const gr = await cmdT(c, "Network.getAllCookies");
      // NextAuth 大 cookie 分片:__Secure-next-auth.session-token.0/.1/... 按序拼接
      const parts = (gr.result.cookies || [])
        .filter((x) => /^__Secure-next-auth\.session-token(\.\d+)?$/.test(x.name))
        .sort((a, b) => {
          const na = parseInt((a.name.match(/\.(\d+)$/) || [])[1] || "0", 10);
          const nb = parseInt((b.name.match(/\.(\d+)$/) || [])[1] || "0", 10);
          return na - nb;
        });
      if (!parts.length || !parts[0].value) throw new Error("no session-token cookie (logged out?)");
      const sessValue = parts.map((x) => x.value).join("");
      return { [site.files.access]: at, [site.files.session]: sessValue };
    }
    let value = null;
    if (site.kind === "local") {
      value = await readLocal(c, site.keys);
      if (!value || value.length < 20) throw new Error("no token in localStorage");
      // 2026-09-05 deepseek 改版兼容:userToken 存 JSON 包裹({"value":...}),
      // 解包取真 token;裸字符串(旧版)原样使用
      if (site.jsonField) {
        try {
          const parsed = JSON.parse(value);
          if (parsed && typeof parsed[site.jsonField] === "string" && parsed[site.jsonField]) {
            value = parsed[site.jsonField];
          }
        } catch { /* 非 JSON(旧版裸 token):原样 */ }
      }
      if (site.jwt && !validJWT(value)) throw new Error("token expired/invalid JWT");
    } else {
      const gr = await cmdT(c, "Network.getAllCookies");
      const all = gr.result.cookies || [];
      const rel = all.filter((ck) => site.domain.test(ck.domain));
      const req = rel.find((x) => x.name === site.require);
      if (!req || !req.value) throw new Error(`no ${site.require} cookie (logged out?)`);
      value = site.assemble(rel);
    }
    return { [site.file]: value };
  } finally {
    c.close();
  }
}

function remoteMd5(file) {
  try {
    const out = execSync(`ssh -o BatchMode=yes ${NAS} "md5sum '${DST}/${file}' 2>/dev/null | cut -d' ' -f1"`, { encoding: "utf8", timeout: 20000 });
    return out.trim();
  } catch {
    return "";
  }
}

function pushToken(file, content) {
  const remote = `${DST}/${file}`;
  // cat | ssh 直写(对齐 doubao-hook 通路);管道规避群晖 SFTP 兼容问题
  execSync(`cat | ssh -o BatchMode=yes ${NAS} "cat > '${remote}'"`, {
    input: content + "\n",
    timeout: 20000,
    shell: "/bin/bash",
  });
  execSync(`ssh -o BatchMode=yes ${NAS} "chmod 644 '${remote}'"`, { timeout: 15000 });
}

// 本地 state 落盘:供同机后续任务使用(如 minimax-checkin 签到读当日 token),
// 免得签到再去 NAS 拉。state 属 600,与凭证红线一致。
const STATE_DIR = new URL("./state/", import.meta.url).pathname;
function saveState(file, content) {
  try {
    fs.mkdirSync(STATE_DIR, { recursive: true });
    fs.writeFileSync(path.join(STATE_DIR, file), content + "\n", { mode: 0o600 });
  } catch (e) {
    console.log(`[harvest] state write failed for ${file}: ${e.message}`);
  }
}

console.log("[harvest]", new Date().toISOString(), "start");
const failures = [];
for (const site of SITES) {
  try {
    const outputs = await harvestSite(site);
    for (const [file, value] of Object.entries(outputs)) {
      const md5 = createHash("md5").update(value + "\n").digest("hex");
      if (md5 === remoteMd5(file)) {
        saveState(file, value); // 幂等也要保证 state 最新(签到读它)
        console.log(`[harvest] ${site.name}/${file}: unchanged(幂等跳过)`);
        continue;
      }
      pushToken(file, value);
      saveState(file, value);
      console.log(`[harvest] ${site.name}/${file}: OK len=${value.length}(已推 NAS,E3 热加载生效)`);
    }
  } catch (e) {
    failures.push(`${site.name}: ${e.message}`);
    console.log(`[harvest] ${site.name}: FAIL ${e.message}`);
  }
}

// ── 失败告警(复用 keeper webhook;状态去重:失败集合未变不重复发)─────
const alertURL = process.env.CREDENTIAL_KEEPER_ALERT_URL || "";
if (failures.length > 0 || fs.existsSync(path.join(STATE_DIR, "last-harvest-failures.txt"))) {
  const key = failures.sort().join("; ");
  const prev = (() => { try { return fs.readFileSync(path.join(STATE_DIR, "last-harvest-failures.txt"), "utf8").trim(); } catch { return ""; } })();
  if (key !== prev && alertURL) {
    const text = failures.length > 0
      ? `[token-harvester] 提取失败: ${failures.join("; ")}`
      : "[token-harvestere] 恢复:全部站点提取成功";
    try {
      await fetch(alertURL, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text }),
        signal: AbortSignal.timeout(10000),
      });
      console.log(`[harvest] alert sent: ${text.slice(0, 120)}`);
    } catch (e) {
      console.log(`[harvest] alert send failed: ${e.message}`);
    }
  }
  fs.writeFileSync(path.join(STATE_DIR, "last-harvest-failures.txt"), key);
}

console.log("[harvest]", new Date().toISOString(), "done");
process.exit(failures.length ? 1 : 0);
