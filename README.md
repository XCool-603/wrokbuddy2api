<h1 align="center">WorkBuddy 2API</h1>

<p align="center">
  <b>一键将 WorkBuddy / CodeBuddy 账号转化为标准 OpenAI 兼容接口的高性能本地/跨平台网关</b><br>
  单文件原生桌面窗口 · 内嵌 Cyberpunk 控制面板 · 账号池调度 · 熔断冷却 · 自动签到 · Docker 一键部署
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.24-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Platform" src="https://img.shields.io/badge/Platform-Windows%20%7C%20Linux%20%7C%20macOS-blue?style=flat-square">
  <img alt="GUI" src="https://img.shields.io/badge/GUI-Native%20WebView2-emerald?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single%20EXE%20%7C%20Docker-2496ED?logo=docker&logoColor=white&style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

---

## 🌟 核心特色

1. **单一独立可执行文件（Single Standalone EXE）**
   - 彻底摒弃散落的辅助可执行文件（如 `login.exe`、`signin.exe`、`trial.exe`）及 `.cmd` 启动批处理脚本。
   - 所有逻辑（OAuth 登录授权、批量日常签到、领取国际版试用额度、账号管理）全部**在进程内（In-Process）原生实现**。
   - 单文件体积仅 ~9.5MB，随下随用，无任何外部环境依赖。

2. **现代化原生桌面 GUI 视窗 & Web 管理中枢**
   - Windows 下基于原生 Edge WebView2 驱动，采用 Windows GUI 模式编译（`-H windowsgui`）。
   - **双击运行即直接弹出 1280x820 桌面客户端窗口**，完全没有黑框命令行控制台干扰。
   - 内嵌重构升级的现代深色管理控制台：
     - **实时可观测性**：账号健康度、在途并发、积分实时消耗、近 14 日历史请求分析与分模型耗时/命中率透视。
     - **一键全功能运维**：一键授权登录、批量签到、国际版试用额度领取、手动导入/删除/停用。
     - **智能冷却与一键解除**：倒计时透明展示（如 `限流冷却中 (45s)`、限流模型透视），支持**一键「解除冷却」**秒级重回选号池。
     - **友好授权流程**：OAuth 授权弹窗增设底部取消/关闭按钮与精准错误透出，防止超时假死。
   - 关闭桌面窗口即自动优雅停机并持久化保存数据状态。

3. **用户角色与 API Key 资源隔离权限系统 (Multi-Tenant & RBAC)**
   - **多用户全生命周期**：支持管理员后台增删改查用户、重置用户专属 API Key、修改密码与角色权限。
   - **双重角色权限（Admin / User）**：
     - **管理员（Admin）**：全权调度全局所有账号池，查看全站流量流水与全部账号状态。
     - **普通用户（User）**：拥有独立专属的 API Key 凭证与账号池隔离，仅可查看和调度属于自己的私有账号或系统公共账号，不同用户会话互不串扰。
   - **内存优先与磁盘容错自愈 (Memory-First)**：即使在容器卷或宿主机遭遇文件系统权限限制（如 `open data/users.json.tmp: permission denied`），新创建/注册的用户与 API Key 依然保证在内存中即时激活生效，外部 API 调用与控制台操作绝不受阻！
   - **全站 Web 密码保护**：支持一键设置管理密码保护控制台，保障公网部署绝对安全。

4. **全格式一键导入与凭证容错备份 (Batch & Multi-Format Import & Export)**
   - **全格式多文件导入**：支持同时拖拽或选中多个 `.json`、`.txt` 文本文件批量导入。
   - **ZIP 压缩包备份恢复**：一键上传历史导出的 `.zip` 备份归档，系统自动解压扫描并无缝恢复所有账号。
   - **智能注释与结构清洗**：自动剥离 C 风格注释（`// ...`、`/* ... */`），容错处理非标准 JSON。
   - **第三方包装结构自动穿透**：兼容各大中继平台导出的嵌套包装结构（如 `{ "data": [...] }`、`{ "accounts": [...] }`、`{ "tokens": [...] }`）。
   - **卡密与文本识别**：智能切分每行 `accessToken----refreshToken`、`token:refresh_token` 等卡密格式，或按行批量录入纯 Token。
   - **导入安全防护**：自动过滤路径穿越与非法文件名字符，确保系统安全性。
   - **异步并发校验与实时回显**：导入后自动在后台并发刷新 Token、测试连通性并同步积分额度，控制台实时回显成功数、失败数及详尽原因清单。
   - **一键导出备份**：支持在控制台一键打包所有已保存凭证为 `.zip` 归档文件，方便跨机器快速迁移与灾备。

