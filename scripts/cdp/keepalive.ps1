# scripts/cdp/keepalive.ps1 — 桥通道登录态定期保活(唤醒→活动→关闭)
#
# 用途: PC 不常驻 Chrome 的前提下,定期唤醒一次桥浏览器做"活动",滚动续期
# 各上游登录 cookie,然后优雅关闭 —— Chrome 每次只活几分钟。
# 实测到期时间: Google(Gemini)~399 天 / Claude ~28 天 / Mimo ~30 天,
# 均为滚动续期 cookie,每周保活一次绰绰有余。
#
# 用法: powershell -File scripts/cdp/keepalive.ps1
# 定时(Windows 任务计划,每周日 04:00,可自改):
#   schtasks /Create /F /TN "aurora-cdp-keepalive" /SC WEEKLY /D SUN /ST 04:00 /RL LIMITED `
#     /TR '""D:\PortableApps\_sys\node\node.exe" "D:\repos\aurora\scripts\cdp\keepalive-node.mjs""'
#   (推荐直接用 keepalive-node.mjs,纯 node 无编码坑;本 ps1 供手动运行)
$ErrorActionPreference = 'Stop'

Write-Host '== 桥通道保活 ==' -ForegroundColor Cyan

# 1. 唤醒
try {
    $w = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8798/wake" -TimeoutSec 90
    Write-Host ("唤醒: " + $w.status) -ForegroundColor Green
} catch {
    Write-Host ("唤醒失败(守护不在线?): " + $_.Exception.Message) -ForegroundColor Red
    exit 1
}
if ($w.status -eq 'timeout') { Write-Host '唤醒超时,退出' -ForegroundColor Red; exit 1 }

# 2. 等桥就绪
$bridgeReady = $false
for ($i = 0; $i -lt 30; $i++) {
    Start-Sleep -Seconds 2
    try {
        $h = Invoke-RestMethod -Uri "http://127.0.0.1:8799/health" -TimeoutSec 3
        if ($h.ok) { $bridgeReady = $true; break }
    } catch {}
}
if (-not $bridgeReady) { Write-Host '桥未就绪,退出' -ForegroundColor Red; exit 1 }
Write-Host '桥已就绪' -ForegroundColor Green

# 3. 对每个已加载令牌的桥 provider 发一条消息(活动=续期;不耗多少配额)
$models = @('gemini-3-flash-chat', 'claude-sonnet-5-chat')
$tokens = $h.providers
foreach ($m in $models) {
    $p = if ($m -like 'gemini*') { 'gemini' } else { 'claude' }
    if (-not $tokens.$p.tokens.at -and -not $tokens.$p.tokens.orgId) { continue }
    try {
        $body = '{"model":"' + $m + '","messages":[{"role":"user","content":"hi"}],"stream":false}'
        $f = Join-Path $env:TEMP ("ka_" + $p + ".json")
        [System.IO.File]::WriteAllText($f, $body, (New-Object System.Text.UTF8Encoding($false)))
        $r = curl.exe -s -m 60 -H "Content-Type: application/json" --data-binary "@$f" "http://127.0.0.1:8799/v1/chat/completions"
        if ($r -like '*"error"*') { Write-Host ("  " + $m + ": 失败 " + $r.Substring(0, [Math]::Min(120, $r.Length))) -ForegroundColor Yellow }
        else { Write-Host ("  " + $m + ": 活动完成 ✓") -ForegroundColor Green }
    } catch {
        Write-Host ("  " + $m + ": 异常 " + $_.Exception.Message) -ForegroundColor Yellow
    }
    Start-Sleep -Seconds 3
}

# 4. 优雅关闭(CDP Browser.close + 停桥)
Write-Host '保活完成,关闭 Chrome + 桥...' -ForegroundColor Cyan
Start-Sleep -Seconds 5
$tmp = Join-Path $env:TEMP 'ka_close.mjs'
@"
import { pathToFileURL } from 'node:url';
import http from 'node:http';
const { cdp } = await import(pathToFileURL('D:/repos/aurora/scripts/cdp/cdp-helper.mjs').href);
function getJSON(p) { return new Promise((res, rej) => { http.get({ host: '127.0.0.1', port: 9222, path: p }, (r) => { let d=''; r.on('data', c=>d+=c); r.on('end', ()=>res(JSON.parse(d))); }).on('error', rej); }); }
try { const targets = await getJSON('/json'); const page = targets.find(t => t.type === 'page'); const c = await cdp(page.webSocketDebuggerUrl); await c.cmd('Browser.close'); console.log('Browser.close sent'); c.close(); } catch (e) { console.log('close err: ' + e.message); }
"@ | Set-Content -Path $tmp -Encoding UTF8
node $tmp
Start-Sleep -Seconds 3
$b = Get-NetTCPConnection -LocalPort 8799 -State Listen -ErrorAction SilentlyContinue
if ($b) { Stop-Process -Id $b[0].OwningProcess -Force -ErrorAction SilentlyContinue }
Write-Host '已关闭 ✓' -ForegroundColor Green
