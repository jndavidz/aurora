# 02 — 构建 aurora 镜像（WSL 本地）

**What to build:** 在 WSL 侧用主干代码构建 `aurora:local-toolfix` 镜像（本机 Windows 侧无 docker，双端纪律：构建只在 WSL 发起）。

**前置：** 01 完成（代码基线绿）。当前 checkout 必须是 `local-toolfix`（或集成分支 FF 自它）。

**命令要点（WSL 侧）：**
```
cd /mnt/d/_work/repos/aurora
git rev-parse --short HEAD          # 记录基线 commit，写入本 ticket
docker build -t aurora:local-toolfix .
docker images --format '{{.Repository}}:{{.Tag}} {{.ID}} {{.CreatedAt}}' | grep aurora
```

**验收：** 构建成功、镜像 ID 记录到本 ticket；镜像内二进制能启动到读配置阶段（`docker run --rm aurora:local-toolfix -h` 或等价探针，不连 NAS）。

**回退：** 无（不改变任何运行环境；失败仅留下本地镜像层，可 `docker rmi` 新层）。

**Status:** pending

**Blocked by:** 01
