# NAS 部署 aurora 方案 — 群晖 DS416play

> 更新时间: 2026-08-12(NAS 内存升级至 **8G** 后复核)
> 关联文档: `D:\dev\apps\aurora\部署说明.md`(PC 端部署,本方案为其姊妹篇)
> 项目: https://github.com/aurora-develop/aurora (ChatGPT 网页端 → OpenAI 兼容 API 网关)

---

## 〇、结论(先说答案)

**可行,无硬性障碍。** NAS 为 x86_64 架构且已具备容器能力,aurora 是静态 Go 单二进制、无数据库,内存升级到 8G 后**内存不再是约束**,Docker 与直接跑二进制两种方式均无压力。

| 维度 | 结论 |
|---|---|
| 架构 | ✅ NAS (Intel Celeron N3060, x86_64) 与本地构建 amd64 镜像 / 自编译 linux 二进制完全匹配 |
| 内存 | ✅ 8G 充裕(实测 PC 上单个服务 RSS 约 40–80MB;docker 容器层约 200MB) |
| 存储 | ✅ 无数据库;token 文件只读即可,账号池纯内存(JSONStore 未接线,已核实) |
| 网络 | ✅ 出网走家庭 AX6000 homeproxy 透明代理;构建走 goproxy.cn,与 PC 同一条路 |
| 推荐方式 | **WSL(PC)本地构建 local-toolfix 镜像 → `docker save` 推送 NAS `load`,NAS 只 `compose up` 复用镜像**(保留全部工具调用修复,且不占 NAS 弱 CPU);自编译二进制为备选 |

---

## 一、NAS 现状(已核实)

| 项目 | 事实 |
|---|---|
| 型号 | 群晖 **DS416play**(Value 系列,2016) |
| CPU | Intel Celeron N3060 双核 1.6GHz(burst 2.48GHz),**x86_64** |
| 内存 | **8G DDR3(2026-08 升级,原 1G)** |
| 容器 | 已装 Docker / Container Manager(证据:`/volume2/@docker`、btrfs、kugou-api 容器经 docker-compose 跑过) |
| 内网 | `10.10.10.2 nas.lan`(家庭 10.10.10.0/24;办公室经 WireGuard 10.99.0.x 可达) |
| 同步 | `/volume2/dev` = `D:\dev` 的 Drive 同步镜像 → `apps/aurora/`、`apps/aurora/.runtime/tokens/` 已在 NAS 上 |
| DSM | 需 DSM 7.2+(Container Manager 前提);以实际套件中心为准 |

---

## 二、项目运行要求(NAS 侧匹配)

| 要求 | aurora 现状 | 备注 |
|---|---|---|
| 运行态 | Go 静态二进制(`CGO_ENABLED=0`),单进程监听 8080 | 无外部依赖 |
| 持久化 | 仅启动时读 token 文件;账号池纯内存 | 无需写盘挂载 |
| 出网 | 访问 `chatgpt.com/backend-api` | 家庭网直连即可 |
| 账号 | `FREE_ACCOUNTS` 默认关(PC 已实证:匿名 UUID 账号最易被 OpenAI 风控) | **建议保持 false**,与内存无关 |
| 鉴权 | `Authorization=david` | 局域网暴露必须带 |

**源码 token 读取逻辑**(`internal/bootstrap/bootstrap.go:tokenFilePath`,local-toolfix 分支):
`.runtime/tokens/` 目录存在 → 从该目录读 token 文件;否则回退当前目录。部署时利用此逻辑即可。
> 本方案方式 A 在 NAS 自构建 local-toolfix 镜像,此逻辑生效——compose 设 `working_dir: /work` + 挂 `/work/.runtime/tokens` 命中隔离目录(见 §3.5)。
> 注:main 分支/官方 `ghcr` 镜像无此函数,仅按 cwd 读扁平文件名;本方案不依赖官方镜像,故不受影响。

---

## 三、部署方式 A:WSL(PC)本地构建 local-toolfix 镜像 → 推 NAS(推荐)

