# NUC 资源占有与 LMS 影响分析（实测版）

> 日期：2026-08-31 ｜ **本版为 SSH 实测数据，取代此前的推算版**
> 实测对象：NUC `10.10.10.3`（hostname `nuc-hifi`，root 公钥登录）｜ NAS `10.10.10.2`（`zxsadmin`）
> NUC IP 经主路由 `10.10.10.1` 的 DHCP 租约定位（静态租约，MAC `c0:3f:d5:66:6b:20`）

---

## 〇、结论先行

| 问题 | 结论 |
|---|---|
| **内存够吗？** | **充裕**。已用 3.4GB / 15.9GB（21%），新增 Chrome ~1GB 后约 28%，余量充足 |
| **CPU 够吗？** | **够用**。当前负载 0.08，squeezelite 仅占 0.1%，余量极大 |
| **白天影响 LMS 吗？** | **≈ 0**。且实测发现 NUC 上**只有 squeezelite 一个实时进程**，风险远低于此前估算 |
| **夜间影响吗？** | **0**（无音频活动） |
| **总体风险** | **低**。实测推翻了此前的三条悲观假设 |

**一句话**：实测后风险等级从「中」降到「**低**」。此前担心的磁盘 I/O、网络抖动、缓冲过小三个问题**均不存在**；唯一新发现是需要把 Immich ML 也纳入 CPU 亲和性管理——它当前正在核 0 的 HT 线程上运行。

---

## 一、实测数据

### 1.1 NUC 硬件（`nuc-hifi`，10.10.10.3）

| 项 | 实测值 | 备注 |
|---|---|---|
| CPU | **i3-4010U @ 1.70GHz** | 2 核 4 线程 |
| 频率 | min 800 / **max 1700 MHz** | **无睿频**（max = base），确认此前判断 |
| 频率策略 | **`performance`** | ✅ 固定满频，无升降频延迟抖动，有利音频 |
| 当前频率 | cpu0–3 均 **1695–1696 MHz** | 满频运行 |
| 内存 | 15892 MB，已用 3473 MB，**可用 12418 MB** | 16GB 满配 |
| 磁盘 | **SanDisk SD7SF6S256G，238.5G，ROTA=0（SSD）** | ✅ 无 HDD |
| 网络 | **eno1 有线** 10.10.10.3/24 | ✅ 非 WiFi |
| 负载 | load 0.08 / 0.02 / 0.01 | 极低 |
| uptime | 1 天 19 小时 | 稳定 |

### 1.2 进程实况

| 进程 | CPU | 内存 | 运行核 | 说明 |
|---|---|---|---|---|
| **squeezelite** | 0.1% | **54 MB** | **cpu0** | 极轻量 |
| Immich ML（python ×3） | 0.1% | **2822 MB** | **cpu2 / cpu3** | ⚠️ 见 §3.1 |
| dockerd / containerd | ~0% | ~178 MB | — | — |

**容器**：`immich_machine_learning`（`ghcr.io/immich-app/immich-machine-learning:v3`），Up 28 hours (healthy)。

### 1.3 squeezelite 实际配置（关键）

```
/usr/bin/squeezelite -n NUC-HiFi -s 10.10.10.2 -o hw:CARD=PCH,DEV=0 \
                     -a 80 4  -b 8192 16384  -m 00:00:00:00:00:00 -C 5
```

| 参数 | 值 | 解读 |
|---|---|---|
| `-s 10.10.10.2` | **LMS 服务器 = NAS** | ⚠️ **LMS 不在 NUC 上**（见 §2.1） |
| `-o hw:CARD=PCH,DEV=0` | ALC283 Analog（**板载声卡**） | ⚠️ 当前未走 DAC（见 §4.1） |
| `-b 8192 16384` | 流缓冲 **8192 KB (8MB)** / 输出缓冲 16384 KB | ✅ **缓冲极大**（见 §2.2） |
| `-a 80 4` | ALSA 缓冲 80 / 周期 4 | 配合上层大缓冲，合理 |
| `-C 5` | 空闲 5 秒关闭输出 | 省电 |

