#!/usr/bin/env bash
# NAS 一键部署 aurora(local-toolfix 本地构建镜像)
# 参考 kugou_api/docs/nas-build-guide.md 的 deploy 范式
#
# 流程(构建在 WSL 本地,不再在 NAS 上 build):
#   1) WSL 本地 docker build 出 aurora:local-toolfix 镜像
#   2) docker save | ssh | docker load 推送到 NAS
#   3) NAS 侧只准备 compose/tokens,然后 docker compose up -d(复用已 load 的本地镜像)
#   4) curl /v1/models 探活
#
# 用法:cd /d/repos/aurora && ./scripts/deploy_nas.sh
set -euo pipefail

# ── 配置(按本机 AGENTS.md 网络拓扑)──────────────────────────
NAS_HOST=zxsadmin@10.10.10.2          # SSH 免密,密钥 D:\dev\data\ssh\id_ed25519
DEPLOY_DIR=/volume2/docker/aurora      # 部署目录(非 Drive 同步区)
TOKEN_SRC=/volume2/dev/apps/aurora/.runtime/tokens  # Drive 同步过来的 token 源
# NAS 上 docker 可执行文件(DSM 7.2 起 ContainerManager 取代 Docker 套件)
DOCKER_CANDIDATES="/var/packages/ContainerManager/target/usr/bin/docker /usr/local/bin/docker"
AUTH=david                            # 与 compose 内 Authorization 一致
NAS_IP=10.10.10.2
PORT=65432   # compose 映射 65432→8080(容器内 8080,宿主 65432)
IMAGE=aurora:local-toolfix
# WSL 本地 docker build 的 buildx 状态目录(~/.docker 偶发权限错乱,独立目录规避)
export DOCKER_CONFIG=${DOCKER_CONFIG:-/tmp/aurora-docker-cfg}
# ────────────────────────────────────────────────────────────

# 定位仓库根(脚本可在任意目录调用)
REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

SSH="ssh -o BatchMode=yes $NAS_HOST"

# NAS 侧 docker 探测
DOCKER=""
for d in $DOCKER_CANDIDATES; do
  $SSH "[ -x '$d' ]" 2>/dev/null && DOCKER="$d" && break
done
[ -z "$DOCKER" ] && { echo "✗ NAS 上找不到 docker 可执行文件"; exit 1; }
echo "==> NAS docker: $DOCKER"

# 本地 docker 探测(WSL 侧)
LOCAL_DOCKER=""
if command -v docker >/dev/null 2>&1; then
  LOCAL_DOCKER=docker
elif [ -x /usr/bin/docker ]; then
  LOCAL_DOCKER=/usr/bin/docker
fi
[ -z "$LOCAL_DOCKER" ] && { echo "✗ 本机(WSL)找不到 docker,无法本地构建"; exit 1; }
echo "==> 本地 docker: $LOCAL_DOCKER (DOCKER_CONFIG=$DOCKER_CONFIG)"

echo -e "\033[36m==> 1/5 本地 docker build $IMAGE\033[0m"
$LOCAL_DOCKER build -t "$IMAGE" -f Dockerfile \
  --build-arg GOPROXY=https://goproxy.cn,direct . \
  || { echo "✗ 本地构建失败"; exit 1; }

echo -e "\033[36m==> 2/5 推送镜像到 NAS(docker save | load)\033[0m"
$LOCAL_DOCKER save "$IMAGE" | $SSH "$DOCKER load" \
  || { echo "✗ 镜像推送失败"; exit 1; }
echo "   镜像已 load 到 NAS"

echo -e "\033[36m==> 3/5 准备 NAS 部署目录(compose + tokens)\033[0m"
# 只传 compose 文件(源码已编进镜像,无需整包传);清空旧部署源码(保留 tokens/logs/tokens-state)
# 注:token 更新走 PC 侧 scripts/keeper/push-tokens-from-pc.sh(WSL)与 NUC doubao-hook,
#     不随 deploy 下发(2026-09-05 修正:原"同步区→部署区"脚本前提不成立,同步区是死水)
tar -czf - docker-compose.nas.yml \
  | $SSH "
set -e
mkdir -p $DEPLOY_DIR/tokens $DEPLOY_DIR/logs $DEPLOY_DIR/tokens-state
cd $DEPLOY_DIR
# 清空旧源码/compose(保留 tokens logs tokens-state)
find . -mindepth 1 -maxdepth 1 ! -name tokens ! -name logs ! -name tokens-state -exec rm -rf {} +
# 首次或 token 缺失:从同步区拷入独立副本
if [ ! -s tokens/session_tokens.txt ]; then
  echo '  tokens 缺失,从 $TOKEN_SRC 拷入'
  cp $TOKEN_SRC/*.txt tokens/ 2>/dev/null || true
  chmod 644 tokens/*.txt 2>/dev/null || true
fi
# A2:GLM/Kimi 池迁入 tokens-state(rw,轮换回写);已有则保留
for f in glm_tokens.txt kimi_tokens.txt; do
  if [ ! -s \"tokens-state/\$f\" ] && [ -s \"tokens/\$f\" ]; then
    cp \"tokens/\$f\" \"tokens-state/\$f\"
    echo \"  已播种 tokens-state/\$f\"
  fi
done
# A2:tokens-state 属主改为容器 uid 65532(nonroot)
$DOCKER run --rm -v $DEPLOY_DIR/tokens-state:/fix alpine:3.20 chown -R 65532:65532 /fix 2>/dev/null \
  || echo '  ⚠ tokens-state 属主修复失败,池文件回写可能 Permission denied'
chmod 755 $DEPLOY_DIR/tokens-state 2>/dev/null || true
# 解压 compose
tar -xzf -
# compose 文件就位
[ -f docker-compose.nas.yml ] && mv -f docker-compose.nas.yml docker-compose.yml
echo '  已就位到 $DEPLOY_DIR'
"

echo -e "\033[36m==> 4/5 启动容器(复用本地镜像,不构建)\033[0m"
$SSH "cd $DEPLOY_DIR && DOCKER_BUILDKIT=1 $DOCKER compose up -d"

echo -e "\033[36m==> 5/5 等待服务就绪(启动含 token 换发,最长 60s)\033[0m"
RESP=""
for i in $(seq 1 30); do
  sleep 2
  RESP=$(curl -s -m 5 -w '\n[HTTP %{http_code}]' -H "Authorization: Bearer $AUTH" "http://$NAS_IP:$PORT/v1/models" || true)
  case "$RESP" in *'[HTTP 200]'*) break;; esac
done

echo "$RESP" | head -c 800
echo
case "$RESP" in
  *'[HTTP 200]'*)
    echo -e "\033[32m✓ 部署成功,账号池就绪\033[0m"
    ;;
  *)
    echo -e "\033[31m✗ 验证失败,拉取容器日志:\033[0m"
    $SSH "$DOCKER logs --tail 60 aurora" || true
    exit 1
    ;;
esac