> **[2026-09-02 勘误]** 原文档写"NAS 本地构建",实测改为 **WSL 侧 `docker build` 出镜像,再 `docker save | ssh docker load` 推到 NAS,NAS 侧只 `docker compose up -d`(不带 `--build`)复用已 load 的 `aurora:local-toolfix`**。理由:NAS 为 Celeron N3060 双核,构建慢且 BuildKit 偶发卡顿;WSL 侧(4 核/8G)构建缓存命中秒级,且本机 docker 29.7.2 + go 1.27 工具链齐备(`~/go-sdk/go`,`DOCKER_CONFIG` 指向用户可写目录规避 `~/.docker` 权限坑)。镜像同架构 amd64,save/load 无损。
>
> 在 WSL 用 local-toolfix 分支源码 `docker build` 出镜像,既保留全部工具调用修复,又享容器便利(自启/隔离/日志)。参考 kugou-api 的 `nas-build-guide.md` 同一套路。
>
> **为何不用官方镜像**:`ghcr.io/aurora-develop/aurora:latest` 是 main 分支构建,**不含 local-toolfix 的工具调用修复**,且 main 无 `tokenFilePath` 函数、不认 `.runtime/tokens/` 目录约定。本地构建 local-toolfix 一次解决这两点。

### 3.0 架构概览(三处分离)

| 角色 | 位置 | 说明 |
|---|---|---|
| 代码唯一源头 | PC `D:\repos\aurora`(git,local-toolfix 分支) | 开发/版本管理在此,改完 push 再换机 |
| NAS 部署副本 | `/volume2/docker/aurora` | docker build 构建上下文,**非 Drive 同步区**,不含凭证,每次部署由 deploy.sh 清空重建 |
| 凭证 | `/volume2/docker/aurora/tokens/` | 从同步区 `/volume2/dev/apps/aurora/.runtime/tokens/` 拷入的独立副本,只读挂载 |

> ⚠️ 不要把代码放 `/volume2/dev`(Drive 同步根),否则镜像回 PC `D:\dev` 污染非代码区。
> ⚠️ token 用独立副本目录而非直接挂同步区:避免 Drive 同步重置 ACL 导致容器内 nonroot 读不到(参考 kugou 同坑)。

### 3.1 宿主环境需求(NAS 侧)

| 项 | 实测/要求 | 备注 |
|---|---|---|
| 系统 | DSM 7.2+ | Container Manager 前提 |
| Docker | Container Manager(2026-08-31 勘误:旧记录 `/usr/local/bin/docker` **已失效**;实际 `/var/packages/ContainerManager/target/usr/bin/docker`,默认不在 PATH) | 用全路径;部署脚本已做自动探测 |
| BuildKit | 必须启用(Dockerfile 用了 `--mount=type=cache`) | `export DOCKER_BUILDKIT=1` 或 buildx |
| 操作权限 | `zxsadmin` 在 docker 组 | 免 sudo |
| 磁盘 | <2GB | golang-alpine 构建层 + distroless 运行层 ~30MB + 缓存 |

### 3.2 源码内容(构建上下文)

部署目录 `/volume2/docker/aurora/` 需含(由 deploy.sh 维护):
- `Dockerfile`(多阶段构建,含 `GOPROXY` build arg)、`go.mod`/`go.sum`、`main.go`、`internal/`、`middlewares/`、`api/`、`conversion/`、`httpclient/`、`typings/`、`util/`、`VERSION`
- `docker-compose.nas.yml`(含 `build:` 段,见 §3.5;部署时重命名为 `docker-compose.yml`)、`.dockerignore`(排除 `.git`/`.zcode`/`.env`/`*.log`/`.runtime`/`scripts/`)

排除项(部署副本不含):`.git`、`.zcode`、`.runtime`、`.env`、`*.log`、`bin/`、`dist/`。

### 3.3 网络需求(构建过程)

| 目标 | 用途 | 状态 | 备注 |
|---|---|---|---|
| `goproxy.cn` | Go 模块下载 | ✅ | compose 注入 `GOPROXY=https://goproxy.cn,direct`,国内快 |
| `gcr.io/distroless/static-debian12` | 运行基础镜像拉取 | ⚠️ | 需透明代理(AX6000 homeproxy)或 PC `docker save` 传 NAS `load` |
| github.com | 构建不需要 | — | 仅源码同步可能用到 |

> 构建全程只走 goproxy.cn;distroless 基础层一次性拉取,后续缓存命中无需再拉。

### 3.4 镜像内构建环境(Dockerfile 决定)

