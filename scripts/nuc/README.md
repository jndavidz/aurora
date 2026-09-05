# NUC 生产配置版本化

NUC(nuc-hifi,10.10.10.3)上所有 systemd 单元与运维脚本的**权威副本**。
2026-08-31 起生效:改 NUC 生产配置必须先改这里、提交,再同步到 NUC——
防止"直接改生产没有记录"的漂移(本日 Chrome 渲染参数即因此产生认知分叉)。

## 文件与部署目标

| 本仓库文件 | NUC 目标路径 | 说明 |
|---|---|---|
| `vnc.service` | `/etc/systemd/system/vnc.service` | Xvfb+openbox+x11vnc;分辨率定稿 **1280x720x24**(2026-08-31 下午,渲染优化;原 2560x1440 软件渲染过慢) |
| `vnc-session.sh` | `/usr/local/bin/vnc-session.sh` | vnc.service 的 ExecStart 脚本 |
| `chrome-cdp.service` | `/etc/systemd/system/chrome-cdp.service` | 桥用 Chrome;含 `--disable-gpu --num-raster-threads=4 --disable-gpu-compositing --disable-dev-shm-usage`(禁 SwiftShader 软件渲染)+ CPUAffinity=1,3 + MemoryMax=2G |
| `aurora-bridge.service` | `/etc/systemd/system/aurora-bridge.service` | 桥常驻;BRIDGE_HOST=0.0.0.0 / IDLE_TIMEOUT_MIN=0 |
| `credential-keeper.service` | `/etc/systemd/system/credential-keeper.service` | D4 凭证探测(oneshot;SuccessExitStatus=1) |
| `credential-keeper.timer` | `/etc/systemd/system/credential-keeper.timer` | 每日 06:30+rand30min |
| `token-harvester.mjs` | `/opt/credential-keeper/token-harvester.mjs` | **G1 统一凭证提取器(2026-09-05)**:CDP 从本机 Chrome 读各站 localStorage/cookie,md5 幂等推 NAS 部署区;站点 minimax/mimo/qianwen/grok(豆包冻结/GLM·Kimi 自愈/DeepSeek 游客票均排除);凭证不入日志 |
| `token-harvester.service` | `/etc/systemd/system/token-harvester.service` | 提取器 oneshot |
| `token-harvester.timer` | `/etc/systemd/system/token-harvester.timer` | 每日 07:00+rand30min(Persistent 补跑) |
| `audio-aware-ml.sh` | `/usr/local/bin/audio-aware-ml.sh` | 播放感知的 ML cpuset 降级(10s 轮询) |
| `pin-audio-irq.sh` | `/usr/local/bin/pin-audio-irq.sh` | 音频 IRQ 绑核(oneshot,动态找 IRQ 号) |
| `squeezelite-affinity.conf` | `/etc/systemd/system/squeezelite.service.d/affinity.conf` | squeezelite CPUAffinity=0 + Nice=-10 |

## 同步命令(仓库 → NUC)

```bash
scp scripts/nuc/vnc.service root@10.10.10.3:/etc/systemd/system/
# ...逐文件,然后:
ssh root@10.10.10.3 'systemctl daemon-reload'
```

## 不入库的 NUC 文件

- `/etc/credential-keeper.env`(600,含网关 token)
- `/root/vnc-password.txt`(600,VNC 密码)
- `/opt/chrome-cdp/profile/`(登录态)、`/opt/aurora-bridge/.runtime/`(桥会话令牌)

## 同步状态基线(2026-09-05)

以上文件均从 NUC 实拉入库或本次新建已同步部署(harvester 三件套已上线并注册 timer);
后续任何 NUC systemd/脚本改动**先改本目录**。

## NUC 文档与脚本全图(2026-08-31 起,防两仓散乱)

NUC(nuc-hifi, 10.10.10.3)的资产分布在两个仓库,分工如下:

| 内容 | 位置 | 角色 |
|---|---|---|
| **生产配置(systemd/运维脚本)** | `aurora/scripts/nuc/`(本目录) | **唯一权威**:装机后一切配置改动先改这里 |
| 裸机装机初始化(nuc-setup.sh: Debian→squeezelite/WOL/CPU) | `open-xiaoai/deploy/nuc/` | 装机一次性;装完即由本目录接管 |
| 音乐入库流水线(ape2flac/metadata/roon) | `open-xiaoai/deploy/nuc/` | 音乐主题,归 open-xiaoai |
| 装机指引 / Roon 部署方案 | `open-xiaoai/doc/plan/nuc-install-guide.md`、`nuc-roon-deploy.md` | 方案文档 |
| 资源实测分析(Chrome 上 NUC 可行性) | `aurora/docs/NUC_RESOURCE_ANALYSIS_2026-08-31.md` | 决策依据 |

**判据**:与 aurora 桥/Chrome/凭证相关的配置 → 本目录;与音乐库/roon 相关 → open-xiaoai;裸机初始化一次性脚本 → open-xiaoai(装完不再改,变更走本目录)。
