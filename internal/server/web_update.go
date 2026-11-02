package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/sysproc"
	"workbuddy2api/internal/usermgr"
)

func isDockerEnvironment() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if raw, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		if strings.Contains(string(raw), "docker") || strings.Contains(string(raw), "containerd") {
			return true
		}
	}
	return false
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type githubReleaseResp struct {
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	Body        string         `json:"body"`
	PublishedAt string         `json:"published_at"`
	HTMLURL     string         `json:"html_url"`
	Assets      []releaseAsset `json:"assets"`
}

// findMatchingAsset 跨平台精准匹配最新发行版二进制产物（支持 Windows x64/arm64、Linux amd64/arm64、macOS darwin amd64/arm64）
func findMatchingAsset(assets []releaseAsset, goos, goarch string) (string, int64) {
	var fallbackURL string
	var fallbackSize int64

	for _, a := range assets {
		name := strings.ToLower(a.Name)
		switch goos {
		case "windows":
			if goarch == "arm64" {
				if strings.Contains(name, "windows-arm64") || strings.Contains(name, "win-arm64") {
					return a.BrowserDownloadURL, a.Size
				}
			} else {
				if name == "workbuddy2api.exe" {
					return a.BrowserDownloadURL, a.Size
				}
			}
			if strings.HasSuffix(name, ".exe") && fallbackURL == "" {
				fallbackURL = a.BrowserDownloadURL
				fallbackSize = a.Size
			}
		case "linux":
			if goarch == "arm64" && strings.Contains(name, "linux-arm64") {
				return a.BrowserDownloadURL, a.Size
			} else if (goarch == "amd64" || goarch == "386") && strings.Contains(name, "linux-amd64") {
				return a.BrowserDownloadURL, a.Size
			}
		case "darwin":
			if goarch == "arm64" && strings.Contains(name, "darwin-arm64") {
				return a.BrowserDownloadURL, a.Size
			} else if (goarch == "amd64" || goarch == "386") && strings.Contains(name, "darwin-amd64") {
				return a.BrowserDownloadURL, a.Size
			}
		}
	}
	return fallbackURL, fallbackSize
}

func fetchLatestRelease() (*githubReleaseResp, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", "https://api.github.com/repos/XCool-603/wrokbuddy2api/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "workbuddy2api-updater")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var rel githubReleaseResp
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func (h *Handler) handleSystemUpdateCheck(w http.ResponseWriter, r *http.Request) {
	rel, err := fetchLatestRelease()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":         false,
			"current_version": CurrentVersion,
			"error":           "检查最新版本失败: " + err.Error(),
		})
		return
	}

	latestTag := strings.TrimSpace(rel.TagName)
	hasUpdate := false
	if latestTag != "" && latestTag != CurrentVersion {
		hasUpdate = true
	}

	downloadURL, assetSize := findMatchingAsset(rel.Assets, runtime.GOOS, runtime.GOARCH)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"current_version": CurrentVersion,
		"latest_version":  latestTag,
		"has_update":      hasUpdate,
		"release_title":   rel.Name,
		"release_notes":   rel.Body,
		"release_url":     rel.HTMLURL,
		"download_url":    downloadURL,
		"asset_size":      assetSize,
		"is_docker":       isDockerEnvironment(),
	})
}

var updateLock sync.Mutex

