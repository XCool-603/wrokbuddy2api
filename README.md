<h1 align="center">WorkBuddy 2API</h1>

<p align="center">
  <b>将 WorkBuddy / CodeBuddy 多账号汇聚为标准 OpenAI / Anthropic 兼容接口的高可用双域网关</b><br>
  单文件桌面端 · 双域余额与积分明细 · AI 接口 IP 白名单 · 免费额度防扣费 · 纯 Go 自动任务与猫猫旅行 · Docker 一键部署
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="Release" src="https://img.shields.io/badge/Release-v1.2.52-blue?style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI%20%26%20Anthropic-412991?style=flat-square">
  <img alt="Platform" src="https://img.shields.io/badge/Platform-Windows%20%7C%20Linux%20%7C%20macOS-emerald?style=flat-square">
  <img alt="GUI" src="https://img.shields.io/badge/GUI-Native%20WebView2-teal?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single%20EXE%20%7C%20Docker-2496ED?logo=docker&logoColor=white&style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

<p align="center">
  👉 <b><a href="DOCUMENTATION.md">【点击查看：完整架构、配置与 API 手册 (DOCUMENTATION.md)】</a></b> 👈
</p>

---

## 🌟 核心能力（截至 `v1.2.52`）

1. **双协议兼容 + 视频大模型代理 + DeepSeek Harness (`dsh`)**
   - **OpenAI & Anthropic 双协议**：提供标准 `/v1/chat/completions`、`/v1/messages`、`/v1/models`，完整支持 SSE 流式输出、Thinking 思维链、Function Calling 与多模态输入（图片 URL 自动转 Base64、Claude `image` 块及文本/代码文档清洗）。
   - **专属视频大模型 (`v1.2.27+`)**：接入国内版 **快手可灵 3.0**（`kling-video-v3` / `kling-v3-t2v` / `kling-v3-i2v` / `keling`）与 **MiniMax-H3 海螺**（`minimax-video-h3`），支持 `POST /v1/videos/generations`、`GET /v1/videos/tasks/{id}` 以及在 `/v1/chat/completions` 对话窗口中直接生成并回传 MP4 视频。
   - **DeepSeek Harness (`dsh`) 内嵌**：支持一键免环境安装便携 Node.js 运行时，直接在控制台标签页使用 Agent UI，或通过虚拟模型 `dsh` 直接调度执行。

2. **简洁现代的 Console 与双域余额 / 积分明细体系（`v1.2.49` – `v1.2.52`）**
   - **前台门户 (`/`) + 控制台 (`/console` / `/dashboard`) 分离**：门户安全脱敏对外展示服务状态、模型矩阵与接入代码；控制台聚焦生产运维。
   - **紧凑布局与域筛选**：支持 `全部 / CN / Global` 一键筛选，顶部摘要条集中展示全池/CN/Global 可用余额与 **「💰 账户余额」「📊 积分明细」** 双入口弹窗，操作列收敛为 **「余额」「明细」「操作 ▾」** 浮层菜单。
   - **精确资源包分桶解析**：精确解析 `/billing/meter/get-user-resource` 小数额度与每个资源包，拆分为 **套餐基础 (`base`) / 平台奖励 (`bonus`) / 购买积分 (`purchased`) / 7日内到期 (`expiring`)**，并汇总结算各模型限流账号数与实测单价（`cost_per_1k`）。
   - **今日自动签到状态胶囊 (`v1.2.52`)**：控制台顶部实时显示 `✓ 今日已签 X/Y CN · 下次 HH:MM`，账号列表同步显示今日签到回执，点击状态胶囊可手动触发全池签到。

3. **AI 接口 IP 白名单防扫描 + 纵深安全防御（`v1.2.51`）**
   - **IP 白名单拦截**：支持单个 IPv4/IPv6 与 CIDR 网段（如 `192.168.1.0/24`）过滤 `/v1/*`、`/v2/videos/*`、`/status`、`/admin/*` 请求，非白名单 IP 直接返回 `403`（`ip_not_allowed`），并支持切换是否始终放行本机回环（`127.0.0.1 / ::1`）。
   - **抗请求头伪造**：仅当 TCP 直连对端为可信内网/回环代理时才采信 `X-Forwarded-For` / `X-Real-IP`，公网直连严格校验真实 `RemoteAddr`。
   - **运行 IP 面板与防爆破锁定**：控制台「客户端接入 & Key」实时显示服务网卡运行 IP、监听端口与当前来访客户端 IP（支持一键 **「+ 加入白名单」**）；同一 IP 在 2 分钟内累计 25 次错误 API Key 自动锁定 2 分钟（`429`）；Web 登录连续 5 次输错密码自动锁定 5 分钟。
   - **多租户 RBAC**：支持 `Admin` 与 `User` 角色、独立 API Key、RPM 频控与每日积分上限配额，Memory-First 容错落盘。