| 项 | 值 |
|---|---|
| 构建镜像 | `golang:1.26.0-alpine`(NAS amd64 原生构建,无需交叉编译) |
| 运行镜像 | `gcr.io/distroless/static-debian12:nonroot`(~2MB,无 shell) |
| 二进制 | 静态 `CGO_ENABLED=0`,strip,放 `/aurora` |
| BuildKit 缓存 | `/go/pkg/mod` + `/root/.cache/go-build` 挂 cache 卷;go.sum 不变 → 秒级重建 |
| 默认 cwd | `/`(distroless 无 WORKDIR);compose 设 `working_dir: /work` |

### 3.5 docker-compose.nas.yml

> 仓库内文件名为 `docker-compose.nas.yml`(与官方 `docker-compose.yml` 并存,避免覆盖);deploy_nas.sh 部署时自动重命名为 `docker-compose.yml`。

```yaml
version: '3'

services:
  app:
    image: aurora:local-toolfix        # 本地构建 tag,不用官方 ghcr 镜像
    container_name: aurora
    restart: unless-stopped
    build:                             # ← 必须有 build 段,否则 --build 被静默忽略
      context: .
      dockerfile: Dockerfile
      args:
        GOPROXY: https://goproxy.cn,direct   # 国内构建加速;CI 不传则用默认 proxy.golang.org
    working_dir: /work                  # 命中 local-toolfix 的 ./.runtime/tokens 隔离目录(见 §二)
    ports:
      - '8080:8080'
    environment:
      - SERVER_HOST=0.0.0.0      # 默认即 0.0.0.0(config.go:33),显式声明确保局域网可达
      - SERVER_PORT=8080
      - Authorization=david
      - FREE_ACCOUNTS=false     # 代码默认 false;env.template 写 true 是模板值,生产保持关
      - TOOL_CALLING_ENABLED=true   # local-toolfix 工具调用修复总开关
      - REFUSAL_RETRIES=1       # pi 场景:普通对话不带 <tool_call> 时立即返回,不重试;ZCode 场景才需 5(重试循环见 chat_handler.go handleToolCalling)
      - DEBUG_TOOL_LOG=/work/.runtime/logs/tool_debug.log   # 工具调用 trace,local-toolfix 专属
    volumes:
      - /volume2/docker/aurora/tokens:/work/.runtime/tokens:ro   # token 只读,命中 tokenFilePath 隔离目录
      - /volume2/docker/aurora/logs:/work/.runtime/logs          # 调试日志(可选)
```

> ⚠️ **`build:` 段不可少**:缺了它 `docker compose up -d --build` 会静默用旧镜像,代码改动永不进容器(kugou 同坑,见参考文档第 6 节)。
> distroless 无 shell,`working_dir` 的 `/work` 及子目录由 volume 挂载自动创建挂载点,无需在镜像内预建;token 读路径靠 `tokenFilePath`(`.runtime/tokens` 存在则读隔离目录)。

### 3.6 构建与更新流程

PC(WSL)端一键部署(脚本见仓库 `scripts/deploy_nas.sh`):

```bash
cd /d/repos/aurora && ./scripts/deploy_nas.sh
```

deploy_nas.sh 内部五步:
1. **WSL 本地 `docker build`** 出 `aurora:local-toolfix` 镜像(BuildKit 缓存命中则秒级;用独立 `DOCKER_CONFIG` 规避 `~/.docker` 权限坑);
2. `docker save aurora:local-toolfix | ssh NAS docker load` 推送镜像到 NAS;
3. tar 仅传 `docker-compose.nas.yml` 到 NAS,清空旧部署目录(保留 tokens/logs/tokens-state)并放好 compose;
4. NAS 侧 `docker compose up -d`(**不带 `--build`**,直接复用已 load 的本地镜像);
5. `curl /v1/models` 探活(带 Authorization),确认账号池非空、token 命中。

> 正常更新:go.sum 不变 → WSL 构建缓存命中秒级,推镜像+重启约 10–20s;依赖变更 → 2-3 分钟(goproxy.cn)。
> 手动(无脚本):WSL `docker build -t aurora:local-toolfix .` → `docker save aurora:local-toolfix | ssh NAS docker load` → NAS `docker compose up -d`。

### 3.7 验证

```bash
curl -s -H "Authorization: Bearer david" http://10.10.10.2:8080/v1/models
# 期望:返回 model 列表(账号池非空);docker logs aurora 无 "no accounts" 类错误
```

### 3.8 已知坑与故障排查

