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
   - 关闭桌面窗口即自动优雅停机并持久化保存数据状态。

3. **用户角色与 API Key 资源隔离权限系统 (Multi-Tenant & RBAC)**
   - **多用户系统**：支持用户自主注册、管理员后台增删改查用户与密码重置。
   - **双重角色权限（Admin / User）**：
     - **管理员（Admin）**：全权调度全局所有账号池，查看全站流量流水与全部账号状态。
     - **普通用户（User）**：拥有独立专属的 API Key 凭证与账号池隔离，仅可查看和调度属于自己的私有账号或系统公共账号，不同用户会话互不串扰。
   - **全站 Web 密码保护**：支持一键设置管理密码保护控制台，保障公网部署绝对安全。

4. **全面支持全模态推理（图片、文档、代码与文件解析）**
   - **支持标准 OpenAI 视觉格式**：全兼容 `type: image_url`（含 Base64 Data URL 与网络图片 URL），自动做上游结构平滑兼容。
   - **支持 Anthropic / Claude 多模态协议**：自动识别并转换 `type: image` + `source: {type: "base64", data: "..."}` 结构，无缝兼容各类 Claude 客户端。
   - **智能文件与文档解析**：支持向模型直接发送文本类文件（`.txt` / `.md` / `.pdf` / `.json` / `.csv` / 各种代码文件），网关自动清洗封装为标准输入上下文，全模型均可阅读推理。
   - **模型多模态能力显式标识**：模型列表与选择器中自动打上 **`[✨ 多模态]`** 高亮徽章，一目了然区分多模态模型与纯文本模型。
   - **内置控制台调试演练场（Playground）**：
     - 支持**点击上传图片 / 文本文件 / 代码文件**，或在 Prompt 输入框中**直接粘贴屏幕截图**（`Ctrl+V`）；
     - 自动组装多模态/文件附件消息进行流式调用与实时耗时测速。

5. **模型积分倍率透明化展示**
   - 模型目录、模型下拉选择器、API 请求列表及统计报表中，全链路透明展示各模型官方计费倍率（如 `x0.05`、`x0.29`、`x1.00`），助您精准把控账号积分开销。

6. **双域自动适配（国内版 CN & 国际版 Global）**
   - 完美适配国内版（`copilot.tencent.com` / `www.codebuddy.cn`）与国际版（`www.workbuddy.ai`）。
   - 智能识别账号 Realm 域，自动按前缀路由或共享调度池，支持一键领取国际版试用加速包。

7. **生产级流式中继与流量治理**
   - **全兼容 OpenAI 接口**：标准 `/v1/chat/completions` 与 `/v1/models`，支持流式 SSE 输出与思维链（DeepSeek Reasoning Content）自动注入。
   - **三因子加权随机选号**：积分余量、快过期积分优先、空闲补偿多维度调度。
   - **故障自愈与熔断机制**：429 限流软冷却避让、模型级独立限流（6004）、连续失败自动熔断保护。
   - **会话粘性路由**：基于会话上下文的稳定号绑定，保证多轮长对话不换号、Prompt Cache 命中最大化。

8. **无缝嵌入 DeepSeek Harness (dsh) 官方 AI Agent 智能体**
   - **一键内嵌与按需运行**：控制台专设「Agent (dsh)」标签页，支持直接在控制台一键启动/停止官方 `@deepseek-ai/dsh` 智能体。
   - **轻量纯净、零环境强依赖**：主程序依然保持 ~10MB 极简 Go 单文件，用户若不需要可完全不安装 Node.js/npx。
   - **自动化参数注入**：自动生成反代环境变量（`OPENAI_BASE_URL`、`DEEPSEEK_BASE_URL`、专属 API Key、监听端口 `3080`），实现一键内嵌 iframe 交互，随用随启。
   - **多端与 Docker 组合支持**：支持导出独立 `.env` 配置文件与跨机器运行指令；支持 `docker compose --profile agent up -d` 零宿主依赖一键组合拉起。

9. **全自动持续集成（CI/CD）与安全隐私保护**
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

> **挂载说明**：
> - `./auths:/app/auths`：账号授权文件目录（持久化保存在宿主机）
> - `./data:/app/data`：状态快照与模型缓存（包含 `./data/dsh` 智能体工作空间）
> - `./config.json:/app/config.json:ro`：配置文件

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
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "global": {
    "enabled": true
  },
  "pool": {
    "max_in_flight": 3,
    "max_in_flight_global": 2,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m"
  },
  "schedule": {
    "checkin_enabled": true,
    "checkin_hours": [9, 21]
  }
}
```

- **`listen`**：服务监听端口，默认 `:7863`。
- **`api_key`**：访问网关接口所需的 Bearer Token，设为空时则不开启客户端鉴权。
- **`auth_dir`**：保存账号授权文件的目录，默认 `./auths`。
- **`state_file`**：账号池状态与熔断信息持久化路径，默认 `./data/state.json`。
- **`global.enabled`**：是否启用国际版支持，默认 `true`。

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

