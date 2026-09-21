#!/bin/bash
# pin-audio-irq.sh —— 把音频中断绑定到 cpu0（与 squeezelite 的 CPUAffinity=0 同核）
# 动态查找 IRQ 号（PCI MSI 号重启后可能变化，不能硬编码）
# 2026-08-31 部署。由 systemd oneshot（audio-irq-affinity.service）开机执行。
#
# 2026-09-21 改（DAC 到货切换）：音频输出已从板载 HDA(ALC283) 切到 USB DAC
# (Topping DX3 Pro+)。USB 音频的中断不在 snd_hda_intel，而在其上游 USB 控制器
# (xhci_hcd) —— 只绑 card0 对新链路完全无效（静默失效）。
# 现同时绑定两者：xhci_hcd = 当前生效路径；snd_hda_intel:card0 = 板载回退路径。
# 二者缺一不算错（设备可能不存在），故逐项判断、不因单项缺失而失败。
#
# 用名字而非卡号：/proc/interrupts 里 HDA 的标识是 "snd_hda_intel:card0"
# (ALC283 板载在 NUC 上恒为 card0)；可加 -e /proc/asound 校验。
set -u

pin() {
  local pattern="$1" label="$2"
  local irq
  irq=$(grep -E "$pattern" /proc/interrupts 2>/dev/null | awk -F: '{print $1}' | tr -d ' ' | head -1)
  if [ -n "$irq" ] && [ -w "/proc/irq/$irq/smp_affinity" ]; then
    echo 1 > "/proc/irq/$irq/smp_affinity" && \
      echo "pinned $label IRQ $irq to cpu0 (affinity=1)"
  else
    echo "WARN: $label IRQ not found (pattern: $pattern)" >&2
  fi
}

pin "xhci_hcd"             "USB(xhci) audio path"
pin "snd_hda_intel:card0"  "onboard HDA"
