#!/bin/bash
# audio-aware-ml.sh —— 音质保护：检测 squeezelite 播放，动态调整 Immich ML 的 cpuset
# 播放中（/proc/asound RUNNING）→ ML 限到 cpu1,3（让出核 0 给音频）
# 空闲 → 恢复全核 0-3（ML 全速）
# 2026-08-31 部署。由 systemd 常驻（audio-aware-ml.service），每 10s 检查一次。
# ML 容器未运行时 docker update 静默失败，无副作用。
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

ML=immich_machine_learning
STATUS=/proc/asound/card0/pcm0p/sub0/status

while true; do
  if grep -q RUNNING "$STATUS" 2>/dev/null; then
    docker update --cpuset-cpus="1,3" "$ML" >/dev/null 2>&1
  else
    docker update --cpuset-cpus="0-3" "$ML" >/dev/null 2>&1
  fi
  sleep 10
done
