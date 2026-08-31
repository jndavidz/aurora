// 按 type 精确定位 citation 所在事件
import fs from "node:fs";
const s = fs.readFileSync("/tmp/mmr.json", "utf8");
for (const line of s.split("\n")) {
  if (!line.startsWith("data:")) continue;
  const body = line.slice(5).trim();
  if (!body.includes("citation")) continue;
  try {
    const j = JSON.parse(body);
    // 递归找含 citation 的字段路径
    const find = (o, path, depth) => {
      if (depth > 6 || !o || typeof o !== "object") return;
      for (const k of Object.keys(o)) {
        const v = o[k];
        if (typeof v === "string" && v.includes("citation")) {
          let i = v.indexOf("citation");
          console.log(`[${j.type}] ${path}.${k}:`);
          console.log("  前文:", JSON.stringify(v.slice(Math.max(0, i - 130), i)));
          console.log("  标记+后文:", JSON.stringify(v.slice(i, i + 30)));
        } else if (typeof v === "object") {
          find(v, path + "." + k, depth + 1);
        }
      }
    };
    find(j, "", 0);
  } catch (e) {
    // 非 JSON 行
    console.log("[非JSON行含citation]", line.slice(0, 80));
  }
}