5. **全面支持全模态推理（图片、文档、代码与文件解析）**
   - **支持标准 OpenAI 视觉格式**：全兼容 `type: image_url`（含 Base64 Data URL 与网络图片 URL），自动做上游结构平滑兼容。
   - **支持 Anthropic / Claude 多模态协议**：自动识别并转换 `type: image` + `source: {type: "base64", data: "..."}` 结构，无缝兼容各类 Claude 客户端。
   - **智能文件与文档解析**：支持向模型直接发送文本类文件（`.txt` / `.md` / `.pdf` / `.json` / `.csv` / 各种代码文件），网关自动清洗封装为标准输入上下文，全模型均可阅读推理。
   - **模型多模态能力显式标识**：模型列表与选择器中自动打上 **`[✨ 多模态]`** 高亮徽章，一目了然区分多模态模型与纯文本模型。
   - **内置控制台调试演练场（Playground）**：
     - 支持**点击上传图片 / 文本文件 / 代码文件**，或在 Prompt 输入框中**直接粘贴屏幕截图**（`Ctrl+V`）；
     - 自动组装多模态/文件附件消息进行流式调用与实时耗时测速。

6. **模型积分倍率透明化展示**
   - 模型目录、模型下拉选择器、API 请求列表及统计报表中，全链路透明展示各模型官方计费倍率（如 `x0.05`、`x0.29`、`x1.00`），助您精准把控账号积分开销。

7. **双域自动适配（国内版 CN & 国际版 Global）**
   - 完美适配国内版（`copilot.tencent.com` / `www.codebuddy.cn`）与国际版（`www.workbuddy.ai`）。
   - 智能识别账号 Realm 域，自动按前缀路由或共享调度池，支持一键领取国际版试用加速包。

8. **生产级流式中继与流量治理**
   - **全兼容 OpenAI 接口**：标准 `/v1/chat/completions` 与 `/v1/models`，支持流式 SSE 输出与思维链（DeepSeek Reasoning Content）自动注入。
   - **三因子加权随机选号**：积分余量、快过期积分优先、空闲补偿多维度调度。
   - **故障自愈与模型级独立限流**：支持 429 智能软避让与**模型级独立限流（错误码 6004）**，单一模型被限流自动隔离该模型而不影响同账号其他模型。
   - **透明倒计时与一键解除**：控制台透明呈现限流冷却倒计时，并支持一键解除冷却立刻恢复调度。
   - **会话粘性路由**：基于会话上下文的稳定号绑定，保证多轮长对话不换号、Prompt Cache 命中最大化。
   - **全链路诊断日志**：请求分发、上游状态码透出、限流熔断轨迹全量记录，毫秒级定位问题。

9. **无缝嵌入 DeepSeek Harness (dsh) 官方 AI Agent 智能体**
   - **一键全自动安装（免任何环境配置）**：控制台提供「一键全自动安装环境」按钮，自动从镜像源静默下载解压便携式绿色 Node.js 运行时，彻底解决 Windows / 宝塔 / Linux 手动配置 Node 环境的痛点。
   - **直接显示与调用（模型代理直连）**：安装或运行后，在模型列表（`/v1/models`）与 Web 控制台下拉菜单中**直接显示 `dsh` 虚拟智能体模型**，在演练场或任何第三方客户端中输入 `dsh` 即可像调用大模型一样直接调用 Agent！
   - **一键内嵌与按需运行**：控制台专设「Agent (dsh)」标签页，支持直接在控制台一键启动/停止官方 `@deepseek-ai/dsh` 智能体网页 UI，iframe 无缝内嵌。
   - **自动化参数注入**：自动注入当前网关的高权重账号池与统一 BaseURL，随用随启、按需加载。
   - **Docker 与宝塔原生开箱即用**：Docker 镜像内已预置 Node 与 npm 运行时，`docker compose up -d` 默认一并拉起，零门槛开箱即用。