### 1.4 音频链路真实拓扑

```
NAS (10.10.10.2)                          NUC (10.10.10.3)
├── /volume2/docker/lyrion/  ← LMS        ├── squeezelite (54MB, 0.1%)
│   （Lyrion = LMS 新版）                 │   └─ 输出 hw:PCH,DEV=0 (ALC283 板载)
├── /volume1/music  8.9GB / 1330 首       │
│   （HDD RAID1）                          └── immich_machine_learning (2.8GB)
└── /volume2/docker/aurora/  ← aurora 源码（容器当前未运行，见 §4.2）
        ──── 网络（有线，LMS 推流）────►
```

---

## 二、实测推翻的三条假设

### 2.1 ❌ 假设「LMS 与 squeezelite 都在 NUC 上」→ **错误**

**实测**：`-s 10.10.10.2` 表明 **LMS 在 NAS 上**（`/volume2/docker/lyrion/`，Lyrion 是 LMS 2024 年后的新名）。NUC 上**只有 squeezelite 播放器**。

**影响（风险大幅降低）**：
- NUC 上需保护的实时进程**只有 squeezelite 一个**（54MB / 0.1% CPU）
- 此前担心的「LMS rescan 争抢 NUC 核 0」**不成立**——rescan 发生在 NAS 上
- LMS rescan 若导致推流短暂卡顿，squeezelite 的大缓冲（见下）足以吸收

### 2.2 ❌ 假设「squeezelite 缓冲偏小，需增大」→ **错误，已配置得很好**

**实测**：`-b 8192 16384` = 流缓冲 **8 MB**、输出缓冲 **16 MB**。

**换算**：CD 品质（44.1kHz/16bit 立体声 ≈ 176 KB/s）
- 8 MB ÷ 176 KB/s ≈ **46 秒**

即便 24/192 高解析（≈ 1.15 MB/s）也有约 **7 秒**。**抗突发能力极强**。

**结论**：此前"增大缓冲"的建议**不必要，应撤回**。任何数秒内的 CPU 突发都不会触及 46 秒的缓冲底线。

### 2.3 ❌ 假设「磁盘 I/O 与网络存在竞争风险」→ **均不成立**

| 风险 | 实测 | 结论 |
|---|---|---|
| 磁盘 I/O 竞争 | NUC 是 **SSD**，且 **不直接读音乐文件**（从 LMS 流式接收）；音乐库在 NAS 的 HDD 上 | **风险归零** |
| 网络抖动 | NUC **有线**连接 | **风险消除** |
| 音乐库与图片库同盘 | 不在同一台机器（音乐在 NAS，ML 在 NUC） | **不适用** |

---

## 三、实测发现的新问题

### 3.1 ⚠️【最关键】声卡中断与 Immich ML 争抢同一个 HT 线程

这是本次实测**最有价值的发现**——比「ML 占用 cpu2」的表述精确得多，也严重得多。

**`/proc/interrupts` 实测**：

```
49:    0      0    228      0   IR-PCI-MSI-0000:00:1b.0  snd_hda_intel:card0
     cpu0   cpu1  cpu2   cpu3
43:    2      0      0      0   xhci_hcd（USB 3.0 控制器）
```

- **IRQ 49（板载声卡）的 228 次中断，全部落在 `cpu2`**；亲和性掩码 `f`（未绑定，内核随机放置）
- Immich ML 的 python 进程也跑在 **cpu2 / cpu3**
- ⇒ **声卡中断处理 与 ML 批处理 直接争抢 cpu2**（核 0 的 HT 兄弟）

**这是当前就存在的真实风险**（非新架构引入）。而且比"进程争抢 CPU"更严重——**声卡中断是硬中断，优先级高于任何进程**；中断被延迟会直接导致音频缓冲欠载。

**拓扑**：i3-4010U 双核四线程 → 物理核 0 = `cpu0`+`cpu2`（HT）｜物理核 1 = `cpu1`+`cpu3`（HT）。

**补充**：squeezelite 本身**未设亲和性**，实测在 cpu0 ↔ cpu1 之间漂移，也应显式绑定。

