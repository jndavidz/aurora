#!/usr/bin/env bash
# NAS 一键部署 aurora(local-toolfix 本地构建镜像)
# 参考 kugou_api/docs/nas-build-guide.md 的 deploy 范式
#
# 流程:tar 打包源码 → ssh 到 NAS → 清空部署目录(保留 tokens/logs)→ 解压
#       → docker compose up -d --build → curl /v1/models 探活
#
# 用法:cd /d/repos/aurora && ./scripts/deploy_nas.sh
set -euo pipefail

# ── 配置(按本机 AGENTS.md 网络拓扑)──────────────────────────
NAS_HOST=zxsadmin@10.10.10.2          # SSH 免密,密钥 D:\dev\data\ssh\id_ed25519
DEPLOY_DIR=/volume2/docker/aurora      # 构建上下文(非 Drive 同步区)
TOKEN_SRC=/volume2/dev/apps/aurora/.runtime/tokens  # Drive 同步过来的 token 源
# docker 可执行文件自动探测(DSM 7.2 起 ContainerManager 取代 Docker 套件,
# /usr/local/bin/docker 已失效——2026-08-31 实测确认;探测在 SSH 定义后执行)
DOCKER_CANDIDATES="/var/packages/ContainerManager/target/usr/bin/docker /usr/local/bin/docker"
AUTH=david                            # 与 compose 内 Authorization 一致
NAS_IP=10.10.10.2
PORT=65432   # compose 映射 65432→8080(容器内 8080,宿主 65432)
# ────────────────────────────────────────────────────────────

# 定位仓库根(脚本可在任意目录调用)
REPO=$(git rev-parse --show-toplevel)
cd "$REPO"

SSH="ssh -o BatchMode=yes $NAS_HOST"

# docker 探测(需 SSH 可用后执行)
DOCKER=""
for d in $DOCKER_CANDIDATES; do
  $SSH "[ -x '$d' ]" 2>/dev/null && DOCKER="$d" && break
done
[ -z "$DOCKER" ] && { echo "✗ NAS 上找不到 docker 可执行文件"; exit 1; }
echo "==> NAS docker: $DOCKER"

echo -e "\033[36m==> 1/4 打包源码并传输(tar,排除敏感/无关项)\033[0m"
tar -czf - \
  --exclude=.git --exclude=.zcode --exclude=.claude --exclude=.idea --exclude=.vscode \
  --exclude=.github --exclude=.env --exclude='.env.*' \
  --exclude='*.log' --exclude='*.pid' --exclude='*.seed' --exclude='*.har' --exclude='*.exe' \
  --exclude=.runtime --exclude=bin --exclude=dist --exclude=target --exclude=_scratch \
  --exclude='*.test' --exclude='*.out' --exclude='*.prof' --exclude='*.pprof' \
  . | $SSH "
set -e
mkdir -p $DEPLOY_DIR/tokens $DEPLOY_DIR/logs $DEPLOY_DIR/tokens-state
cd $DEPLOY_DIR
# 清空旧源码(保留 tokens/ logs/)
find . -mindepth 1 -maxdepth 1 ! -name tokens ! -name logs ! -name tokens-state -exec rm -rf {} +
# 首次或 token 缺失:从同步区拷入独立副本
if [ ! -s tokens/session_tokens.txt ]; then
  echo '  tokens 缺失,从 $TOKEN_SRC 拷入'
  cp $TOKEN_SRC/*.txt tokens/ 2>/dev/null || true
  chmod 644 tokens/*.txt 2>/dev/null || true
fi
# A2:GLM/Kimi 池迁入 tokens-state(rw,轮换回写);已有则保留(内含轮换后的新票)
for f in glm_tokens.txt kimi_tokens.txt; do
  if [ ! -s "tokens-state/\$f" ] && [ -s "tokens/\$f" ]; then
    cp "tokens/\$f" "tokens-state/\$f"
    echo "  已播种 tokens-state/\$f"
  fi
done
# A2:tokens-state 属主改为容器 uid 65532(nonroot)——否则容器内轮换回写
# Permission denied(2026-08-31 实测)。借 root 容器 chown(zxsadmin 无权直改)。
$DOCKER run --rm -v $DEPLOY_DIR/tokens-state:/fix alpine:3.20 chown -R 65532:65532 /fix 2>/dev/null \
  || echo '  ⚠ tokens-state 属主修复失败,池文件回写可能 Permission denied'
chmod 755 $DEPLOY_DIR/tokens-state 2>/dev/null || true
# 解压新源码
tar -xzf -
# compose 文件就位:仓库内 docker-compose.nas.yml → docker-compose.yml
[ -f docker-compose.nas.yml ] && mv -f docker-compose.nas.yml docker-compose.yml
echo '  已解压到 $DEPLOY_DIR'
ls -la $DEPLOY_DIR | head -20
"

echo -e "\033[36m==> 2/4 构建并启动容器(BuildKit 缓存命中则秒级)\033[0m"
$SSH "cd $DEPLOY_DIR && DOCKER_BUILDKIT=1 $DOCKER compose up -d --build"

echo -e "\033[36m==> 3/4 等待服务就绪(启动含 token 换发,最长 60s)\033[0m"
RESP=""
for i in $(seq 1 30); do
  sleep 2
  RESP=$(curl -s -m 5 -w '\n[HTTP %{http_code}]' -H "Authorization: Bearer $AUTH" "http://$NAS_IP:$PORT/v1/models" || true)
  case "$RESP" in *'[HTTP 200]'*) break;; esac
done

echo -e "\033[36m==> 4/4 验证 /v1/models\033[0m"
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
