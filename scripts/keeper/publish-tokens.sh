#!/bin/sh
# publish-tokens.sh —— keeper publish 最小版(G1/E3 闭环,2026-09-05)
#
# 职责:把 Drive 同步区的 token 文件(用户在 PC 侧维护)同步到 aurora 部署区,
# 配合 E3 凭证热加载(容器内 NextToken/webClient 的 mtime 检查),实现
# "PC 补票 → Drive 同步 → 本脚本 → 容器内即时生效"全闭环,无需重启。
#
# 同步范围(仅容器实际读取的 ro 卷 tokens/ 下的文件,与 compose 环境变量对齐):
#   deepseek/qianwen/grok/minimax/mimo 的 token 文件 + doubao_accounts.json
#   + chatgpt 池文件(access/session/refresh/free/proxies)
#   GLM/KIMI 明确排除 —— 它们走 tokens-state(rw)进程内 refresh 自愈,
#   同步旧文件会覆盖进程回写的新池。
#
# 安全规则:
#   - 源文件为空/缺失 → 跳过(keep-last-good,防误清部署区)
#   - 内容相同 → 零动作(幂等,cron 高频跑不刷屏)
#   - 原子覆盖(cp 到临时名 + mv rename),避免热加载读到半截文件
#   - .json 源文件先经 JSON 合法性校验,不合法跳过并告警
#
# 调度(二选一,需 root 因脚本属主为 zxsadmin 而部署区同属主,普通用户即可):
#   - DSM 任务计划 → 计划任务 → 用户定义的脚本(建议每 15 分钟)
#   - /etc/crontab 加行(群晖不读用户 crontab,必须 root):
#       */15 * * * * root /volume2/docker/aurora/publish-tokens.sh
set -u

SRC=/volume2/dev/apps/aurora/.runtime/tokens
DST=/volume2/docker/aurora/tokens
LOG=/volume2/docker/aurora/logs/publish-tokens.log

# 容器实际读取的文件清单(与 docker-compose.nas.yml 环境变量对齐)
FILES="deepseek_tokens.txt qianwen_tokens.txt grok_cookies.txt minimax_tokens.txt mimo_tokens.txt doubao_accounts.json access_tokens.txt session_tokens.txt refresh_tokens.txt free_tokens.txt proxies.txt"

[ -d "$SRC" ] || { echo "$(date '+%F %T') [publish] 同步区不存在: $SRC" >> "$LOG"; exit 0; }
mkdir -p "$DST" "$(dirname "$LOG")"

# 日志轮转:超过 256KB 截断保留尾部
if [ -f "$LOG" ] && [ "$(wc -c < "$LOG")" -gt 262144 ]; then
  tail -n 200 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
fi

changed=""
for f in $FILES; do
  src="$SRC/$f"; dst="$DST/$f"
  [ -s "$src" ] || continue            # 源缺失/空:跳过,保留部署区现值
  if [ -f "$dst" ] && cmp -s "$src" "$dst"; then
    continue                            # 内容相同:幂等零动作
  fi
  case "$f" in
    *.json)
      # JSON 防呆:用 python3(NAS 非交互 PATH 可达;/usr/local/bin 的 node 不可达)。
      # 校验器存在时,非法 JSON 拦截不覆盖;校验器缺失时放行并记录,不阻塞同步。
      if command -v python3 >/dev/null 2>&1; then
        if ! python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$src" >/dev/null 2>&1; then
          echo "$(date '+%F %T') [publish] WARN $f JSON 校验失败,跳过" >> "$LOG"
          continue
        fi
      else
        echo "$(date '+%F %T') [publish] INFO python3 缺失,$f 未做 JSON 校验" >> "$LOG"
      fi
      ;;
  esac
  # 原子覆盖:同文件系统 rename 原子,热加载要么见旧文件要么见完整新文件
  if cp "$src" "$dst.tmp" && mv "$dst.tmp" "$dst"; then
    chmod 644 "$dst"
    changed="$changed $f"
  else
    echo "$(date '+%F %T') [publish] WARN $f 覆盖失败" >> "$LOG"
  fi
done

if [ -n "$changed" ]; then
  echo "$(date '+%F %T') [publish] 已同步:$changed(E3 热加载将自动生效)" >> "$LOG"
  # 容器内 mtime 检查最迟下一次请求时生效,无需任何 reload 动作
fi
