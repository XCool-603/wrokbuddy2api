#!/bin/sh
set -e

# 确保挂载的数据卷目录存在
mkdir -p /app/auths /app/data

# 修复宿主机挂载卷权限（自动解决跨宿主机与容器 UID 导致的无权读写问题）
chmod 777 /app/auths /app/data 2>/dev/null || true
chmod 666 /app/auths/* 2>/dev/null || true
chmod 666 /app/data/* 2>/dev/null || true

# 若容器以 root 启动，将目录属主移交给 app(10001)，并通过 su-exec 降权运行主程序
if [ "$(id -u)" = "0" ]; then
    chown -R app:app /app/auths /app/data 2>/dev/null || true
    exec su-exec app /app/wb2api "$@"
else
    exec /app/wb2api "$@"
fi
