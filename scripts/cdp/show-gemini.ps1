# scripts/cdp/show-gemini.ps1 — 把屏幕外的 Chrome 窗口拉回屏幕
# 日常唤醒的 Chrome 窗口驻留在屏幕外(-32000,-32000)不打扰;
# 登录小号、令牌自愈需要人工操作页面时,运行本脚本把窗口拉回 (100,100)。
# 用法: powershell -File scripts/cdp/show-gemini.ps1
$ErrorActionPreference = 'Stop'
try {
    $r = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8798/show" -TimeoutSec 10
    if ($r.ok) { Write-Host '窗口已拉回屏幕 (100,100)' -ForegroundColor Green }
    else { Write-Host ("失败: " + $r.reason) -ForegroundColor Red }
} catch {
    Write-Host ("失败: 唤醒守护不在线? " + $_.Exception.Message) -ForegroundColor Red
}