10. **全自动持续集成（CI/CD）与安全隐私保护**
    - **隐私绝对安全**：账号凭证（`auths/`）与数据库状态（`data/`）被严格隔离并在 `.gitignore` / `.dockerignore` 中屏蔽，绝不泄露任何私有数据。
    - **GitHub Actions 自动化打包**：每次打 tag 或手动点击即可自动编译 Windows 单文件桌面版与发布 Docker 镜像。

---

## 🚀 快速上手

### 方式 A：Windows 桌面客户端（最简单，双击即用）

1. 从 [Releases 页面](https://github.com/XCool-603/wrokbuddy2api/releases) 下载最新版的 `workbuddy2api.exe`。
2. 将 `workbuddy2api.exe` 放入任意独立文件夹中。
3. **直接双击 `workbuddy2api.exe`**：
   - 程序将自动拉起桌面客户端窗口；
   - 首次运行会自动生成默认配置文件 `config.json` 及数据目录；
   - 在客户端界面中点击「一键登录」，浏览器完成授权后即可直接使用！
4. 本地 OpenAI 兼容 API 接入地址：
   - **API Base URL**: `http://127.0.0.1:7863/v1`
   - **API Key**: 留空或填写配置文件中指定的 Key

---

### 方式 B：Docker 一键部署（推荐服务器 / NAS / Linux 使用）

项目内嵌完整的 Docker 支持，零配置快速构建部署：

```bash
git clone https://github.com/XCool-603/wrokbuddy2api.git
cd wrokbuddy2api

# 复制一份基础配置
cp config.example.json config.json

# 一键构建并后台启动
docker compose up -d --build
```

**更新到最新版本（无缝平滑升级，保留已有账号与数据）**：
```bash
# 1. 拉取最新代码
git pull origin main

# 2. 重新编译镜像并重启容器
docker compose up -d --build
```

**Docker 常用管理命令**：
```bash
# 查看容器日志
docker compose logs -f

# 容器内健康检查
curl http://127.0.0.1:7863/healthz

# 停止容器
docker compose down

# 可选：一键联动启动 DeepSeek Harness (dsh) AI Agent 智能体容器（无需宿主机 Node.js）
docker compose --profile agent up -d
```

> **挂载说明与卷权限自愈机制**：
> - `./auths:/app/auths`：账号授权凭证文件目录（持久化保存在宿主机）
> - `./data:/app/data`：状态快照、多用户数据库与缓存（持久化保存在宿主机）
> - `./config.json:/app/config.json:ro`：配置文件
> 
> 💡 **自动权限自愈**：新版容器内置 `docker-entrypoint.sh` 入口，容器启动时以 root 初始化，自动检测并对挂载的 `/app/auths` 和 `/app/data` 目录执行权限自愈修复（`chmod 777` / `chown`），随后通过 `su-exec` 安全降权至非 root `app (10001)` 启动主服务，彻底消除宿主机与容器文件权限冲突。
> 
> 🛠️ **历史挂载卷权限快速修复（若遇 `permission denied` 报错）**：
> ```bash
> # 方式 1：在宿主机项目根目录下执行（推荐）
> chmod -R 777 ./data ./auths
> 
> # 方式 2：在运行中的容器内直接提权修复（无需重启服务）
> docker compose exec -u 0 wb2api chmod -R 777 /app/data /app/auths
> ```

---

### 方式 C：源码编译

若需自行从源码构建：

```powershell
# 1. 克隆仓库
git clone https://github.com/XCool-603/wrokbuddy2api.git
cd wrokbuddy2api

# 2. 编译 Windows 单一独立桌面 GUI 程序 (无黑框控制台)
go build -trimpath -ldflags "-H windowsgui -s -w" -o workbuddy2api.exe ./cmd/server

# 3. 编译无头命令行/服务器端可执行文件
go build -trimpath -ldflags "-s -w" -o workbuddy2api-server ./cmd/server
```

---

## ⚙️ 配置说明 (`config.json`)

系统启动时会自动读取当前目录下的 `config.json`（若不存在则自动根据模板生成）：

```json
{
  "listen": ":7863",
  "api_key": "test_key",
  "web_password": "",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "global": {
    "enabled": true
  },
  "pool": {
    "max_in_flight": 3,
    "max_in_flight_global": 2,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m",
    "idle_weight_per_hour": 0.5,
    "idle_weight_max": 5.0,
    "expiring_soon": "168h"
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
    "activity_hours": [10]
  },
  "session_sticky": {
    "enabled": true,
    "ttl": "30m"
  }
}
```

- **`listen`**：服务监听端口，默认 `:7863`。
- **`api_key`**：访问网关接口所需的默认系统级 Bearer Token，留空时不开启 API 鉴权。
- **`web_password`**：Web 管理控制台保护密码，设置后访问前端需输密登录，公网部署必备。
- **`auth_dir`**：保存账号授权文件的目录，默认 `./auths`。
- **`state_file`**：账号池状态、熔断信息与指标持久化路径，默认 `./data/state.json`。
- **`global.enabled`**：是否启用国际版（`workbuddy.ai`）支持，默认 `true`。
- **`pool.max_in_flight`**：单个国内版账号允许的最大在途并发数，超出会自动调度至其他可用账号。
- **`pool.breaker_threshold`** / **`breaker_cooldown`**：连续失败熔断阈值与熔断冷却时间。
- **`cooldown.soft_rate`**：遭遇上游 429 限流时的初始软冷却时长（默认 600 秒）。
- **`schedule.checkin_hours`**：每日自动定时签到执行的小时列表（24 小时制），支持多时间点配置。
- **`session_sticky`**：会话粘性路由配置，开启后保证同一多轮对话稳定绑定同一账号，最大化 Prompt Cache 命中率。

---

## 🔌 客户端接入示例

所有支持设置自定义 OpenAI 接口地址的软件（如 NextChat、Cherry Studio、Chatbox、OpenCode、Cursor、Cline 等）均可无缝接入：

### cURL 请求测试
```bash
curl -N http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer test_key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v4-flash",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'
```

### Python (OpenAI SDK)
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:7863/v1",
    api_key="test_key"
)

