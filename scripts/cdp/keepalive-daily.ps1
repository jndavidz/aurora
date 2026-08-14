# scripts/cdp/keepalive-daily.ps1 — 每周桥保活(随机时刻,错过自动补)
#
# 任务计划每天 08:30 启动本脚本(注册命令见下)。逻辑:
#   1. 读状态文件:距上次成功保活 < 7 天 → 秒退(平时零开销);
#   2. ≥ 7 天 → 随机延迟 0~930 分钟(15.5h),实际执行点落在 08:30~24:00
#      任意时刻,每天不同,不固定;
#   3. 跑 keepalive-node.mjs(唤醒→活动→优雅关闭);成功才写状态时间戳,
#      失败次日重试 —— PC 关机错过的那天自动补跑。
#
# 注册(管理员或普通用户均可用 /RL LIMITED):
#   schtasks /Create /F /TN "aurora-cdp-keepalive" /SC DAILY /ST 08:30 /RL LIMITED `
#     /TR 'powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "D:\repos\aurora\scripts\cdp\keepalive-daily.ps1"'
$ErrorActionPreference = 'Continue'

$log = 'D:\repos\aurora\.runtime\keepalive.log'
$state = 'D:\repos\aurora\.runtime\keepalive-state.txt'
$node = 'D:\PortableApps\_sys\node\node.exe'
$ka = 'D:\repos\aurora\scripts\cdp\keepalive-node.mjs'

$ts = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'

# 1. 距上次成功保活 < 7 天 → 今天不跑
if (Test-Path $state) {
    $last = ([string](Get-Content $state -Raw)).Trim()
    if ($last) {
        try {
            $lastDt = [datetime]$last
            if ((Get-Date) - $lastDt -lt [timespan]::FromDays(7)) {
                "[keepalive-daily] $ts skip (last ok: $last)" | Out-File -Append -Encoding utf8 $log
                exit 0
            }
        } catch { }
    }
}

# 2. 随机延迟 0..930 分钟
$delaySec = Get-Random -Minimum 0 -Maximum 55800
"[keepalive-daily] $ts due; delay $([int]($delaySec / 60)) min" | Out-File -Append -Encoding utf8 $log
Start-Sleep -Seconds $delaySec

# 3. 执行保活;成功(exit 0)才写状态
$ts = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
"[keepalive-daily] $ts running keepalive-node" | Out-File -Append -Encoding utf8 $log
& $node $ka *>&1 | Out-File -Append -Encoding utf8 $log
if ($LASTEXITCODE -eq 0) {
    Get-Date -Format 'yyyy-MM-dd HH:mm:ss' | Out-File -Encoding utf8 $state
    "[keepalive-daily] done, state updated" | Out-File -Append -Encoding utf8 $log
} else {
    "[keepalive-daily] failed (exit $LASTEXITCODE), will retry tomorrow" | Out-File -Append -Encoding utf8 $log
}
