package dshmgr

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Manager 管理 DeepSeek Harness (dsh) 子进程生命周期与状态探测。
type Manager struct {
	mu         sync.RWMutex
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	running    bool
	startedAt  time.Time
	port       int
	recentLogs []string
	logLimit   int
	workDir    string
}

// New 创建 DSH 管理器。
func New(workDir string) *Manager {
	if workDir == "" {
		workDir = "./data/dsh"
	}
	_ = os.MkdirAll(workDir, 0755)
	return &Manager{
		port:       3080,
		logLimit:   100,
		workDir:    workDir,
		recentLogs: make([]string, 0, 100),
	}
}

// Status 返回 DSH 当前运行状态及环境信息。
type Status struct {
	Installed       bool     `json:"installed"`
	NodeVersion     string   `json:"node_version,omitempty"`
	NpxFound        bool     `json:"npx_found"`
	Running         bool     `json:"running"`
	ExternalRunning bool     `json:"external_running"`
	Port            int      `json:"port"`
	WebURL          string   `json:"web_url,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
	Logs            []string `json:"logs,omitempty"`
}

// DetectEnv 探测宿主机 Node.js 与 npx 环境。
func (m *Manager) DetectEnv() (hasNode bool, nodeVer string, hasNpx bool) {
	nodeCmd := exec.Command("node", "-v")
	if out, err := nodeCmd.Output(); err == nil {
		hasNode = true
		nodeVer = string(out)
		if len(nodeVer) > 0 && nodeVer[len(nodeVer)-1] == '\n' {
			nodeVer = nodeVer[:len(nodeVer)-1]
		}
		if len(nodeVer) > 0 && nodeVer[len(nodeVer)-1] == '\r' {
			nodeVer = nodeVer[:len(nodeVer)-1]
		}
	}

	npxCmdName := "npx"
	if runtime.GOOS == "windows" {
		npxCmdName = "npx.cmd"
		if _, err := exec.LookPath(npxCmdName); err != nil {
			npxCmdName = "npx"
		}
	}
	if _, err := exec.LookPath(npxCmdName); err == nil {
		hasNpx = true
	}
	return
}

// GetStatus 获取当前状态。
func (m *Manager) GetStatus() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	hasNode, nodeVer, hasNpx := m.DetectEnv()

	// 探测端口 3080 是否通（支持本地进程或 Docker Compose / 宿主机外置独立进程）
	portReachable := false
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", m.port), 300*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		portReachable = true
	}

	// 尝试探测是否为容器内通过 Docker 网络互通的 dsh 服务（dsh:3080）
	if !portReachable {
		if dshConn, dshErr := net.DialTimeout("tcp", fmt.Sprintf("dsh:%d", m.port), 300*time.Millisecond); dshErr == nil {
			_ = dshConn.Close()
			portReachable = true
		}
	}

	isRunning := m.running || portReachable
	externalRunning := !m.running && portReachable

	logsCopy := make([]string, len(m.recentLogs))
	copy(logsCopy, m.recentLogs)

	st := Status{
		Installed:       hasNode && hasNpx,
		NodeVersion:     nodeVer,
		NpxFound:        hasNpx,
		Running:         isRunning,
		ExternalRunning: externalRunning,
		Port:            m.port,
		Logs:            logsCopy,
	}
	if isRunning {
		st.WebURL = fmt.Sprintf("http://127.0.0.1:%d", m.port)
		if !m.startedAt.IsZero() {
			st.StartedAt = m.startedAt.Format("2006-01-02 15:04:05")
		} else if externalRunning {
			st.StartedAt = "Docker / 外置服务托管中"
		}
	}
	return st
}

// Start 启动 dsh 进程。自动生成配置文件指向本网关。
func (m *Manager) Start(gatewayURL, apiKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return fmt.Errorf("DeepSeek Harness 已经在运行中")
	}

	hasNode, _, hasNpx := m.DetectEnv()
	if !hasNode || !hasNpx {
		return fmt.Errorf("未检测到 Node.js 或 npx 环境。请先安装 Node.js (https://nodejs.org) 或使用 Docker Compose 运行")
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	// 查找 npx 执行文件
	npxExe := "npx"
	if runtime.GOOS == "windows" {
		if path, err := exec.LookPath("npx.cmd"); err == nil {
			npxExe = path
		}
	}

	_ = os.MkdirAll(m.workDir, 0755)

	// 设置环境变量
	env := os.Environ()
	if gatewayURL == "" {
		gatewayURL = "http://127.0.0.1:7863/v1"
	}
	env = append(env,
		fmt.Sprintf("OPENAI_BASE_URL=%s", gatewayURL),
		fmt.Sprintf("OPENAI_API_BASE=%s", gatewayURL),
		fmt.Sprintf("DEEPSEEK_BASE_URL=%s", gatewayURL),
		fmt.Sprintf("OPENAI_API_KEY=%s", apiKey),
		fmt.Sprintf("PORT=%d", m.port),
	)

	cmd := exec.CommandContext(ctx, npxExe, "-y", "@deepseek-ai/dsh", "web", "--port", fmt.Sprintf("%d", m.port))
	cmd.Dir = m.workDir
	cmd.Env = env

	m.cmd = cmd
	m.running = true
	m.startedAt = time.Now()
	m.recentLogs = append(m.recentLogs, fmt.Sprintf("[%s] 启动 DeepSeek Harness (端口: %d)...", time.Now().Format("15:04:05"), m.port))

	if err := cmd.Start(); err != nil {
		m.running = false
		m.cancel = nil
		m.cmd = nil
		return fmt.Errorf("启动失败: %w", err)
	}

	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		defer m.mu.Unlock()
		m.running = false
		m.recentLogs = append(m.recentLogs, fmt.Sprintf("[%s] DeepSeek Harness 进程已退出", time.Now().Format("15:04:05")))
	}()

	return nil
}

// Stop 停止 dsh 进程。
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.running || m.cmd == nil {
		return nil
	}

	if m.cancel != nil {
		m.cancel()
	}
	if m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
	}

	m.running = false
	m.cmd = nil
	m.cancel = nil
	m.recentLogs = append(m.recentLogs, fmt.Sprintf("[%s] DeepSeek Harness 已手动停止", time.Now().Format("15:04:05")))
	return nil
}

// WriteStandaloneConfig 生成供客户端外置运行使用的快捷配置说明与文件。
func (m *Manager) WriteStandaloneConfig(targetDir, gatewayURL, apiKey string) string {
	if gatewayURL == "" {
		gatewayURL = "http://127.0.0.1:7863/v1"
	}
	cfgPath := filepath.Join(targetDir, ".env")
	content := fmt.Sprintf("# Auto-generated by WorkBuddy2API for DeepSeek Harness (dsh)\nOPENAI_BASE_URL=%s\nOPENAI_API_KEY=%s\nPORT=3080\n", gatewayURL, apiKey)
	_ = os.WriteFile(cfgPath, []byte(content), 0644)
	return cfgPath
}
