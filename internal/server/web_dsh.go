package server

import (
	"fmt"
	"net/http"
	"strings"

	"workbuddy2api/internal/usermgr"
)

// DeepSeek Harness (dsh) 管理处理器

func (h *Handler) handleDshStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.DshMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"supported": false,
			"error":     "DSH 管理器未初始化",
		})
		return
	}
	st := h.cfg.DshMgr.GetStatus()
	user := h.getWebSessionUser(r)
	apiKey := h.GetAPIKey()
	if user != nil && user.APIKey != "" {
		apiKey = user.APIKey
	}

	gatewayURL := "http://127.0.0.1:7863/v1"
	if r.Host != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		gatewayURL = fmt.Sprintf("%s://%s/v1", scheme, r.Host)
	}

	dshWebURL := st.WebURL
	if st.Running && r.Host != "" {
		hostOnly := r.Host
		if colon := strings.Index(hostOnly, ":"); colon != -1 {
			hostOnly = hostOnly[:colon]
		}
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		if st.LaunchToken != "" {
			dshWebURL = fmt.Sprintf("%s://%s:%d/?token=%s", scheme, hostOnly, st.Port, st.LaunchToken)
		} else {
			dshWebURL = fmt.Sprintf("%s://%s:%d", scheme, hostOnly, st.Port)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"supported":        true,
		"installed":        st.Installed,
		"node_version":     st.NodeVersion,
		"npx_found":        st.NpxFound,
		"installing":       st.Installing,
		"install_error":    st.InstallError,
		"running":          st.Running,
		"external_running": st.ExternalRunning,
		"is_docker":        isDockerEnvironment(),
		"port":             st.Port,
		"web_url":          dshWebURL,
		"launch_token":     st.LaunchToken,
		"started_at":       st.StartedAt,
		"logs":             st.Logs,
		"gateway_url":      gatewayURL,
		"api_key":          apiKey,
	})
}

func (h *Handler) handleDshInstall(w http.ResponseWriter, r *http.Request) {
	if h.cfg.DshMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "DSH 管理器未启用"})
		return
	}

	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可执行 DSH 安装操作"})
		return
	}

	if err := h.cfg.DshMgr.AutoInstall(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "便携式 Node.js 绿色运行时自动安装已在后台启动...",
	})
}

func (h *Handler) handleDshReset(w http.ResponseWriter, r *http.Request) {
	if h.cfg.DshMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "DSH 管理器未启用"})
		return
	}

	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可重置 DSH 环境"})
		return
	}

	if err := h.cfg.DshMgr.CleanRuntime(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "已清除本地绿色运行时缓存并重置环境检测",
	})
}

func (h *Handler) handleDshStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.DshMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "DSH 管理器未启用"})
		return
	}

	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可启动 DSH 进程"})
		return
	}

	apiKey := h.GetAPIKey()
	if user != nil && user.APIKey != "" {
		apiKey = user.APIKey
	}

	gatewayURL := "http://127.0.0.1:7863/v1"
	if r.Host != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		gatewayURL = fmt.Sprintf("%s://%s/v1", scheme, r.Host)
	}

	if err := h.cfg.DshMgr.Start(gatewayURL, apiKey); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "DeepSeek Harness (dsh) 启动指令已发出，正在启动 Web UI...",
	})
}

func (h *Handler) handleDshStop(w http.ResponseWriter, r *http.Request) {
	if h.cfg.DshMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "DSH 管理器未启用"})
		return
	}

	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可停止 DSH 进程"})
		return
	}

	if err := h.cfg.DshMgr.Stop(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "DeepSeek Harness (dsh) 进程已停止",
	})
}

func (h *Handler) handleDshExport(w http.ResponseWriter, r *http.Request) {
	if h.cfg.DshMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "DSH 管理器未启用"})
		return
	}

	user := h.getWebSessionUser(r)
	apiKey := h.GetAPIKey()
	if user != nil && user.APIKey != "" {
		apiKey = user.APIKey
	}

	gatewayURL := "http://127.0.0.1:7863/v1"
	if r.Host != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		gatewayURL = fmt.Sprintf("%s://%s/v1", scheme, r.Host)
	}

	cfgPath := h.cfg.DshMgr.WriteStandaloneConfig("./data/dsh", gatewayURL, apiKey)
	writeJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"config_path": cfgPath,
		"gateway_url": gatewayURL,
		"api_key":     apiKey,
		"port":        3080,
		"env_content": fmt.Sprintf("OPENAI_BASE_URL=%s\nOPENAI_API_KEY=%s\nPORT=3080\n", gatewayURL, apiKey),
		"launch_cmd":  "node scripts/dsh-bridge.js & npx @deepseek-ai/dsh web --port 3081",
	})
}