4. **免费模型护盾 + 三因子调度与排队保活（`v1.2.47` – `v1.2.48`）**
   - **免费模型直连与扣费熔断**：`hy4free` 直连官方免费节点 `hy4free`（不再误映射到收费 `hy4-preview`）；对任意免费模型，只要上游返回 `credit > 0` 或 `quota.percent >= 100`，立即将该账号该模型冷却至重置窗口并自动切换下一健康账号。
   - **双域隔离 & 同邮箱多渠道共存**：支持国内版（`copilot.tencent.com` / `www.codebuddy.cn`）与国际版（`www.workbuddy.ai`）融合调度与跨域兜底；Google / Twitter(X) / GitHub 同邮箱登录自动独立分流落盘，互不覆盖。
   - **三因子加权随机 + 排队 SSE 心跳**：综合余量、7 日临期积分（优先消耗）、空闲时长三因子调度；模型级独立限流（`6004`）；并发满载排队期间每 3 秒发送 `: ping` SSE 心跳防止客户端断连。

5. **全自动日常签到、纯 Go 成长任务引擎与猫猫旅行（`v1.2.33` – `v1.2.46`, `v1.2.52`）**
   - **内置纯 Go 成长任务引擎**：即使无 Python 环境的单文件桌面版/服务器也可直接完成 18+ 项成长任务与活动奖励收割（包括技能分享、腾讯云轻量云专家、发现应用、公众号关注、5 模板对话等）。
   - **全自动流水线**：服务启动后 3 秒按 `签到 -> 成长任务（解锁 5 连对话门槛）-> 猫猫旅行（自动领养首只猫 +300 积分 / 派出 / 领奖）` 顺序执行；定时排程严格对齐北京时间（CST UTC+8）；遇到 `401` 自动强刷 `RefreshToken` 并重试签到。

6. **单一独立可执行文件 & 多格式批量备份恢复**
   - Windows 桌面版基于原生 Edge WebView2（`-H windowsgui`），双击即弹出窗口，无黑框控制台；支持 `.json` / `.txt`（`token----refresh` 卡密）/ `.zip` 一键导入与全量打包导出，支持应用内一键检查升级。

---

## 🚀 快速上手

