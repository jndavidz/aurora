#!/usr/bin/env bash
# 一键启动/优雅关闭/状态检查 Chrome for Testing + CDP（scripts/cdp/* 抓取脚本的前置工具）
# 用法: bash start-chrome-cdp.sh {start|stop|status} [port]
# 特性:
#   - Chrome 152 实测忽略 --remote-debugging-address(强制绑 127.0.0.1, 新版安全策略)
#     → WSL 侧 127.0.0.1 不可达, 抓取统一走 Windows 侧 node 直连(见下)
#   - stop 用 WM_CLOSE(优雅), 不用 /F 强杀(避免"异常关闭"横幅导致自动化失效)
#   - 自动清理单实例锁文件(强杀残留后必须)
# 抓取(Windows 侧, 最稳):
#   D:\PortableApps\_sys\node\node.exe D:\repos\aurora\scripts\cdp\cdp-drive.mjs <url> --out out.txt
set -u

PORT="${2:-9222}"
CHROME_DIR="/mnt/d/PortableApps/_net/Chrome for Testing"
PROFILE="/mnt/d/PortableApps/_net/chrome-cdp/profile"

# /mnt/d/... → D:\...  (Windows 程序参数用)
win() { echo "$1" | sed 's|^/mnt/\([a-z]\)/|\U\1:\\|; s|/|\\|g'; }

cdp_ok_win() { # 1=通 0=不通 (Windows 侧检查 127.0.0.1)
  cmd.exe /c "curl -s -m 3 http://127.0.0.1:$PORT/json/version" 2>/dev/null | grep -q "Browser" && echo 1 || echo 0
}

case "${1:-}" in
  start)
    echo ">> 清理残留实例与锁文件"
    taskkill.exe /IM chrome.exe 2>/dev/null; sleep 2
    rm -f "$(win "$PROFILE")lockfile" 2>/dev/null
    echo ">> 启动 Chrome for Testing (port $PORT)"
    cd "$CHROME_DIR"
    nohup ./chrome.exe --remote-debugging-port=$PORT \
      --user-data-dir="$(win "$PROFILE")" --no-first-run --no-default-browser-check \
      --disable-background-networking --disable-component-update --disable-sync \
      --window-size=1280,900 about:blank > /tmp/chrome-cdp.log 2>&1 & disown
    echo ">> 等待 CDP 就绪 (Windows 侧检查)"
    for i in $(seq 1 25); do
      sleep 1
      [ "$(cdp_ok_win)" = "1" ] && break
    done
    if [ "$(cdp_ok_win)" = "1" ]; then
      echo "✅ CDP 就绪: http://127.0.0.1:$PORT (仅回环, WSL 不可直达)"
      echo "   抓取(Windows 侧): D:\\PortableApps\\_sys\\node\\node.exe D:\\repos\\aurora\\scripts\\cdp\\cdp-drive.mjs <url> --out out.txt"
    else
      echo "❌ CDP 未就绪, 看日志: tail /tmp/chrome-cdp.log"; exit 1
    fi
    ;;
  stop)
    if taskkill.exe /IM chrome.exe 2>/dev/null; then
      echo "已发送优雅关闭信号(WM_CLOSE)"
    else
      echo "无运行中的 chrome.exe"
    fi
    ;;
  status)
    echo "Windows 侧 127.0.0.1:$PORT: $(cdp_ok_win | sed 's/1/✅/;s/0/❌/')"
    tasklist.exe /FI "IMAGENAME eq chrome.exe" 2>/dev/null | grep -ci "chrome.exe" | sed 's/^/chrome 进程数: /'
    ;;
  *) echo "用法: bash start-chrome-cdp.sh {start|stop|status} [port]"; exit 1;;
esac
