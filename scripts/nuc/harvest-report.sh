#!/usr/bin/env bash
# harvest-report.sh —— 每日凭证任务汇总报告(2026-09-05)
#
# 汇总当天 token-harvester(6 站凭证提取)+ minimax-checkin(签到)的结果,
# 输出一行摘要 + 明细到本服务 journal(可 ssh journalctl -u harvest-report 查)。
# 由 harvest-report.timer 每日 09:45 触发(checkin 09:00+rand30 之后)。
#
# 用法: harvest-report.sh [YYYY-MM-DD]   (默认今天)
# 权威副本: scripts/nuc/(改先改仓库再同步)
set -uo pipefail

DAY="${1:-$(date +%F)}"

h=$(journalctl -u token-harvester.service --since "$DAY 00:00" --until "$DAY 23:59:59" --no-pager 2>/dev/null | grep '\[harvest\]')
c=$(journalctl -u minimax-checkin.service --since "$DAY 00:00" --until "$DAY 23:59:59" --no-pager 2>/dev/null | grep '\[checkin\]')

pushes=$(printf '%s\n' "$h" | grep -c 'OK len')
same=$(printf '%s\n' "$h" | grep -c 'unchanged')
fails=$(printf '%s\n' "$h" | grep -c 'FAIL')
ran=0
[ -n "$h" ] && ran=1
checkin=$(printf '%s\n' "$c" | grep -E '签到成功|已签到过' | tail -1)
[ -z "$checkin" ] && checkin="(无记录:未运行?)"

echo "[report $DAY] harvester run=$ran 推送=$pushes 幂等=$same 失败=$fails | checkin: $checkin"
# 明细(便于 journal 追溯)
printf '%s\n' "$h" | grep -E 'OK len|FAIL' | sed 's/^.*\[harvest\]/[report]/'
if [ "$ran" = "0" ] || [ "$fails" != "0" ] || printf '%s' "$checkin" | grep -q '无记录'; then
  echo "[report $DAY] ⚠ 存在缺口(未运行/有失败/签到缺失)—— 需人工确认"
  exit 1
fi
echo "[report $DAY] ✅ 全部完成"
exit 0
