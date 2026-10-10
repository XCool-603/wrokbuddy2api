package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usermgr"
)

func (h *Handler) handleAccountAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID    string `json:"uid"`
		Action string `json:"action"` // disable, enable, revive, clear_cooling
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}

	user := h.getWebSessionUser(r)
	if a := h.cfg.Pool.AuthByUID(req.UID); a != nil {
		if !h.isAccountOwnedBy(a, user) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足：只能操作自己名下的账号"})
			return
		}
	}

	switch req.Action {
	case "disable":
		reason := req.Reason
		if reason == "" {
			reason = "Web控制台手动停用"
		}
		h.cfg.Pool.SetManualDisabled(req.UID, true, reason)
	case "enable":
		h.cfg.Pool.SetManualDisabled(req.UID, false, "")
	case "revive":
		h.cfg.Pool.ReviveDisabled(req.UID)
		h.cfg.Pool.ClearCooling(req.UID)
	case "clear_cooling":
		h.cfg.Pool.ClearCooling(req.UID)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知操作"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func isAlreadyCheckinErr(err error) bool {
	if err == nil {
		return false
	}
	var ue *upstream.Error
	msg := err.Error()
	if errors.As(err, &ue) {
		msg = ue.Msg
	}
	low := strings.ToLower(msg)
	for _, code := range []string{"10001", "14001"} {
		if strings.Contains(low, code) {
			return true
		}
	}
	for _, marker := range []string{"已签到", "already", "未开启", "未开放", "已过期"} {
		if strings.Contains(low, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

func (h *Handler) handleActionSignin(w http.ResponseWriter, r *http.Request) {
	authDir := h.getAuthDir()
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"output": "未找到任何账号凭证文件 (" + authDir + " 目录为空)",
		})
		return
	}

	up := h.cfg.Upstream
	if up == nil {
		up = upstream.New()
		up.GlobalEnabled = true
	}

	user := h.getWebSessionUser(r)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-6s | %s\n", "UID", "昵称", "签到状态", "余额", "详情"))
	sb.WriteString("-------------------------------------+--------------+------------+--------+--------------------\n")

	okN, alreadyN, failN := 0, 0, 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-6s | %s\n", filepath.Base(f), "-", "LOAD_ERR", "-", err.Error()))
			failN++
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-6s | %s\n", filepath.Base(f), "-", "PARSE_ERR", "-", err.Error()))
			failN++
			continue
		}
		a.FilePath = f

		// 普通租户只能为属于自己的账号执行手动签到
		if !h.isAccountVisibleTo(a, user) {
			continue
		}

		// token 临近过期时自动刷新
		if a.NeedsRefresh(2 * 3600) {
			if err := up.RefreshToken(a); err == nil {
				a.BackfillRealm()
				_ = a.SaveAtomic()
			}
		}

		err = up.DailyCheckin(a)
		status := "FAIL"
		detail := ""
		if err == nil {
			status = "OK"
			okN++
		} else if isAlreadyCheckinErr(err) {
			status = "ALREADY"
			detail = "今日已签到"
			alreadyN++
		} else {
			status = "FAIL"
			detail = err.Error()
			failN++
		}

		remainStr := "-"
		if remain, qerr := up.UserResource(a); qerr == nil {
			remainStr = fmt.Sprintf("%d", remain)
			h.cfg.Pool.SetCredits(a.UID, remain)
		}

		nick := a.Nickname
		if nick == "" {
			nick = "-"
		}
		sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %-6s | %s\n",
			logfmt.Truncate(a.UID, 36), logfmt.Truncate(nick, 12), status, remainStr, logfmt.Truncate(detail, 30)))
	}

	sb.WriteString(fmt.Sprintf("\n总计: %d | 成功: %d | 已签: %d | 失败: %d\n", len(files), okN, alreadyN, failN))

	writeJSON(w, http.StatusOK, map[string]any{
		"output": sb.String(),
	})
}

