# 03 — 推送镜像到 NAS 并切换 aurora 容器（远程）

**What to build:** 把 02 的镜像推 NAS 并让 aurora 容器跑新镜像，替换 NAS 上的旧镜像。

**前置授权：** 远程状态变更，需用户单独批准（与仓库写入分开）。

**步骤（执行时逐条回读验证）：**
1. **先记回退点**：NAS 上取当前镜像 ID 与容器配置——`docker images --format '{{.Repository}}:{{.Tag}} {{.ID}}'`、`docker inspect aurora --format '{{.Image}}'`、compose 文件拷贝备份（带时间戳）。
2. `bash scripts/deploy_nas.sh`（WSL 侧：`docker save | ssh | docker load` → NAS `compose up -d`，脚本内含探活）。
3. 回读：`curl -s http://10.10.10.2:65432/ping`；`curl -s http://10.10.10.2:65432/v1/models` 含 `deepseek-v4-flash`；容器 `Up` 且日志无反复重启。
4. **单发通道健康**（真人节奏、间隔 ≥2.5s、只发一条）：经 aurora 调 `deepseek-v4-flash`，确认无 TLS 握手失败、无 403/风控拒绝、首 delta 正常返回。

**验收：** 上述四项全过；镜像 ID 与基线 commit 记录到本 ticket。

**回退：** 用第 1 步备份恢复旧镜像 tag + `compose up -d`；若仅新 TLS 栈被拦，优先试 `AURORA_LEGACY_IDENTITY=1`（DeepSeek 通道可回退 Go 原生，免重建）。

**Status:** pending

**Blocked by:** 02