### 方式 A：Windows 桌面客户端（最简单）
1. 从 [Releases 页面](https://github.com/XCool-603/wrokbuddy2api/releases) 下载 `workbuddy2api.exe` 并放入独立目录。
2. 双击运行：自动生成默认 `config.json` 与 `./data`、`./auths` 目录并打开桌面窗口。
3. 点击右上角 **「添加账号」** 授权国内版/国际版或导入凭证，即可开始使用。

### 方式 B：Docker Compose 部署
```bash
git clone https://github.com/XCool-603/wrokbuddy2api.git workbuddy2api
cd workbuddy2api
cp config.example.json config.json
docker compose up -d
```
日常维护常用命令：
```bash
# 更新镜像并重启
git pull && docker compose build && docker compose up -d

# 查看日志 / 健康检查 / 停止
docker compose logs -f workbuddy2api
curl -s http://127.0.0.1:7863/healthz
docker compose down

# 可选：一并启动 DeepSeek Harness (dsh) Agent 容器
docker compose --profile agent up -d
```

### 方式 C：源码编译（Go 1.22+）
```bash
git clone https://github.com/XCool-603/wrokbuddy2api.git workbuddy2api
cd workbuddy2api

# Windows 原生桌面 GUI 版（无黑框）
go build -ldflags="-s -w -H windowsgui" -o workbuddy2api.exe ./cmd/server

# Linux / macOS / Windows 无头服务端
go build -ldflags="-s -w" -o workbuddy2api-headless ./cmd/server
```

---

## ⚙️ 配置说明 (`config.json` & 环境变量)

首次启动自动生成的推荐配置示例（详细全量字段与全部 `WB2A_*` 环境变量见 [DOCUMENTATION.md 第 12 章](DOCUMENTATION.md#12-配置全集与环境变量参考)）：

```json
{
  "listen": ":7863",
  "api_key": "sk-workbuddy2api",
  "web_password": "",
  "ip_whitelist_enabled": false,
  "allowed_ips": [],
  "allow_localhost": true,
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "global": { "enabled": true },
  "pool": {
    "max_in_flight": 3,
    "max_in_flight_global": 2,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m",
    "degrade_threshold": 5,
    "degrade_cooldown": "10m",
    "idle_weight_per_hour": 0.5,
    "idle_weight_max": 5.0,
    "expiring_soon": "168h",
    "queue_timeout_seconds": 60
  },
  "cooldown": {
    "soft_rate": "600s",
    "soft_rate_max": "2h"
  },
  "schedule": {
    "checkin_enabled": true,
    "checkin_hours": [9, 21],
    "travel_enabled": true,
    "travel_hours": [9, 21],
    "activity_enabled": true,
    "activity_hours": [10],
    "tasks_enabled": true,
    "tasks_hours": [11]
  },
  "session_sticky": {
    "enabled": true,
    "ttl": "30m",
    "gc_interval": "5m"
  }
}
```

常用核心环境变量覆盖：
- `WB2A_LISTEN` / `WB2A_API_KEY` / `WB2A_WEB_PASSWORD`
- `WB2A_IP_WHITELIST_ENABLED` / `WB2A_ALLOWED_IPS`（逗号分隔 IP 或 CIDR）/ `WB2A_ALLOW_LOCALHOST`
- `WB2A_UPSTREAM_PROXY`（支持 `http://` / `https://` / `socks5://`）
- `WB2A_ADMIN_ENABLED` / `WB2A_MODEL_ALIASES` / `WB2A_ALERT_ENABLED` / `WB2A_ALERT_WEBHOOKS`

> 控制台在线修改的 `api_key`、`web_password`、`ip_whitelist_enabled`、`allowed_ips`、`allow_localhost` 会优先写入 `./data/settings.json` 并同步尝试写入 `config.json`，升级/重启容器不会重置。

---

## 🔌 客户端与 API 接入示例

- **OpenAI Base URL**: `http://127.0.0.1:7863/v1`
- **Anthropic Base URL**: `http://127.0.0.1:7863`（端点 `POST /v1/messages`）

### cURL 对话测试
```bash
curl -N http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer sk-workbuddy2api" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "hy4free",
    "messages": [{"role": "user", "content": "你好，请介绍一下你自己"}],
    "stream": true
  }'
```

### Python (`openai` SDK)
```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:7863/v1", api_key="sk-workbuddy2api")
resp = client.chat.completions.create(
    model="deepseek-v4.1-flash",
    messages=[{"role": "user", "content": "用 Go 写一个并发安全的缓存示例"}],
    stream=True,
)
for chunk in resp:
    delta = chunk.choices[0].delta.content
    if delta:
        print(delta, end="", flush=True)
```

### 专属视频大模型（国内版 CN 专属）
支持通过 `POST /v1/videos/generations` 提交或直接在对话模型中选择 `kling-video-v3` / `minimax-video-h3`：
```bash
# 提交异步视频任务
curl -X POST http://127.0.0.1:7863/v1/videos/generations \
  -H "Authorization: Bearer sk-workbuddy2api" \
  -H "Content-Type: application/json" \
  -d '{"model":"kling-video-v3","prompt":"赛博雨夜街头霓虹倒影，电影运镜","duration":5,"resolution":"1080P"}'

# 轮询视频任务状态
curl http://127.0.0.1:7863/v1/videos/tasks/<task_id> \
  -H "Authorization: Bearer sk-workbuddy2api"
```

---

## ❓ 常见问题排查 (FAQ)

### Q1: 提示 `open data/users.json.tmp: permission denied` 如何解决？
- 容器/宿主机挂载目录 UID 不一致时，执行 `chmod -R 777 ./data ./auths`；同时系统采用 Memory-First 架构，即使磁盘不可写，内存用户与密钥仍即时生效。

### Q2: 自动签到为什么看起来像没签？国际版支持每日签到吗？
- **仅国内版 (CN) 支持官方每日签到**（每次约 +6~+10 积分）；国际版 (Global) 官方无每日签到接口，调度器按规则跳过。
- 默认每天北京时间（CST）`09:00` 与 `21:00` 及服务启动 3 秒后执行。自 `v1.2.52` 起，控制台顶部摘要条会实时展示 **`✓ 今日已签 X/Y CN · 下次 HH:MM`**，账号列表状态列也会显示今日已签回执，遇到 `401` 会自动强刷 `RefreshToken` 重试。

### Q3: 如何防止别人扫描端口并盗用我的 API Key？
- 进入控制台 **「客户端接入 & Key」** -> **「🛡️ AI 接口 IP 白名单 & 运行 IP」**：查看服务端运行 IP、监听端口与当前来访客户端 IP，可一键 **「+ 加入白名单」**，填写允许的 IP/CIDR 网段并启用即可（非白名单 IP 直接返回 `403`）；同时连续 25 次错 Key 会自动触发防爆破锁定（`429`）。

### Q4: 免费模型（如 `hy4free`）为什么会被限流或切号？
- 网关直连官方免费节点 `hy4free`，一旦检测到上游返回积分扣费（`credit > 0`）或免费额度用尽（`quota.percent >= 100`），会自动限流该账号在该免费模型上的额度至重置时间并切换下一可用账号，保护账户积分不被消耗。

### Q5: 国际版（Global）同一邮箱登录 Google 和 Twitter/X 会互相覆盖吗？
- 不会。网关按登录渠道区分落盘文件（`workbuddy-global-google-xxx.json` / `workbuddy-global-twitter-xxx.json`）并在内存池独立建槽，转发上游时还原原始邮箱。

---

## 🔒 隐私与安全性

- `.gitignore` / `.dockerignore` 默认严格忽略 `auths/`、`data/`、`config.json` 等私有凭证与运行态数据库。
- 网关完全本地/自托管部署，所有 API 请求直接转发至官方上游服务器，不经由任何第三方中继。

## 👥 贡献者与开源许可

- 项目维护：[XCool-603](https://github.com/XCool-603)
- 开源协议：[MIT License](LICENSE)
