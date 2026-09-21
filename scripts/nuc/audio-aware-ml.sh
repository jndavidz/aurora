#!/bin/bash
# audio-aware-ml.sh —— 音质保护：检测播放状态，动态调整 Immich ML 的 cpuset
# 播放中（/proc/asound RUNNING）→ ML 限到 cpu1,3（让出核 0 给音频）
# 空闲 → 恢复全核 0-3（ML 全速）
# 2026-08-31 部署。由 systemd 常驻（audio-aware-ml.service），每 10s 检查一次。
# ML 容器未运行时 docker update 静默失败，无副作用。
#
# 2026-09-21 改（DAC 到货切换）：音频输出改走 USB DAC(Topping DX3 Pro+ = ALSA card "Pro")。
# 原实现硬编码 card0（板载 ALC283）→ 换 DAC 后永远读不到 RUNNING，
# 播放感知静默失效 → ML 不再让核 → 可能爆音。
# 改用声卡【名字】路径（cardN 会随 USB 枚举顺序漂移：本次 DAC=card1，HDMI 被挤到 card2），
# 并同时监测板载卡作为回退（DAC 未接时仍能保护板载播放）。
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

ML=immich_machine_learning
# 按名字访问，避免卡号漂移；两项任一 RUNNING 即视为正在播放
STATUSES=(
  /proc/asound/Pro/pcm0p/sub0/status      # USB DAC（当前主路径）
  /proc/asound/PCH/pcm0p/sub0/status      # 板载 HDA（回退）
)

playing() {
  local f
  for f in "${STATUSES[@]}"; do
    grep -q RUNNING "$f" 2>/dev/null && return 0
  done
  return 1
}

while true; do
  if playing; then
    docker update --cpuset-cpus="1,3" "$ML" >/dev/null 2>&1
  else
    docker update --cpuset-cpus="0-3" "$ML" >/dev/null 2>&1
  fi
  sleep 10
done
