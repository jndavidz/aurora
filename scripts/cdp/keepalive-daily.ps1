# scripts/cdp/keepalive-daily.ps1 — 每周桥保活(随机时刻,错过自动补)
#
# 任务计划每天 08:30 启动本脚本(注册命令见下)。逻辑:
#   1. 读状态文件:距上次成功保活 < 7 天 → 秒退(平时零开销);
#   2. ≥ 7 天 → 随机延迟 0~930 分钟(15.5h),实际执行点落在 08:30~24:00
#      任意时刻,每天不同,不固定;
#   3. 依次执行三步,全部成功才写状态(失败次日重试 —— PC 关机错过自动补):
#      a. refresh-tokens.mjs:唤醒 Chrome,从页面代取 MiniMax/Mimo 凭证,
#         回写本地 token 文件;
#      b. scp 凭证到 NAS 容器挂载目录(直连,不走 Drive);
#      c. keepalive-node.mjs:gemini/claude 各发一条随机问候,活动续期,
#         最后 CDP Browser.close 优雅关闭 Chrome。
#
# 注册(管理员或普通用户均可用 /RL LIMITED):
#   schtasks /Create /F /TN "aurora-cdp-keepalive" /SC DAILY /ST 08:30 /RL LIMITED `
#     /TR 'powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "D:\repos\aurora\scripts\cdp\keepalive-daily.ps1"'
$ErrorActionPreference = 'Continue'

$log = 'D:\repos\aurora\.runtime\keepalive.log'
$state = 'D:\repos\aurora\.runtime\keepalive-state.txt'
$node = 'D:\PortableApps\_sys\node\node.exe'
$rf = 'D:\repos\aurora\scripts\cdp\refresh-tokens.mjs'
$ka = 'D:\repos\aurora\scripts\cdp\keepalive-node.mjs'
$tokens = 'D:\repos\aurora\.runtime\tokens'
$scp = 'C:\Windows\System32\OpenSSH\scp.exe'

$ts = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'

# 0. MiniMax 每日签到(每天执行,不受下方 7 天保活判断影响;签到积分补充 Token Plan 配额)
$ck = 'D:\repos\aurora\scripts\cdp\minimax-checkin.mjs'
& $node $ck *>&1 | Out-File -Append -Encoding utf8 $log

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

# 3a. 代取 MiniMax/Mimo 凭证
$ts = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
"[keepalive-daily] $ts running refresh-tokens" | Out-File -Append -Encoding utf8 $log
& $node $rf *>&1 | Out-File -Append -Encoding utf8 $log
$rfOk = ($LASTEXITCODE -eq 0)

# 3b. scp 到 NAS(容器挂载目录,直读生效)
$scpOk = $false
if ($rfOk) {
    "[keepalive-daily] $ts scp tokens to NAS" | Out-File -Append -Encoding utf8 $log
    & $scp -o BatchMode=yes -o ConnectTimeout=10 "$tokens\minimax_tokens.txt" "$tokens\mimo_tokens.txt" 'zxsadmin@10.10.10.2:/volume2/docker/aurora/tokens/' *>&1 | Out-File -Append -Encoding utf8 $log
    $scpOk = ($LASTEXITCODE -eq 0)
} else {
    "[keepalive-daily] $ts scp skipped (refresh failed)" | Out-File -Append -Encoding utf8 $log
}

# 3c. 保活消息(gemini/claude),结尾优雅关闭 Chrome
$ts = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
"[keepalive-daily] $ts running keepalive-node" | Out-File -Append -Encoding utf8 $log
& $node $ka *>&1 | Out-File -Append -Encoding utf8 $log
$kaOk = ($LASTEXITCODE -eq 0)

if ($rfOk -and $scpOk -and $kaOk) {
    Get-Date -Format 'yyyy-MM-dd HH:mm:ss' | Out-File -Encoding utf8 $state
    "[keepalive-daily] done, state updated" | Out-File -Append -Encoding utf8 $log
} else {
    "[keepalive-daily] failed (refresh=$rfOk scp=$scpOk keepalive=$kaOk), will retry tomorrow" | Out-File -Append -Encoding utf8 $log
}
