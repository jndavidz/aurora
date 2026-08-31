#!/bin/bash
# pin-audio-irq.sh —— 把板载声卡（snd_hda_intel:card0）的中断绑定到 cpu0
# 动态查找 IRQ 号（PCI MSI 号重启后可能变化，不能硬编码）
# 2026-08-31 部署。由 systemd oneshot（audio-irq-affinity.service）开机执行。
IRQ=$(grep -E "snd_hda_intel:card0" /proc/interrupts | awk -F: '{print $1}' | tr -d ' ')
if [ -n "$IRQ" ]; then
  echo 1 > "/proc/irq/$IRQ/smp_affinity"
  echo "pinned audio IRQ $IRQ to cpu0 (affinity=1)"
else
  echo "WARN: snd_hda_intel:card0 IRQ not found" >&2
fi
