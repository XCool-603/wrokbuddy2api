package server

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/usermgr"
)

func (h *Handler) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可访问"})
		return
	}

	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"users":          []any{},
			"allow_register": true,
			"account_counts": map[string]int{},
		})
		return
	}

	users := h.cfg.UserMgr.ListUsers()
	// 稳定排序：admin 排首位，其余用户按创建时间倒序（最新注册排前）
	sort.Slice(users, func(i, j int) bool {
		if users[i].Role == usermgr.RoleAdmin && users[j].Role != usermgr.RoleAdmin {
			return true
		}
		if users[i].Role != usermgr.RoleAdmin && users[j].Role == usermgr.RoleAdmin {
			return false
		}
		return users[i].CreatedAt.After(users[j].CreatedAt)
	})

	// 统计各用户名下拥有的账号数
	accountCounts := make(map[string]int)
	authDir := h.getAuthDir()
	if files, err := auth.LoadAuthFiles(authDir); err == nil {
		for _, f := range files {
			if raw, err := os.ReadFile(f); err == nil {
				if a, err := auth.Parse(raw); err == nil {
					owner := a.OwnerValue()
					if owner == "" || owner == "admin" || owner == "u_admin" {
						accountCounts["admin"]++
					} else {
						accountCounts[owner]++
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"users":          users,
		"allow_register": h.cfg.UserMgr.AllowRegister(),
		"account_counts": accountCounts,
	})
}

func (h *Handler) handleAdminToggleUser(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可操作"})
		return
	}

	var req struct {
		Username string `json:"username"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求参数错误"})
		return
	}

	if err := h.cfg.UserMgr.ToggleUserDisabled(req.Username, req.Disabled); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *Handler) handleAdminSetUserRole(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可操作"})
		return
	}

	var req struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求参数错误"})
		return
	}

	if err := h.cfg.UserMgr.SetUserRole(req.Username, req.Role); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *Handler) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可操作"})
		return
	}

	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求参数错误"})
		return
	}

	if err := h.cfg.UserMgr.DeleteUser(req.Username); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *Handler) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可创建用户"})
		return
	}
	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "用户管理模块未初始化"})
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求参数解析错误"})
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	req.Password = strings.TrimSpace(req.Password)
	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "用户名和密码不能为空"})
		return
	}

	created, err := h.cfg.UserMgr.CreateUser(req.Username, req.Password, req.Role)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"user": map[string]any{
			"id":         created.ID,
			"username":   created.Username,
			"role":       created.Role,
			"api_key":    created.APIKey,
			"created_at": created.CreatedAt,
		},
	})
}

func (h *Handler) handleAdminSetAllowRegister(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可操作"})
		return
	}

	var req struct {
		Allow bool `json:"allow"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求参数错误"})
		return
	}

	if err := h.cfg.UserMgr.SetAllowRegister(req.Allow); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"allow_register": req.Allow,
	})
}

func (h *Handler) handleAdminResetUserPassword(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可重置用户密码"})
		return
	}
	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "用户系统未初始化"})
		return
	}

	var req struct {
		Username    string `json:"username"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求参数解析错误"})
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.NewPassword = strings.TrimSpace(req.NewPassword)
	if req.Username == "" || req.NewPassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "用户名与新密码均不能为空"})
		return
	}
	if len(req.NewPassword) < 6 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "新密码长度至少 6 位"})
		return
	}

	if err := h.cfg.UserMgr.ChangePassword(req.Username, "", req.NewPassword, true); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 若管理员修改了自己的密码，同步更新 web_password
	if req.Username == "admin" {
		h.SetWebPassword(req.NewPassword)
		h.savePersistentSetting("web_password", req.NewPassword)
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *Handler) handleAdminResetUserAPIKey(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可重置 API Key"})
		return
	}
	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "用户系统未初始化"})
		return
	}

	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求参数解析错误"})
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "用户名不能为空"})
		return
	}

	newKey, err := h.cfg.UserMgr.ResetUserAPIKey(req.Username)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 若重置的是 admin，同步更新全局 API Key
	if req.Username == "admin" {
		h.SetAPIKey(newKey)
		h.savePersistentSetting("api_key", newKey)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"api_key": newKey,
	})
}

