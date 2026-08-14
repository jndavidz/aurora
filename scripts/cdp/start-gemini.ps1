# scripts/cdp/start-gemini.ps1 — 按需启动 Gemini CDP 通道(Chrome + 桥)
# 设计目标:低频备用通道不常驻,用的时候一键起,用完即停。
# 登录态(Chrome profile)与令牌(.runtime/bridge/gemini_session.json)都在磁盘,随起随用。
#
# 用法: powershell -File scripts/cdp/start-gemini.ps1   (或右键"使用 PowerShell 运行")
# 停止: ① Ctrl+C 停桥 + 关闭 Chrome 窗口;② 不操作也行 —— 桥默认 30 分钟无对话活动
#        自动经 CDP 关闭整个 Chrome 并退出(环境变量 IDLE_TIMEOUT_MIN 可调,0=关闭)。
#        本脚本加了 --disable-background-mode,Chrome 关窗即全退,不留后台进程。
#
# 令牌过期自愈:起桥后若 /health 或对话报 token_stale,在浏览器页面上手动发任意一条
# 消息,桥的 Network 监听会自动刷新令牌(无需重跑 capture-streamgenerate.mjs 引导)。
$ErrorActionPreference = 'Stop'

$CHROME  = 'D:\PortableApps\_net\Chrome for Testing\chrome.exe'
$PROFILE = 'D:\PortableApps\_net\chrome-cdp\profile'
$BRIDGE  = 'D:\repos\aurora\scripts\cdp\bridge.mjs'
$CDP_PORT = 9222

Write-Host '== Gemini CDP 通道按需启动 ==' -ForegroundColor Cyan

# ── 1/3 启动 Chrome for Testing(独立 profile)────────────────────
$conn = Get-NetTCPConnection -LocalPort $CDP_PORT -State Listen -ErrorAction SilentlyContinue
$chromeRunning = $false
if ($conn) {
    $proc = Get-Process -Id $conn[0].OwningProcess -ErrorAction SilentlyContinue
    if ($proc -and $proc.Path -like '*Chrome for Testing*') {
        Write-Host "[1/3] Chrome 已在运行(CDP $CDP_PORT 就绪)" -ForegroundColor Green
        $chromeRunning = $true
    } else {
        Write-Host "[!] 端口 $CDP_PORT 被 $($proc.ProcessName) (PID $($proc.Id)) 占用" -ForegroundColor Yellow
        Write-Host '    请先关闭占用进程(如 Min/其他 Chrome),或修改本脚本 CDP_PORT 后重试'
        exit 1
    }
}

if (-not $chromeRunning) {
    if (-not (Test-Path $CHROME)) {
        Write-Host "[!] 未找到 Chrome for Testing: $CHROME" -ForegroundColor Red
        exit 1
    }
    Write-Host '[1/3] 启动 Chrome for Testing(独立 profile)...'
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    Start-Process -FilePath $CHROME -ArgumentList @(
        "--remote-debugging-port=$CDP_PORT",
        "--user-data-dir=$PROFILE",
        # 资源减配(不破坏真指纹;禁用 sync/后台网络/组件更新是省内存大头)
        '--disable-extensions','--disable-sync','--disable-background-networking',
        '--disable-component-update','--disable-default-apps','--disable-notifications',
        '--no-first-run','--no-default-browser-check','--mute-audio',
        # 更轻:关后台驻留(关窗即全退,配合自动停止)/限渲染进程数/限磁盘缓存
        '--disable-background-mode','--renderer-process-limit=4',
        '--disk-cache-size=104857600','--disable-crash-reporter',
        '--noerrdialogs','--disable-logging'
        # ⚠️ 禁止加 --headless / --disable-gpu:headless 的 WebGL 走 SwiftShader
        #    软件渲染,恰是 Google 认 bot 的招牌信号;真指纹必须 headful + 真核显
    )
    $ver = $null
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 250
        try {
            $ver = (Invoke-RestMethod -Uri "http://127.0.0.1:$CDP_PORT/json/version" -TimeoutSec 2).Browser
            break
        } catch {}
    }
    $sw.Stop()
    if ($ver) { Write-Host "[1/3] CDP 就绪: $ver (冷启动耗时 $([math]::Round($sw.Elapsed.TotalSeconds,1))s)" -ForegroundColor Green }
    else { Write-Host '[1/3] CDP 未就绪(Chrome 可能启动失败),继续起桥,桥会自报连接状态' -ForegroundColor Yellow }
}

# ── 2/3 启动桥(前台,桥日志直接打到本窗口;Ctrl+C 停止)─────────────
if (-not (Test-Path $BRIDGE)) {
    Write-Host "[!] 未找到桥脚本: $BRIDGE" -ForegroundColor Red
    exit 1
}
# 桥默认监听 0.0.0.0:局域网可达,供 NAS 的 GEMINI_CDP_URL 转发。
# 仅本机使用可改回 127.0.0.1;局域网开放建议设 BRIDGE_AUTH(与 NAS 的 GEMINI_CDP_KEY 一致)。
if (-not $env:BRIDGE_HOST) { $env:BRIDGE_HOST = '0.0.0.0' }
Write-Host '[2/3] 启动桥(监听 ' -NoNewline -ForegroundColor Cyan
Write-Host "$($env:BRIDGE_HOST)" -NoNewline -ForegroundColor Cyan
Write-Host ';停止 = Ctrl+C,然后关闭 Chrome 窗口)' -ForegroundColor Cyan
Write-Host '[3/3] 本机: http://127.0.0.1:8799   局域网/NAS 转发: http://<本机IP>:8799' -ForegroundColor Cyan
Write-Host '      健康检查: http://127.0.0.1:8799/health' -ForegroundColor Cyan
Write-Host ''
node $BRIDGE
