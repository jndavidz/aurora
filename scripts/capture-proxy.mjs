// 日志反向代理:捕获客户端(pi 等)发给模型网关的完整请求体并转发。
//
// 用途:看客户端往上下文里注入了什么(系统提示/工具定义/技能等)。
// 支持两种转发模式:
//   A) 单上游: node capture-proxy.mjs <listenPort> <upstreamBase> [logFile]
//   B) 按 apiKey 路由(多 provider): node capture-proxy.mjs <listenPort> --map <mapping.json> [logFile]
//      mapping.json: {"<apiKey>": "https://upstream1/v1", "<apiKey2>": "https://upstream2/v1"}
//      客户端(pi)的每个 provider 都指向本代理,代理按 Authorization 头路由到真实上游。
import http from "node:http";
import https from "node:https";
import { appendFileSync, readFileSync } from "node:fs";

const [port, modeOrUpstream, arg2, logFileArg] = process.argv.slice(2);
if (!port || !modeOrUpstream) {
  console.error("usage: node capture-proxy.mjs <listenPort> <upstreamBase> [logFile]");
  console.error("   or: node capture-proxy.mjs <listenPort> --map <mapping.json> [logFile]");
  process.exit(1);
}
const logFile = logFileArg || "/tmp/capture.log";
let byKey = null;
if (modeOrUpstream === "--map") {
  byKey = JSON.parse(readFileSync(arg2, "utf8"));
  console.log(`routing by apiKey: ${Object.keys(byKey).length} providers`);
} else {
  console.log(`single upstream: ${modeOrUpstream}`);
}

function resolveUpstream(authHeader) {
  if (byKey) {
    const key = (authHeader || "").replace(/^Bearer\s+/i, "").trim();
    const up = byKey[key];
    if (!up) {
      return { error: `no upstream for apiKey ${key.slice(0, 8)}...` };
    }
    return { upstream: up };
  }
  return { upstream: modeOrUpstream };
}

const server = http.createServer((req, res) => {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    const body = Buffer.concat(chunks).toString("utf8");
    const entry = {
      ts: new Date().toISOString(),
      method: req.method,
      path: req.url,
      headers: req.headers,
      body,
    };
    try { appendFileSync(logFile, JSON.stringify(entry) + "\n", "utf8"); } catch (e) {}
    console.log(`[${new Date().toISOString()}] ${req.method} ${req.url} body=${body.length}B`);

    const { upstream, error } = resolveUpstream(req.headers.authorization);
    if (error) {
      console.error("  " + error);
      res.writeHead(502, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error }));
      return;
    }
    if (body.length > 0 && body.length < 4000) {
      console.log("  body: " + body.slice(0, 4000));
    } else if (body.length >= 4000) {
      console.log("  body (first 800): " + body.slice(0, 800) + " ...");
    }

    // 转发到上游(https 上游用 https.request)
    const u = new URL(upstream + req.url);
    const headers = { ...req.headers };
    delete headers.host;
    const transport = u.protocol === "https:" ? https : http;
    const fwd = transport.request(
      { hostname: u.hostname, port: u.port, path: u.pathname + u.search, method: req.method, headers },
      (upRes) => {
        res.writeHead(upRes.statusCode, upRes.headers);
        upRes.pipe(res); // SSE 流式直通
      }
    );
    fwd.on("error", (e) => { console.error("forward error:", e.message); res.writeHead(502); res.end(); });
    fwd.write(body);
    fwd.end();
  });
});

server.listen(+port, "127.0.0.1", () => {
  console.log(`capture proxy listening on 127.0.0.1:${port}, log: ${logFile}`);
});