// downloadAssetWithFallback 下载更新包，采用 10 秒快速探测 + 国内多镜像自动回退机制。
// 优先直连官方源，若遇国内 GFW 网络屏蔽 (i/o timeout / connection refused) 则自动无缝切换至国内高速加速镜像。
func downloadAssetWithFallback(rawURL string) (*http.Response, string, error) {
	mirrors := []string{
		"", // 优先官方直连
		"https://ghfast.top/",
		"https://ghproxy.net/",
		"https://gh-proxy.com/",
	}

	// 支持环境变量 GH_PROXY / UPDATE_MIRROR 自定义镜像
	if custom := strings.TrimSpace(os.Getenv("GH_PROXY")); custom != "" {
		if !strings.HasSuffix(custom, "/") {
			custom += "/"
		}
		mirrors = append([]string{custom}, mirrors...)
	} else if custom := strings.TrimSpace(os.Getenv("UPDATE_MIRROR")); custom != "" {
		if !strings.HasSuffix(custom, "/") {
			custom += "/"
		}
		mirrors = append([]string{custom}, mirrors...)
	}

	dialer := &net.Dialer{
		Timeout:   10 * time.Second, // 连接超时设为 10 秒，防止直连超时卡死数分钟
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
	}

	var errs []string
	for _, prefix := range mirrors {
		targetURL := rawURL
		sourceDesc := "GitHub 官方直连"
		if prefix != "" {
			targetURL = prefix + rawURL
			sourceDesc = "加速镜像 (" + prefix + ")"
		}

		req, err := http.NewRequest("GET", targetURL, nil)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", sourceDesc, err))
			continue
		}
		req.Header.Set("User-Agent", "workbuddy2api-updater")

		log.Printf("[Updater] 正在尝试从 %s 下载更新包...", sourceDesc)
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[Updater] %s 下载失败: %v，自动切换备用加速源...", sourceDesc, err)
			errs = append(errs, fmt.Sprintf("%s: %v", sourceDesc, err))
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			log.Printf("[Updater] %s 响应异常 (HTTP %d)，自动切换备用加速源...", sourceDesc, resp.StatusCode)
			errs = append(errs, fmt.Sprintf("%s: HTTP %d", sourceDesc, resp.StatusCode))
			continue
		}

		log.Printf("[Updater] 成功连接更新源并开始下载: %s", targetURL)
		return resp, targetURL, nil
	}

	return nil, "", fmt.Errorf("所有下载源均失败: %s", strings.Join(errs, "; "))
}

func (h *Handler) handleSystemUpdateDo(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可执行系统升级"})
		return
	}

	if !updateLock.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "已有升级任务正在进行中，请勿重复操作",
		})
		return
	}
	defer updateLock.Unlock()

	rel, err := fetchLatestRelease()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "获取最新版本信息失败: " + err.Error(),
		})
		return
	}

	targetAssetURL, _ := findMatchingAsset(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if targetAssetURL == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": fmt.Sprintf("最新 Release 中未找到适用于当前系统平台 (%s/%s) 的安装包", runtime.GOOS, runtime.GOARCH),
		})
		return
	}

	currentExe, err := os.Executable()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "定位当前程序路径失败: " + err.Error(),
		})
		return
	}

	// 1. 下载新版本到临时文件 .new (带多镜像自动回退)
	newExePath := currentExe + ".new"
	resp, actualURL, err := downloadAssetWithFallback(targetAssetURL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "下载更新包失败: " + err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	log.Printf("[Updater] 正在从 %s 写入新版本...", actualURL)

	out, err := os.OpenFile(newExePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "无法创建新版本临时文件: " + err.Error(),
		})
		return
	}
	_, copyErr := io.Copy(out, resp.Body)
	out.Close()
	if copyErr != nil {
		_ = os.Remove(newExePath)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "保存更新包数据失败: " + copyErr.Error(),
		})
		return
	}
	_ = os.Chmod(newExePath, 0755)

	// 2. 跨平台二进制替换
	oldExePath := currentExe + ".old"
	_ = os.Remove(oldExePath) // 若存在旧残留先移除
	if err := os.Rename(currentExe, oldExePath); err != nil {
		_ = os.Remove(newExePath)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "重命名当前运行程序失败: " + err.Error(),
		})
		return
	}

	// 3. 将 newExe 重命名为 targetExe
	if err := os.Rename(newExePath, currentExe); err != nil {
		// 回滚
		_ = os.Rename(oldExePath, currentExe)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "替换新版本可执行文件失败: " + err.Error(),
		})
		return
	}

	// 成功响应客户端
	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"latest_version": rel.TagName,
		"message":        "最新版本已成功下载并就绪，系统将在 2 秒后自动重启更新！",
	})

	// 4. 异步重启新进程并退出当前进程
	go func() {
		time.Sleep(1500 * time.Millisecond)
		if isDockerEnvironment() {
			if h.cfg.StopFunc != nil {
				h.cfg.StopFunc()
			}
			os.Exit(0)
			return
		}

		// 本地桌面或独立进程模式：启动新进程并退出旧进程
		cmd := sysproc.HideWindow(exec.Command(currentExe, os.Args[1:]...))
		cmd.Dir = filepath.Dir(currentExe)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Start()

		if h.cfg.StopFunc != nil {
			h.cfg.StopFunc()
		} else {
			os.Exit(0)
		}
	}()
}