1. **compose 缺 `build:` 段**(最隐蔽):`--build` 不报错、容器照常 Running,但镜像从不重建。改动后验证 `docker image inspect aurora:local-toolfix` 的 `Created` 时间已刷新。
2. **BuildKit 未启用**:`--mount=type=cache` 报错;`export DOCKER_BUILDKIT=1` 或用 `docker buildx build`。
3. **运行基础镜像拉不下**(早期文档写 gcr.io/distroless,需代理):**当前 `docker-compose.nas.yml` 运行基已改为 `alpine:3.20`**(见 Dockerfile `RUNTIME_BASE`),alpine 在 NAS/PC 均可直拉,无 distroless 代理坑。若日后切回 distroless,兜底——PC `docker pull gcr.io/distroless/static-debian12:nonroot && docker save | ssh ... docker load`。
4. **token ACL 被 Drive 重置**:若误把同步区 token 直接挂载,Drive 同步可能把 ACL 改成 000,容器内 nonroot(uid 65532)读不到 → 账号池空。本方案用独立副本 `/volume2/docker/aurora/tokens/` 规避;若仍复现,`chmod -R 644 /volume2/docker/aurora/tokens/ && docker restart aurora`。
5. **distroless 无 shell**:无法 `docker exec` 进容器;排查靠 `docker logs aurora` 与宿主 curl。
6. **工具调用提示词平台语境**:local-toolfix 提示词含"bash 是 Git Bash 不是 PowerShell"等 Windows 专属规则,NAS(Linux)上无害但不适用;长期 NAS 化可参数化(低优先)。
7. **构建缓存被清**:NAS 若有 `Docker_Auto_Prune` 计划任务,改为 `docker image prune -f && docker builder prune -f --filter until=168h`(保留 7 天构建缓存,参考 kugou)。

---

## 四、部署方式 B:自编译 linux 二进制(保留全部 local-toolfix 修复)

### 4.1 PC 上交叉编译

```bash
cd D:\repos\aurora      # 或 D:\dev\src\aurora(local-toolfix 分支)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o aurora-linux .
```

产物约 28MB,经 Drive 同步(放 `/volume2/dev/apps/aurora/aurora-linux`)或 scp 上传。

### 4.2 NAS 上启动(群晖任务计划,开机自启)

控制面板 → 任务计划 → 新增 → 触发的任务 → 用户自定义脚本 → 勾选"开机触发",用户选 root,脚本:

```bash
cd /volume2/dev/apps/aurora
mkdir -p .runtime/logs
nohup ./aurora-linux >> .runtime/logs/aurora_run.log 2>&1 &
```

> 工作目录必须在 `/volume2/dev/apps/aurora`(或含 `.runtime/tokens/` 的目录),否则 token 回退读当前目录。
> 亦可在同一任务下先 `stop` 旧进程再启动,实现重启。

### 4.3 验证

同 §3.3。`DEBUG_TOOL_LOG` 需在 `.env` 指向 NAS 路径(如 `/volume2/dev/apps/aurora/.runtime/logs/tool_debug.log`)。

### 4.4 说明

- ✅ 保留 local-toolfix 全部修复,内存足迹最小,无容器层开销
- ⚠️ 进程守护/自启靠群晖任务计划;重启策略、日志轮转需自行维护
- 可选进阶:本地 `docker build`(amd64)后 `docker save`/`load` 到 NAS,既保留修复又享容器便利

---

## 五、注意事项(否则会翻车)

1. **双端互斥(session 互踢)**:`session_tokens.txt` 同一批 token **NAS 与 PC 不能同时运行**,否则互相踢登录态。NAS 启用后 PC 端 aurora 必须停;或 NAS 独立一份 token。
2. **session token 必须带**:NAS 上只有 access token 会被 OpenAI 403 `Unusual activity`(部署说明 §三.2 已验证)。`session_tokens.txt` + `access_tokens.txt` 成对提供,启动时自动换发新鲜登录态。
3. **监听地址**:`SERVER_HOST` 代码默认即 `0.0.0.0`(`config.go:33`),局域网默认可达,compose 里显式写出仅为防 `.env` 覆盖;`Authorization` 鉴权必须配,防内网裸奔。
4. **外网访问**:需要的话走主路由 Lucky 反代(已装)把 `https://xxx.pegbiotec.com` 反代到 `http://10.10.10.2:8080`,不要直接暴露 NAS 端口到公网。
5. **工具调用提示词的平台语境**:local-toolfix 提示词含"bash 是 Git Bash 不是 PowerShell"等 Windows 专属规则,在 NAS(Linux)上无害但不适用;长期 NAS 化可考虑把提示词示例参数化(低优先)。
6. **免费账号**:8G 内存下 `FREE_ACCOUNTS` 可以开,但 PC 实测匿名 UUID 账号最易被风控,建议保持 `false`。

