// 零依赖 CDP over WebSocket 客户端(供 Node 内核 / 命令行使用)
//
// 用途:接管带 --remote-debugging-port 启动的 Chromium/Electron 浏览器
// (如 Min 浏览器),读 cookie/localStorage、抓 Network 请求、驱动页面。
// 用法:
//   import { cdp } from "cdp-helper.mjs";
//   const c = await cdp("ws://127.0.0.1:9222/devtools/page/<id>");
//   await c.cmd("Page.navigate", { url });
//   c.on((m) => { if (m.method === "Network.requestWillBeSent") ... });
// 配套流程:见 docs/CDP_BROWSER_DEBUG.md
import { createConnection } from "node:net";
import { createHash, randomBytes } from "node:crypto";

export async function cdp(wsUrl) {
  const u = new URL(wsUrl);
  const key = randomBytes(16).toString("base64");
  const accept = (k) =>
    createHash("sha1").update(k + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64");

  const sock = await new Promise((res, rej) => {
    const s = createConnection({ host: u.hostname, port: +u.port }, () => res(s));
    s.once("error", rej);
  });

  // HTTP Upgrade 握手
  const req = [
    `GET ${u.pathname} HTTP/1.1`,
    `Host: ${u.host}`,
    "Upgrade: websocket",
    "Connection: Upgrade",
    `Sec-WebSocket-Key: ${key}`,
    "Sec-WebSocket-Version: 13",
    "", "",
  ].join("\r\n");
  sock.write(req);

  let handshake = "";
  await new Promise((res, rej) => {
    const onData = (d) => {
      handshake += d.toString("latin1");
      const idx = handshake.indexOf("\r\n\r\n");
      if (idx >= 0) {
        const head = handshake.slice(0, idx);
        if (!/101/.test(head.split("\r\n")[0])) return rej(new Error("bad handshake: " + head.split("\r\n")[0]));
        if (!head.includes(`Sec-WebSocket-Accept: ${accept(key)}`)) return rej(new Error("bad accept key"));
        sock.off("data", onData);
        res();
      }
    };
    sock.on("data", onData);
    sock.once("error", rej);
  });

  // 帧解析
  let buf = Buffer.alloc(0);
  const listeners = new Set();
  let msgId = 0;
  const pending = new Map();
  sock.on("data", (d) => {
    buf = Buffer.concat([buf, d]);
    for (;;) {
      const f = parseFrame(buf);
      if (!f) break;
      buf = buf.subarray(f.consumed);
      if (f.opcode === 0x1) {
        let m;
        try { m = JSON.parse(f.payload.toString("utf8")); } catch { continue; }
        if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); continue; }
        listeners.forEach((l) => l(m));
      } else if (f.opcode === 0x9) {
        sock.write(buildFrame(0xA, f.payload, true)); // ping → pong
      } else if (f.opcode === 0x8) {
        listeners.forEach((l) => l({ __closed: true }));
      }
    }
  });

  return {
    on(fn) { listeners.add(fn); },
    off(fn) { listeners.delete(fn); },
    sendRaw(obj) { sock.write(buildFrame(0x1, Buffer.from(JSON.stringify(obj)), true)); },
    cmd(method, params = {}) {
      return new Promise((res) => {
        const id = ++msgId;
        pending.set(id, res);
        this.sendRaw({ id, method, params });
      });
    },
    waitEvent(method, timeoutMs = 15000) {
      return new Promise((res, rej) => {
        const t = setTimeout(() => { listeners.delete(h); rej(new Error("timeout waiting " + method)); }, timeoutMs);
        const h = (m) => {
          if (m.method === method) { clearTimeout(t); listeners.delete(h); res(m.params); }
        };
        listeners.add(h);
      });
    },
    close() { try { sock.write(buildFrame(0x8, Buffer.alloc(0), true)); } catch {} sock.destroy(); },
  };
}

function parseFrame(buf) {
  if (buf.length < 2) return null;
  const b0 = buf[0], b1 = buf[1];
  const opcode = b0 & 0x0f;
  const masked = (b1 & 0x80) !== 0;
  let len = b1 & 0x7f;
  let off = 2;
  if (len === 126) { if (buf.length < 4) return null; len = buf.readUInt16BE(2); off = 4; }
  else if (len === 127) { if (buf.length < 10) return null; len = Number(buf.readBigUInt64BE(2)); off = 10; }
  let maskKey = null;
  if (masked) { if (buf.length < off + 4) return null; maskKey = buf.subarray(off, off + 4); off += 4; }
  if (buf.length < off + len) return null;
  let payload = buf.subarray(off, off + len);
  if (maskKey) { const p = Buffer.from(payload); for (let i = 0; i < p.length; i++) p[i] ^= maskKey[i & 3]; payload = p; }
  return { opcode, payload, consumed: off + len };
}

function buildFrame(opcode, payload, mask) {
  let len = payload.length;
  let header;
  if (len < 126) { header = Buffer.from([0x80 | opcode, (mask ? 0x80 : 0) | len]); }
  else if (len < 65536) { header = Buffer.alloc(4); header[0] = 0x80 | opcode; header[1] = (mask ? 0x80 : 0) | 126; header.writeUInt16BE(len, 2); }
  else { header = Buffer.alloc(10); header[0] = 0x80 | opcode; header[1] = (mask ? 0x80 : 0) | 127; header.writeBigUInt64BE(BigInt(len), 2); }
  if (mask) {
    const maskKey = randomBytes(4);
    const masked = Buffer.from(payload);
    for (let i = 0; i < masked.length; i++) masked[i] ^= maskKey[i & 3];
    return Buffer.concat([header, maskKey, masked]);
  }
  return Buffer.concat([header, payload]);
}