func (h *Handler) handleActionTrial(w http.ResponseWriter, r *http.Request) {
	authDir := h.getAuthDir()
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"output": "未找到任何账号凭证文件 (" + authDir + " 目录为空)",
		})
		return
	}

	up := h.cfg.Upstream
	if up == nil {
		up = upstream.New()
		up.GlobalEnabled = true
	}

	user := h.getWebSessionUser(r)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %s\n", "UID", "昵称", "领取状态", "详情"))
	sb.WriteString("-------------------------------------+--------------+------------+--------------------\n")

	okN, alreadyN, naN, failN := 0, 0, 0, 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %s\n", filepath.Base(f), "-", "LOAD_ERR", err.Error()))
			failN++
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %s\n", filepath.Base(f), "-", "PARSE_ERR", err.Error()))
			failN++
			continue
		}
		a.FilePath = f

		if !h.isAccountVisibleTo(a, user) {
			continue
		}

		if !a.IsGlobal() {
			sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %s\n",
				logfmt.Truncate(a.UID, 36), logfmt.Truncate(a.Nickname, 12), "N/A", "国内版账号不适用"))
			naN++
			continue
		}

		claimed, err := up.ClaimTrial(a)
		status := "FAIL"
		detail := ""
		if err != nil {
			status = "FAIL"
			detail = err.Error()
			failN++
		} else if claimed {
			status = "OK"
			detail = "试用额度到账"
			okN++
		} else {
			status = "ALREADY"
			detail = "此前已领取过"
			alreadyN++
		}

		nick := a.Nickname
		if nick == "" {
			nick = "-"
		}
		sb.WriteString(fmt.Sprintf("%-36s | %-12s | %-10s | %s\n",
			logfmt.Truncate(a.UID, 36), logfmt.Truncate(nick, 12), status, logfmt.Truncate(detail, 30)))
	}

	sb.WriteString(fmt.Sprintf("\n总计: %d | 成功: %d | 已领: %d | 忽略: %d | 失败: %d\n", len(files), okN, alreadyN, naN, failN))

	writeJSON(w, http.StatusOK, map[string]any{
		"output": sb.String(),
	})
}

func (h *Handler) handleActionDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Filename string `json:"filename"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Filename == "" || filepath.Dir(req.Filename) != "." {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法文件名"})
		return
	}

	authDir := h.getAuthDir()
	target := filepath.Join(authDir, req.Filename)
	raw, err := os.ReadFile(target)
	if err != nil {
		// 如果磁盘文件不存在，尝试检查是否是纯内存池账号删除
		deletedFromPool := false
		if h.cfg.Pool != nil {
			for _, st := range h.cfg.Pool.List() {
				if fmt.Sprintf("workbuddy-%s.json", st.UID) == req.Filename ||
					fmt.Sprintf("workbuddy-%s-%s.json", st.Realm, st.UID) == req.Filename ||
					st.UID == req.Filename {
					h.cfg.Pool.Remove(st.UID)
					deletedFromPool = true
					break
				}
			}
		}
		if deletedFromPool {
			writeJSON(w, http.StatusOK, map[string]any{"success": true})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "凭证文件不存在"})
		return
	}

	user := h.getWebSessionUser(r)
	var deletedUID string
	if a, err := auth.Parse(raw); err == nil {
		deletedUID = a.UID
		if !h.isAccountOwnedBy(a, user) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足：只能删除自己绑定的账号"})
			return
		}
	}

	_ = os.Remove(target)
	if h.cfg.Pool != nil {
		if deletedUID != "" {
			h.cfg.Pool.Remove(deletedUID)
		}
		for _, st := range h.cfg.Pool.List() {
			if a := h.cfg.Pool.AuthByUID(st.UID); a != nil {
				if a.FilePath == target || filepath.Base(a.FilePath) == req.Filename {
					h.cfg.Pool.Remove(st.UID)
					break
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *Handler) savePersistentSetting(key, val string) {
	// 1. 持久化到 ./data/settings.json（Docker volume 及跨更新持久化目录）
	stateFile := h.cfg.StateFile
	if stateFile == "" {
		stateFile = "./data/state.json"
	}
	settingsDir := filepath.Dir(stateFile)
	_ = os.MkdirAll(settingsDir, 0755)
	settingsPath := filepath.Join(settingsDir, "settings.json")

	var sMap map[string]any
	if raw, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(raw, &sMap)
	}
	if sMap == nil {
		sMap = make(map[string]any)
	}
	sMap[key] = val
	if encoded, err := json.MarshalIndent(sMap, "", "  "); err == nil {
		_ = os.WriteFile(settingsPath, encoded, 0644)
	}

	// 2. 同时尝试更新 config.json（若非只读）
	cfgFile := h.cfg.ConfigPath
	if cfgFile == "" {
		cfgFile = "config.json"
	}
	var dataMap map[string]any
	if raw, err := os.ReadFile(cfgFile); err == nil {
		_ = json.Unmarshal(raw, &dataMap)
	}
	if dataMap == nil {
		dataMap = make(map[string]any)
	}
	dataMap[key] = val
	if encoded, err := json.MarshalIndent(dataMap, "", "  "); err == nil {
		_ = os.WriteFile(cfgFile, encoded, 0644)
	}
}

func (h *Handler) handleConfigAPIKey(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足：仅管理员可修改全局主 API Key"})
		return
	}

	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体 JSON 解析失败"})
		return
	}

	newKey := strings.TrimSpace(req.APIKey)
	h.SetAPIKey(newKey)
	h.savePersistentSetting("api_key", newKey)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"api_key": newKey,
	})
}
