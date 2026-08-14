# scripts/cdp/keepalive-daily.ps1 — 每日桥保活:固定 8:30 触发 + 随机延迟
#
# 任务计划每天 08:30 启动本脚本,脚本随机延迟 0~930 分钟(15.5h),
# 因此实际保活时刻每天落在 08:30~24:00 之间任意点,不固定。
# 保活全程只走本地 PC(守护→Chrome→桥),不依赖 NAS。
#
# 注册(管理员或普通用户均可用 /RL LIMITED):
#   schtasks /Create /F /TN "aurora-cdp-keepalive" /SC DAILY /ST 08:30 /RL LIMITED `
#     /TR 'powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "D:\repos\aurora\scripts\cdp\keepalive-daily.ps1"'
$ErrorActionPreference = 'Continue'

$log = 'D:\repos\aurora\.runtime\keepalive.log'
$node = 'D:\PortableApps\_sys\node\node.exe'
$ka = 'D:\repos\aurora\scripts\cdp\keepalive-node.mjs'

# 随机延迟:0..930 分钟
$delaySec = Get-Random -Minimum 0 -Maximum 55800
$ts = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
"[keepalive-daily] $ts scheduled; delay $([int]($delaySec / 60)) min" | Out-File -Append -Encoding utf8 $log

Start-Sleep -Seconds $delaySec

$ts = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
"[keepalive-daily] $ts running keepalive-node" | Out-File -Append -Encoding utf8 $log
& $node $ka *>&1 | Out-File -Append -Encoding utf8 $log
