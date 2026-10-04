# =============================================================================
# Stage 1: Build stage (Go 1.23)
# =============================================================================
FROM golang:1.23-alpine AS build

WORKDIR /src
ENV GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct

# 1. 优先拷贝依赖定义文件，并利用 BuildKit 缓存挂载加速模块下载
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# 2. 拷贝业务源代码
COPY . .

# 3. 编译所有二进制产物（开启编译器缓存挂载，实现秒级增量重构）
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trial_bin ./cmd/trial \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/activity_bin ./cmd/activity

# =============================================================================
# Stage 2: Runtime stage (Alpine 3.20)
# =============================================================================
FROM alpine:3.20

# 1. 参数化非 root 用户 UID 与 GID（默认 10001，便于和宿主机权限对齐）
ARG UID=10001
ARG GID=10001

# 2. 国内源镜像加速，安装系统必要依赖并建立固定 UID/GID 的非 root 运行期用户与 HOME
RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories \
 && apk add --no-cache wget ca-certificates tzdata python3 bash nodejs npm su-exec \
 && addgroup -g "${GID}" -S app \
 && adduser -u "${UID}" -S -G app -h /home/app -s /sbin/nologin app \
 && install -d -o app -g app -m 755 /app /app/auths /app/data /app/scripts /home/app

WORKDIR /app
ENV HOME=/home/app \
    TZ=Asia/Shanghai

# 3. 产物与脚本置入：统一在 COPY 时以 --chown 与 --chmod 固化属主与权限，避免生成多余的数据修改层
COPY --from=build --chown=app:app --chmod=755 /out/ /app/
COPY --chown=app:app --chmod=755 login.sh signin.sh credit.sh trial.sh /app/
COPY --chown=app:app --chmod=755 scripts/global_region.py scripts/task_common.py scripts/task_runner.py scripts/school_open_day_2026.py /app/scripts/
COPY --chown=app:app --chmod=755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY --chown=app:app config.example.json /app/config.json
COPY --chown=app:app config/ /app/config/

# 4. 网络端口与健康检查（监听 7863 > 1024，天然安全兼容非 root）
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1

# 5. USER 只出现一次，且放在最后（满足 docker-skill 最小正确规范）
USER app

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["-config", "/app/config.json"]