**处置**（零成本，优先级最高）：

```bash
# ① 声卡中断绑定到 cpu0（与 squeezelite 同核，减少跨核通信）
echo 1 > /proc/irq/49/smp_affinity
```

```ini
# ② squeezelite（systemd unit）
CPUAffinity=0
Nice=-10

# ③ Immich ML 启动配置（compose，每次手动启动自动生效）
cpuset: "1,3"
CPUQuota=80%
```

⇒ 结果：**核 0 成为纯音频核**（进程 + 中断同核），核 1 承载全部批处理。

**三点注意**：

1. **Immich ML 是手动启动的闲时任务**（用户澄清：当前在跑是因正在测试），**非常驻** ⇒ 绑核写进**启动配置**即可，不必按常驻服务处理。
2. ✅ **`irqbalance` 未运行** —— IRQ 不会被动态迁移，手动设置的亲和性可保持。**建议保持关闭**（若启用会覆盖手动设置）。
3. ⚠️ **`/proc/irq/*/smp_affinity` 重启失效**，需 systemd oneshot 或 udev 规则持久化。

### 3.2 ✅ 意外利好：CPU 固定 performance 策略

governor = `performance`，cpu0–3 全部锁定 1695+ MHz。

**意义**：无升降频切换延迟——这正是音频实时性的隐性杀手（频率切换会造成数十微秒级抖动）。当前配置对音频**有利**，不应改为 powersave / ondemand。

### 3.3 输出设备为板载 ALC283，非外部 DAC

`-o hw:CARD=PCH,DEV=0` 指向 **ALC283 Analog**（NUC 板载声卡），`aplay -l` 也仅列出 PCH 与 HDMI，**无 USB DAC 设备**。

**这与「LMS HiFi 输出至 DAC」的认知不符**，详见 §4.1。

---

## 四、两个需要你确认的问题

### 4.1 音频输出：当前走的是板载声卡，不是 DAC

预期链路是 `squeezelite → DAC → Onkyo TX-SR674 → Tannoy Arena 5.1`，但实测：
- 输出设备 = `hw:CARD=PCH,DEV=0` = **ALC283 板载模拟输出**
- 系统里**没有识别到任何 USB DAC**

可能情况：① DAC 未连接 / 未开机 ② DAC 走光纤/同轴且未启用 ③ 有意使用板载输出到功放（但音质会受限）

**这不是本次架构的问题，但直接影响音质**，建议确认。

### 4.2 ✅ NAS Docker 已恢复 —— 原为临时异常，用户已重启解决

```
07:57 实测  docker.sock 存在但无 dockerd 进程 → aurora 离线
08:07 复测  Docker daemon 已恢复，19 个容器全部在线
```

**用户确认是临时异常，已重启解决。** 复测结果：

| 容器 | 状态 |
|---|---|
| **aurora** | ✅ **Up**（约 1 分钟） |
| **lyrion**（LMS） | ✅ Up |
| immich_server / immich_postgres / immich_redis | Up（server 一度 `unhealthy`，系重启后 healthcheck 未过，通常自动恢复） |
| homeassistant / ha-mariadb / ha-nodered | Up |
| kugou-api / ncm-api / xiaomusic / musicdl-api | Up |
| open-xiaoai-bridge / wecom-assistant / openlist / homebox / epg / dpanel / cups_olbat | Up |

**仍建议确认**：ContainerManager 是否设为**开机自启**。作为其他项目的基础设施，意外停机影响面很大——aurora 离线即上层 ZCode / pi / codebuddy 全部不可用，而这本可由开机自启避免。

**附带发现**：agent.md 记录的 docker 路径 **`/usr/local/bin/docker` 已过时**，实际为
`/var/packages/ContainerManager/target/usr/bin/docker`（DSM 7.2 起 ContainerManager 取代 Docker 套件）——建议同步更新 agent.md。

> 需确认：是刚重启尚未自启，还是套件未设开机自启？若属后者，**这比本项目任何重构都更该优先解决**。

---

## 五、修正后的资源账本