response = client.chat.completions.create(
    model="deepseek-v4-flash",
    messages=[{"role": "user", "content": "你好，请做个自我介绍"}],
    stream=True
)

for chunk in response:
    content = chunk.choices[0].delta.content or ""
    print(content, end="", flush=True)
```

### 多模态 / 图片视觉推理 (Image Vision)
支持向带有 `[👁️ 视觉]` 标识的模型（如 `claude-3-5-sonnet`、`gpt-4o` 等）发送图片：
```bash
curl -N http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer test_key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "cn:claude-3-5-sonnet",
    "messages": [
      {
        "role": "user",
        "content": [
          {"type": "text", "text": "请分析这张图里有什么"},
          {
            "type": "image_url",
            "image_url": {
              "url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
            }
          }
        ]
      }
    ],
    "stream": true
  }'
```

---

## ❓ 常见问题与运维排查 (FAQ & Troubleshooting)

### Q1: 提示 `保存新用户失败: open data/users.json.tmp: permission denied` 如何解决？
- **产生原因**：
  在 Docker 容器或 Linux 宿主机环境中，`./data` 目录可能由 root 创建，导致容器内运行的无特权用户 `app (10001)` 在创建 `.tmp` 临时文件时遭遇文件系统权限限制。
- **高可用韧性保障（内存优先）**：
  系统内部已全面实施**内存优先（Memory-First）容错机制**。即使磁盘写入暂时受限，新创建或注册的用户、API Key 依然常驻内存 100% 激活生效，外部 API 鉴权调用与控制台操作均不受任何阻碍。
- **彻底修复权限（任选其一）**：
  1. **宿主机直接修复（推荐）**：
     ```bash
     chmod -R 777 ./data ./auths
     ```
  2. **在运行中的容器内提权修复（无需重启服务）**：
     ```bash
     docker compose exec -u 0 wb2api chmod -R 777 /app/data /app/auths
     ```
  3. **重新构建镜像**：更新到最新代码后执行 `docker compose up -d --build`，容器自带的 `docker-entrypoint.sh` 入口会在每次开机自愈修复卷目录权限。

---

### Q2: 批量导入账号支持哪些格式？导入失败怎么排查？
- **全面兼容以下多种导入格式**：
  - **多文件拖拽**：支持同时选中或拖入多个 `.json`、`.txt` 文件批量导入。
  - **ZIP 备份包恢复**：直接上传历史导出的 `.zip` 归档包，系统自动解压扫描还原。
  - **卡密与单行文本**：每行 `accessToken----refreshToken` 或 `token:refreshToken`，或每行一个纯 Token。
  - **宽松 JSON 与上游结构穿透**：支持带 `//`、`/* */` 注释的 JSON 文件；兼容各第三方分发平台包装结构（包含 `data`、`accounts`、`tokens` 等外层字段）。
