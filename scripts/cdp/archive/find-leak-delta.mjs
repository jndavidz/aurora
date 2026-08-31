// 找含 citation 的 delta,输出完整上下文
import fs from "node:fs";
const s = fs.readFileSync("/tmp/mmr.json", "utf8");
for (const line of s.split("\n")) {
  if (!line.startsWith("data:")) continue;
  try {
    const j = JSON.parse(line.slice(5).trim());
    const d = j.delta !== undefined ? String(j.delta) : null;
    if (d && d.includes("citation")) {
      console.log("delta 长度:", d.length);
      let i = d.indexOf("citation");
      while (i >= 0 && i < d.length) {
        console.log("前文:", JSON.stringify(d.slice(Math.max(0, i - 150), i)));
        console.log("标记:", JSON.stringify(d.slice(i, i + 20)));
        i = d.indexOf("citation", i + 1);
        if (i >= 0) console.log("---");
      }
      break;
    }
  } catch {}
}