| 指标 | 当前实测 | 新增 Chrome CDP 桥后 | 余量 |
|---|---|---|---|
| **内存** | 3.4 GB / 15.9 GB（21%） | ~4.4 GB（**28%**） | ✅ 充足 |
| **CPU（白天）** | load 0.08，核 0 仅 squeezelite 0.1% | 核 1 增加 Chrome 空闲 ≈0% | ✅ 充足 |
| **核 0 占用** | squeezelite 0.1%（+ Immich ML 误占 cpu2） | 修正后仅 squeezelite | ✅ 极空闲 |
| **磁盘 I/O** | NUC 不读音乐文件，本地仅 SSD | 无新增压力 | ✅ 无风险 |
| **网络** | 有线，音频流 <10 Mbps | 新增 <1 Mbps | ✅ 无风险 |

**新增负载对 LMS 的净影响：≈ 0**（squeezelite 独占核 0 + 46 秒缓冲 + SSD + 有线，四重保障）

---

## 六、修正后的建议（按优先级）

| 优先级 | 建议 | 理由 | 成本 |
|---|---|---|---|
| **P0** | **把 Immich ML 绑定到 `cpu1, cpu3`** | 它当前占用 cpu2（核 0 的 HT），是与音频争抢的**现存隐患** | 低（一个配置） |
| **P0** | **确认 NAS Docker 未启动的原因** | aurora 作为基础设施当前不在线 | 低 |
| **P0** | **确认音频输出设备**（板载 vs DAC） | 直接影响音质，与架构无关 | — |
| **P1** | 新增 Chrome 绑定 `cpu1, cpu3` + `CPUQuota=80%` + `MemoryMax=2G` | 与新方案一致 | 低 |
| **P1** | 需浏览器的保活动作排入 **22:00–24:00** 窗口 | 时间隔离最彻底 | 低 |
| **P2** | 保持 CPU governor 为 `performance` | 避免频率切换抖动 | 零 |

**已撤回的建议**：~~增大 squeezelite 缓冲~~（实测已是 8MB ≈ 46 秒，无需调整）｜~~LMS rescan 夜间化（针对 NUC）~~（rescan 发生在 NAS 上，不争抢 NUC；若 NAS 上 rescan 影响推流，46 秒缓冲足以吸收）

---

## 七、仍需实测验证的项

| # | 验证项 | 方法 | 通过标准 |
|---|---|---|---|
| 1 | **调度延迟** | `cyclictest -p 80 -t 1 -n -i 1000 -l 100000`，对比「仅音频」与「音频 + Chrome 满载」 | Max latency 无显著恶化 |
| 2 | **缓冲欠载** | squeezelite 日志 / output underrun 统计 | 零 underrun |
| 3 | **主观听音** | 播放熟悉曲目 + 触发 CDP 请求 | 无可闻异常 |
| 4 | **白天 CDP 响应延迟** | 白天发起 Gemini/Claude 请求测端到端耗时 | 可接受（批处理仅 1 核，需确认不过慢） |
| 5 | **Immich ML 繁忙时行为** | ML 批量处理时观察 CPU 占用与音频是否受影响 | 绑定 cpu1,3 后无影响 |

> 验证 1–3 是阶段 D 上线门槛；验证 4–5 为新增项。有了 46 秒缓冲的实测结论，这几项的通过概率显著提升。

---

## 附：与推算版的差异对照

| 项 | 推算版 | 实测版 |
|---|---|---|
| LMS 位置 | 假设在 NUC | **在 NAS（lyrion）** |
| squeezelite 缓冲 | 假设默认偏小 | **8MB（≈46 秒），已很充裕** |
| 磁盘 I/O 风险 | 中（待确认） | **无风险**（SSD + 不读音乐文件） |
| 网络风险 | 中（待确认） | **无风险**（有线） |
| Immich ML | "闲时"任务 | **常驻 2.8GB，且误占 cpu2** |
| 内存基线 | 估算 1.5–2.5GB | **实测 3.4GB**（含 ML 2.8GB） |
| 总体风险 | 中 | **低** |