- **导入失败排查**：
  - 导入完成后，控制台弹窗会实时列出成功数、失败数以及每一条失败的具体原因。
  - 若报错包含 `401 Unauthorized` 或 `token expired`，说明该 Token 已经被官方吊销或过期，请重新登录授权获取。

---

### Q3: 为什么请求出现 503 或提示 `no healthy account available`？
- **产生原因**：
  - 账号池内暂无可用账号，或账号积分已耗尽；
  - 所有可用账号的并发在途请求均已达到 `pool.max_in_flight` 阈值；
  - 账号触发了上游限流（429）或连续错误熔断，暂时进入冷却状态。
- **模型级独立软限流与一键解除**：
  - 当某个模型触发官方 429 或 6004 限流时，系统**仅将该特定模型置入冷却状态**，同账号其他模型仍可被正常调用；
  - 在 Web 控制台「账号管理」列表中，可直观查看当前处于限流冷却中的账号与到期倒计时（如 `限流冷却中 (45s)`）；
  - 若确认上游限制已解除或需要立刻测试，点击对应账号卡片上的 **「解除冷却」** 按钮，即可瞬间将账号强制重回选号池！

---

### Q4: 用户授权登录页面打不开或关闭不了？
- **弹窗交互优化**：
  OAuth 授权弹窗底部已专门加入 **「取消 / 关闭」** 按钮，任何时候点击均可直接关闭弹窗并停止轮询，杜绝页面卡死假死。
- 若网络超时未成功打开网页，请检查本地或服务器能否正常访问 Tencent / WorkBuddy 官方域名，或改用手动导入 Token 方式。

---

### Q5: 自动签到与定时任务未执行？
- 请确认 `config.json` 中配置项 `"schedule.checkin_enabled": true`；
- 签到任务默认在每日北京时间 9:00 与 21:00 定时执行（可通过 `schedule.checkin_hours` 自定义多时间点）；
- 若刚导入账号希望立即获得每日积分，可在 Web 管理控制台中点击 **「一键签到」** 手动即时触发。

---

## 🔒 隐私与安全性

- **零敏感信息泄露**：
  - 本项目源代码、Docker 镜像构建与版本库已严格将 `auths/`、`data/`、`config.json` 屏蔽过滤，**绝不会把您的任何本地凭证、数据库记录打包或提交出去**。
- **本地自托管**：
  - 所有请求仅在您的本地机器/私有服务器与上游官方服务器之间直接通信，不经由任何第三方中继服务器。

---

## 👥 贡献者与致谢

### 核心贡献者
- [XCool-603](https://github.com/XCool-603) - 项目主导、架构重构与维护
- [Antigravity](https://github.com) - 桌面视窗、单文件整合、进程内改造与 CI/CD 自动化

### 特别鸣谢
- 衷心感谢上游开源社区及原始项目贡献者（[@Sliverkiss](https://github.com/Sliverkiss) 等）在协议逆向与调度原型上的先驱探索与灵感！

---

## 📄 开源许可证

本项目基于 [MIT License](LICENSE) 协议开源。

