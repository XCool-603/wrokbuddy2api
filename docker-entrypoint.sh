#!/bin/sh
set -e

# 设置宽松 umask，保证容器内创建的文件与宿主机双向读写无障碍
umask 000

# 确保挂载的数据卷目录存在
mkdir -p /app/auths /app/data

# 递归放行挂载卷权限（自动解决宿主机挂载卷导致的读写被拒）
chmod -R 777 /app/auths /app/data 2>/dev/null || true

# 直接运行主程序（容器内以 root 权限执行，彻底消除 UID 10001 跨卷挂载 Permission Denied）
exec /app/wb2api "$@"
