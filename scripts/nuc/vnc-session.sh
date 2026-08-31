#!/bin/bash
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Xvfb :1 -screen 0 1280x720x24 -nolisten tcp &
sleep 1
DISPLAY=:1 openbox &
exec x11vnc -display :1 -rfbport 5900 -forever -shared -rfbauth /etc/x11vnc.pass