---

## 六、落地步骤(本方案采用方式 A:NAS 本地构建 local-toolfix 镜像)

1. [ ] 首次:NAS 上 `mkdir -p /volume2/docker/aurora/{tokens,logs}`,拷入 4 个 token 文件(session 必带)到 `tokens/`
2. [ ] PC 上 `cd /d/repos/aurora && ./scripts/deploy_nas.sh`(首次构建约 2-3 分钟,拉 distroless 基础层)
3. [ ] `docker logs aurora` 看启动日志,确认账号池非空、token 命中 `/.runtime/tokens/`
4. [ ] curl 验证 `http://10.10.10.2:8080/v1/models`(带 `Authorization: Bearer david`)
5. [ ] **停掉 PC 端 aurora**(双端互斥,见 §五.1)
6. [ ] ZCode / 客户端 provider 地址 `127.0.0.1:8080` → `10.10.10.2:8080`
7. [ ] (可选)加 Lucky 反代做外网入口 `https://xxx.pegbiotec.com` → `http://10.10.10.2:8080`
8. [ ] 观察 1–2 天:`docker logs` 有无 403/限流/session 过期,确认健康检查续期正常

> 后续更新代码:PC `git pull` → `./scripts/deploy_nas.sh`(缓存命中秒级重建)。

---

## 三之补:DeepSeek 通道(NAS 部署)

> 详见 `docs/DEEPSEEK.md` 与 `docs/ARCHITECTURE.md`。此处只列 NAS 侧要点。

- **开关**:compose 里 `DEEPSEEK_WEB_TOKENS` 非空时,DeepSeek provider 才注册(`/v1/models` 出现 `deepseek-*`)。
  该值通过宿主机 `.env`(与 compose 同目录)注入,**不入库**:
  ```bash
  # /volume2/docker/aurora/.env
  DEEPSEEK_WEB_TOKENS=/work/.runtime/tokens/deepseek_tokens.txt
  DEEPSEEK_PROXY=http://<非美区代理>:<port>
  ```
- **token 文件**:`deepseek_tokens.txt` 放进 `/volume2/docker/aurora/tokens/`(与 ChatGPT token 同目录,只读挂载到 `/work/.runtime/tokens/`)。每行一个 user_token,**只放可丢弃小号**。
- **对外**:pi/zcode/codebuddy 走 `/v1/responses`(pi 的实际路径是 `/v1/models/responses`,已加别名路由),模型选 `deepseek-v4-*-chat`(对话)或 `*-coding`(工具调用)。
- **双端互斥**:DeepSeek 网页 token 与 ChatGPT 的 session token 一样,同一批 token 不能 NAS/PC 同时运行(互相踢登录态);NAS 启用后 PC 端 aurora 停。
- **WAF**:`DEEPSEEK_PROXY` 务必非美区,否则 CloudFront 202 拦截。
- **搜索开关(2026-09-02 提速)**:`DEEPSEEK_WEB_SEARCH=1` 时 quick 档(`-chat`)带联网搜索(+1~2s 首字延迟);
  **默认关闭**(API 客户端联网应由自己侧调 search 工具)。未在 NAS `.env` 设置 = 关闭。
  提速专项实测详见 `docs/WORKBUDDY_VALIDATION_2026-09-02.md` §11(官方 460ms vs 反代 3126ms→2400ms)。

---

## 七、参考

- PC 端部署: `D:\dev\apps\aurora\部署说明.md`(Drive 同步区,非本仓库)
- 同类部署范式: `D:\repos\kugou_api\docs\nas-build-guide.md`(NAS 本地构建镜像 + deploy.sh 一键,本文档参照其结构)
- 官方仓库: https://github.com/aurora-develop/aurora(README / docker-compose.yml / env.template)
- 网络拓扑: `D:\dev\docs\网络IP结构.txt`(NAS = 10.10.10.2;家庭 10.10.10.0/24,WireGuard 10.99.0.0/24)
