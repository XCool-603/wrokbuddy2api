#!/bin/sh
set -eu

# 如果首个参数是以 - 开头的 flag（如 -config /app/config.json），自动补齐主程序路径 /app/wb2api
if [ "${1#-}" != "$1" ]; then
    set -- /app/wb2api "$@"
fi

# 如果首个参数为 wb2api，转换为绝对路径
if [ "$1" = "wb2api" ]; then
    shift
    set -- /app/wb2api "$@"
fi

# 确保数据卷目录存在
mkdir -p /app/auths /app/data /app/scripts

# 如果容器以 root (UID 0) 身份启动（常见于 bind mount 挂载宿主机目录）：
# 自动矫正挂载卷属主为 app:app 并放行权限，然后通过 su-exec 降权至非 root app 用户执行
if [ "$(id -u)" = "0" ]; then
    chown -R app:app /app/auths /app/data 2>/dev/null || true
    chmod 775 /app/auths /app/data 2>/dev/null || true
    exec su-exec app:app "$@"
fi

# 非 root 身份直接以当前用户执行（确保 PID 1 正常接收系统 SIGTERM/SIGINT 实现优雅停机）
exec "$@"
