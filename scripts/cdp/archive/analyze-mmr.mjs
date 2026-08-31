// 分析 responses 流式 delta 里的 citation
import fs from "node:fs";
const s = fs.readFileSync("/tmp/mmr.json", "utf8");
// SSE 行解析:response.output_text.delta 事件的 delta 字段
const lines = s.split("\n");
let idx = 0;
for (const line of lines) {
  if (!line.startsWith("data:")) continue;
  try {
    const j = JSON.parse(line.slice(5).trim());
    if (j.type === "response.output_text.delta" && j.delta) {
      idx++;
      const d = String(j.delta);
      let ci = d.indexOf("citation");
      while (ci >= 0) {
        console.log(`delta[${idx}] CTX:`, JSON.stringify(d.slice(Math.max(0, ci - 30), ci + 30)));
        ci = d.indexOf("citation", ci + 1);
      }
    }
    if (j.type === "response.completed") {
      const full = j.response && j.response.output && JSON.stringify(j.response.output) || "";
      let ci = full.indexOf("citation");
      console.log("completed 含 citation:", ci >= 0 ? "YES " + (full.match(/citation/g) || []).length + " 处" : "no");
    }
  } catch {}
}
console.log("delta 总数:", idx);
