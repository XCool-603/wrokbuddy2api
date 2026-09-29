package dshmgr

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Manager 管理 DeepSeek Harness (dsh) 子进程生命周期、自动安装与状态探测。
type Manager struct {
	mu           sync.RWMutex
	cmd          *exec.Cmd
	cancel       context.CancelFunc
	running      bool
	startedAt    time.Time
	port         int
	recentLogs   []string
	logLimit     int
	workDir      string
	installing   bool
	installError string
}

// New 创建 DSH 管理器。
func New(workDir string) *Manager {
	if workDir == "" {
		workDir = "./data/dsh"
	}
	_ = os.MkdirAll(workDir, 0755)
	return &Manager{
		port:       3080,
		logLimit:   150,
		workDir:    workDir,
		recentLogs: make([]string, 0, 150),
	}
}

// Status 返回 DSH 当前运行状态及环境信息。
type Status struct {
	Installed       bool     `json:"installed"`
	NodeVersion     string   `json:"node_version,omitempty"`
	NpxFound        bool     `json:"npx_found"`
	Installing      bool     `json:"installing"`
	InstallError    string   `json:"install_error,omitempty"`
	Running         bool     `json:"running"`
	ExternalRunning bool     `json:"external_running"`
	Port            int      `json:"port"`
	WebURL          string   `json:"web_url,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
	Logs            []string `json:"logs,omitempty"`
}

// localBinDir 获取内置绿色 Node.js 所在目录。
func (m *Manager) localBinDir() string {
	return filepath.Join(m.workDir, "runtime", "node")
}

func (m *Manager) appendLog(msg string) {
	entry := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg)
	m.recentLogs = append(m.recentLogs, entry)
	if len(m.recentLogs) > m.logLimit {
		m.recentLogs = m.recentLogs[len(m.recentLogs)-m.logLimit:]
	}
}

// resolveNodeNpx 查找可用 node 与 npx 的绝对路径或可执行文件名。
// 优先查找本地绿色运行时目录，若无则查找系统全局 PATH。
func (m *Manager) resolveNodeNpx() (nodePath, npxPath string, ok bool) {
	localDir := m.localBinDir()
	if runtime.GOOS == "windows" {
		localNode := filepath.Join(localDir, "node.exe")
		localNpx := filepath.Join(localDir, "npx.cmd")
		if _, err := os.Stat(localNode); err == nil {
			if _, err2 := os.Stat(localNpx); err2 == nil {
				return localNode, localNpx, true
			}
		}
	} else {
		// Linux / macOS 官方二进制包解压后通常在 bin 目录下，或直接在 localDir 下
		possibleNode := []string{
			filepath.Join(localDir, "bin", "node"),
			filepath.Join(localDir, "node"),
		}
		possibleNpx := []string{
			filepath.Join(localDir, "bin", "npx"),
			filepath.Join(localDir, "npx"),
		}
		for i, nPath := range possibleNode {
			if _, err := os.Stat(nPath); err == nil {
				xPath := possibleNpx[i]
				if _, err2 := os.Stat(xPath); err2 == nil {
					return nPath, xPath, true
				}
			}
		}
	}

	// 查找系统全局 PATH
	sysNode, errNode := exec.LookPath("node")
	if errNode == nil {
		npxName := "npx"
		if runtime.GOOS == "windows" {
			npxName = "npx.cmd"
		}
		sysNpx, errNpx := exec.LookPath(npxName)
		if errNpx != nil && runtime.GOOS == "windows" {
			sysNpx, errNpx = exec.LookPath("npx")
		}
		if errNpx == nil {
			return sysNode, sysNpx, true
		}
	}

	return "", "", false
}

// DetectEnv 探测宿主机或内置 Node.js 与 npx 环境。
func (m *Manager) DetectEnv() (hasNode bool, nodeVer string, hasNpx bool) {
	nodePath, npxPath, ok := m.resolveNodeNpx()
	if !ok {
		return false, "", false
	}

	cmd := exec.Command(nodePath, "-v")
	if out, err := cmd.Output(); err == nil {
		hasNode = true
		nodeVer = strings.TrimSpace(string(out))
		if strings.Contains(nodePath, filepath.Join(m.workDir, "runtime")) {
			nodeVer += " (绿色便携版)"
		}
	}

	if npxPath != "" {
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
		Installing:      m.installing,
		InstallError:    m.installError,
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

	nodeExe, npxExe, ok := m.resolveNodeNpx()
	if !ok {
		return fmt.Errorf("未检测到 Node.js 或 npx 环境。请点击一键自动安装环境，或通过 Docker Compose 启动")
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	_ = os.MkdirAll(m.workDir, 0755)

	// 设置环境变量
	env := os.Environ()
	if gatewayURL == "" {
		gatewayURL = "http://127.0.0.1:7863/v1"
	}

	// 将内置 node 所在目录放入 PATH 首位
	localBin := filepath.Dir(nodeExe)
	pathKey := "PATH"
	for _, e := range env {
		if strings.HasPrefix(strings.ToUpper(e), "PATH=") {
			pathKey = e[:4]
			break
		}
	}
	env = append(env,
		fmt.Sprintf("%s=%s%c%s", pathKey, localBin, os.PathListSeparator, os.Getenv(pathKey)),
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
	m.appendLog(fmt.Sprintf("启动 DeepSeek Harness (端口: %d)...", m.port))

	if err := cmd.Start(); err != nil {
		m.running = false
		m.cancel = nil
		m.cmd = nil
		m.appendLog(fmt.Sprintf("启动失败: %v", err))
		return fmt.Errorf("启动失败: %w", err)
	}

	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		defer m.mu.Unlock()
		m.running = false
		m.appendLog("DeepSeek Harness 进程已退出")
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
	m.appendLog("DeepSeek Harness 已手动停止")
	return nil
}

// AutoInstall 自动下载并解压免安装的 Node.js 绿色便携包。
func (m *Manager) AutoInstall() error {
	m.mu.Lock()
	if m.installing {
		m.mu.Unlock()
		return fmt.Errorf("正在安装中，请稍候...")
	}
	hasNode, _, hasNpx := m.DetectEnv()
	if hasNode && hasNpx {
		m.mu.Unlock()
		return fmt.Errorf("环境已就绪，无需重复安装")
	}
	m.installing = true
	m.installError = ""
	m.appendLog("开始执行 Node.js 便携绿色运行时自动安装任务...")
	m.mu.Unlock()

	go func() {
		err := m.doInstall()
		m.mu.Lock()
		defer m.mu.Unlock()
		m.installing = false
		if err != nil {
			m.installError = err.Error()
			m.appendLog(fmt.Sprintf("自动安装失败: %v", err))
		} else {
			m.appendLog("Node.js 便携绿色环境安装并验证成功！已直接就绪。")
		}
	}()

	return nil
}

func (m *Manager) doInstall() error {
	const nodeVersion = "v20.18.0"
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	var archiveName string
	var isZip bool

	switch goos {
	case "windows":
		arch := "x64"
		if goarch == "arm64" {
			arch = "arm64"
		}
		archiveName = fmt.Sprintf("node-%s-win-%s.zip", nodeVersion, arch)
		isZip = true
	case "linux":
		arch := "x64"
		if goarch == "arm64" {
			arch = "arm64"
		}
		archiveName = fmt.Sprintf("node-%s-linux-%s.tar.gz", nodeVersion, arch)
		isZip = false
	case "darwin":
		arch := "x64"
		if goarch == "arm64" {
			arch = "arm64"
		}
		archiveName = fmt.Sprintf("node-%s-darwin-%s.tar.gz", nodeVersion, arch)
		isZip = false
	default:
		return fmt.Errorf("不支持的操作系统: %s", goos)
	}

	urls := []string{
		fmt.Sprintf("https://npmmirror.com/mirrors/node/%s/%s", nodeVersion, archiveName),
		fmt.Sprintf("https://nodejs.org/dist/%s/%s", nodeVersion, archiveName),
	}

	targetDir := m.localBinDir()
	_ = os.RemoveAll(targetDir)
	_ = os.MkdirAll(filepath.Dir(targetDir), 0755)

	tempFile := filepath.Join(m.workDir, "runtime", archiveName)
	defer os.Remove(tempFile)

	var downloadErr error
	for _, u := range urls {
		m.mu.Lock()
		m.appendLog(fmt.Sprintf("正在从镜像源下载 Node.js (%s)...", u))
		m.mu.Unlock()

		err := downloadFile(u, tempFile)
		if err == nil {
			downloadErr = nil
			break
		}
		downloadErr = err
		m.mu.Lock()
		m.appendLog(fmt.Sprintf("当前镜像源下载失败: %v，尝试备用源...", err))
		m.mu.Unlock()
	}

	if downloadErr != nil {
		return fmt.Errorf("下载运行时失败: %w", downloadErr)
	}

	m.mu.Lock()
	m.appendLog(fmt.Sprintf("下载完成，正在解压部署至 %s ...", targetDir))
	m.mu.Unlock()

	extractDir := filepath.Join(m.workDir, "runtime", "tmp_extract")
	_ = os.RemoveAll(extractDir)
	_ = os.MkdirAll(extractDir, 0755)
	defer os.RemoveAll(extractDir)

	if isZip {
		if err := unzip(tempFile, extractDir); err != nil {
			return fmt.Errorf("解压 zip 失败: %w", err)
		}
	} else {
		if err := untargz(tempFile, extractDir); err != nil {
			return fmt.Errorf("解压 tar.gz 失败: %w", err)
		}
	}

	// 查找解压后的顶层子目录并重命名移动至 targetDir
	entries, err := os.ReadDir(extractDir)
	if err != nil || len(entries) == 0 {
		return fmt.Errorf("解压目录为空")
	}

	var rootSubDir string
	for _, entry := range entries {
		if entry.IsDir() {
			rootSubDir = filepath.Join(extractDir, entry.Name())
			break
		}
	}
	if rootSubDir == "" {
		rootSubDir = extractDir
	}

	if err := os.Rename(rootSubDir, targetDir); err != nil {
		// 跨卷或 rename 失败降级为拷贝
		if cpErr := copyDir(rootSubDir, targetDir); cpErr != nil {
			return fmt.Errorf("部署解压目录失败: %w", cpErr)
		}
	}

	// 给 linux/macOS 二进制文件赋可执行权限
	if runtime.GOOS != "windows" {
		_ = filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				_ = os.Chmod(path, 0755)
			}
			return nil
		})
	}

	// 最终环境验证
	hasNode, ver, hasNpx := m.DetectEnv()
	if !hasNode || !hasNpx {
		return fmt.Errorf("解压完成但验证运行失败 (Node: %v, Npx: %v)", hasNode, hasNpx)
	}

	m.mu.Lock()
	m.appendLog(fmt.Sprintf("便携式 Node.js 环境验证正常: %s", ver))
	m.mu.Unlock()

	return nil
}

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP 状态码异常: %d", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(dest, f.Name)
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			continue // 防 Zip Slip 漏洞
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(fpath, os.ModePerm)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
			return err
		}
		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}
		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func untargz(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		fpath := filepath.Join(dest, header.Name)
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(fpath, 0755)
		case tar.TypeReg:
			_ = os.MkdirAll(filepath.Dir(fpath), 0755)
			outFile, err := os.OpenFile(fpath, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
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
