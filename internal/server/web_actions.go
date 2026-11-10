package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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

		// 国际版账号官方无每日签到活动，明确跳过并提示，与自动签到调度器保持一致
		if a.IsGlobal() {
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
				logfmt.Truncate(a.UID, 36), logfmt.Truncate(nick, 12), "SKIPPED", remainStr, "国际版官方无签到活动"))
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
		removed := false
		for _, st := range h.cfg.Pool.List() {
			if a := h.cfg.Pool.AuthByUID(st.UID); a != nil {
				if a.FilePath == target || filepath.Base(a.FilePath) == req.Filename {
					h.cfg.Pool.Remove(st.UID)
					removed = true
					break
				}
			}
		}
		if !removed && deletedUID != "" {
			h.cfg.Pool.Remove(deletedUID)
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

// handleInFlightData 实时在途流监控数据接口 (F-06)。
func (h *Handler) handleInFlightData(w http.ResponseWriter, r *http.Request) {
	items := h.inFlightTracker.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(items),
		"items": items,
	})
}

// handleAccountUpdateToken 一键重新授权/Token 更新与解封接口 (F-04)。
func (h *Handler) handleAccountUpdateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID          string `json:"uid"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UID == "" || strings.TrimSpace(req.AccessToken) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误：UID 与 AccessToken 必填"})
		return
	}

	user := h.getWebSessionUser(r)
	a := h.cfg.Pool.AuthByUID(req.UID)
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "未找到指定账号"})
		return
	}
	if !h.isAccountOwnedBy(a, user) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足：只能操作自己名下的账号"})
		return
	}

	// 更新凭证值
	a.AccessToken = strings.TrimSpace(req.AccessToken)
	if req.RefreshToken != "" {
		a.RefreshToken = strings.TrimSpace(req.RefreshToken)
	}

	// 激活/解封账号并清空错误与冷却
	h.cfg.Pool.ReviveDisabled(req.UID)
	h.cfg.Pool.ClearCooling(req.UID)
	h.cfg.Pool.NoteSuccess(req.UID)

	// 落盘保存到磁盘文件
	if a.FilePath != "" {
		if raw, err := json.MarshalIndent(a, "", "  "); err == nil {
			_ = os.WriteFile(a.FilePath, raw, 0600)
		}
	}

	// 异步刷新一次该号资产明细
	h.triggerAsyncCreditFetch(a, req.UID, true)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Token 更新成功，账号已解除封禁并恢复健康",
	})
}

// handleAdminSetUserQuota 管理员设置租户配额与 RPM 限制 (F-07)。
func (h *Handler) handleAdminSetUserQuota(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "仅管理员可配置配额"})
		return
	}
	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "用户系统未启用"})
		return
	}
	var req struct {
		Username   string `json:"username"`
		DailyLimit int64  `json:"daily_limit"`
		RPM        int    `json:"rpm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误：用户名必填"})
		return
	}
	if err := h.cfg.UserMgr.SetUserQuota(req.Username, req.DailyLimit, req.RPM); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}



// handleActionWelfare 一键为所有（或指定）账号吃满全套新手礼包、活动补偿、每日签到与试用福利。
func (h *Handler) handleActionWelfare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

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
	sb.WriteString(fmt.Sprintf("%-28s | %-10s | %-8s | %-8s | %-8s | %-8s | %-8s\n", "UID", "新手礼包", "补偿", "签到", "试用", "本次到账", "最新余额"))
	sb.WriteString("-----------------------------+------------+----------+----------+----------+----------+----------\n")

	var results []upstream.WelfareResult
	var totalNewCredits int64
	giftSuccessN, checkinSuccessN, trialSuccessN := 0, 0, 0

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f

		// 用户权限过滤
		if !h.isAccountVisibleTo(a, user) {
			continue
		}

		// 若指定了单个 UID，则只处理该账号
		if req.UID != "" && a.UID != req.UID && auth.CleanRawUID(a.UID) != auth.CleanRawUID(req.UID) {
			continue
		}

		res := up.ClaimAllWelfare(a)
		if res != nil {
			if res.FinalCredits > 0 && h.cfg.Pool != nil {
				h.cfg.Pool.SetCredits(a.UID, res.FinalCredits)
			}
			results = append(results, *res)
			totalNewCredits += res.TotalNewCredits

			if res.GiftStatus == "OK" {
				giftSuccessN++
			}
			if res.CheckinStatus == "OK" {
				checkinSuccessN++
			}
			if res.TrialStatus == "OK" {
				trialSuccessN++
			}

			// 格式化表格显示
			giftCol := res.GiftStatus
			if res.GiftCredit > 0 {
				giftCol = fmt.Sprintf("+%d", res.GiftCredit)
			}
			compCol := res.CompStatus
			if res.CompCredit > 0 {
				compCol = fmt.Sprintf("+%d", res.CompCredit)
			}
			trialCol := res.TrialStatus
			if res.TrialCredit > 0 {
				trialCol = fmt.Sprintf("+%d", res.TrialCredit)
			}
			newCreditsCol := "-"
			if res.TotalNewCredits > 0 {
				newCreditsCol = fmt.Sprintf("+%d", res.TotalNewCredits)
			}
			finalCol := "-"
			if res.FinalCredits > 0 {
				finalCol = fmt.Sprintf("%d", res.FinalCredits)
			}

			sb.WriteString(fmt.Sprintf("%-28s | %-10s | %-8s | %-8s | %-8s | %-8s | %-8s\n",
				logfmt.Truncate(a.UID, 28), giftCol, compCol, res.CheckinStatus, trialCol, newCreditsCol, finalCol))
		}
	}

	sb.WriteString(fmt.Sprintf("\n总计账号: %d | 新手礼包到账: %d | 签到成功: %d | 试用到账: %d | 本轮累计到账算力: +%d\n",
		len(results), giftSuccessN, checkinSuccessN, trialSuccessN, totalNewCredits))

	if h.cfg.RunTasksFunc != nil {
		go h.cfg.RunTasksFunc()
		sb.WriteString("\n[已联动触发后台全量成长任务与最新活动自动点亮领奖 (买得好/抖音乐园/商家宣传/博汪汪/无畏契约拼豆等)]\n")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":           true,
		"total_new_credits": totalNewCredits,
		"results":           results,
		"output":            sb.String(),
	})
}

// handleActionBindInvite 新账号绑定邀请人（填入老号邀请码）。
func (h *Handler) handleActionBindInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID        string `json:"uid"`
		InviteCode string `json:"invite_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UID == "" || strings.TrimSpace(req.InviteCode) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误：账号 UID 与邀请码均为必填"})
		return
	}

	user := h.getWebSessionUser(r)
	a := h.cfg.Pool.AuthByUID(req.UID)
	if a == nil {
		// 尝试从磁盘加载
		authDir := h.getAuthDir()
		files, _ := auth.LoadAuthFiles(authDir)
		for _, f := range files {
			raw, _ := os.ReadFile(f)
			if parsed, perr := auth.Parse(raw); perr == nil && (parsed.UID == req.UID || auth.CleanRawUID(parsed.UID) == auth.CleanRawUID(req.UID)) {
				parsed.FilePath = f
				a = parsed
				break
			}
		}
	}

	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "未找到指定账号"})
		return
	}

	if !h.isAccountOwnedBy(a, user) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足：只能操作自己名下的账号"})
		return
	}

	up := h.cfg.Upstream
	if up == nil {
		up = upstream.New()
	}

	reward, err := up.AcceptInvite(a, req.InviteCode)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   "绑定邀请码失败: " + err.Error(),
		})
		return
	}

	// 绑定成功后刷新资产
	h.triggerAsyncCreditFetch(a, a.UID, true)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"reward":  reward,
		"message": fmt.Sprintf("成功绑定邀请人！恭喜获得邀请奖励积分: +%d", reward),
	})
}

// handleActionTravel 一键为所有（或指定）账号执行猫猫旅行巡检：自动领养 / 派出旅行 / 领奖归来。
func (h *Handler) handleActionTravel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

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
	}

	user := h.getWebSessionUser(r)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-28s | %-12s | %-10s | %-10s | %-16s | %-8s | %-8s\n", "UID", "昵称", "猫猫", "当前状态", "本次动作", "本次到账", "最新余额"))
	sb.WriteString("-----------------------------+--------------+------------+------------+------------------+----------+----------\n")

	departN, claimN, adoptN, skipN, totalReward := 0, 0, 0, 0, int64(0)

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f

		if !h.isAccountVisibleTo(a, user) {
			continue
		}
		if req.UID != "" && a.UID != req.UID && auth.CleanRawUID(a.UID) != auth.CleanRawUID(req.UID) {
			continue
		}

		nick := a.Nickname
		if nick == "" {
			nick = "-"
		}

		if a.IsGlobal() {
			sb.WriteString(fmt.Sprintf("%-28s | %-12s | %-10s | %-10s | %-16s | %-8s | %-8s\n",
				logfmt.Truncate(a.UID, 28), logfmt.Truncate(nick, 12), "-", "N/A", "国际版无猫猫体系", "-", "-"))
			skipN++
			continue
		}

		// 优先使用内存中最新鲜的凭证对象
		if h.cfg.Pool != nil {
			if live := h.cfg.Pool.AuthByUID(a.UID); live != nil {
				a = live
			}
		}
		// token 临近过期时刷新
		if a.NeedsRefresh(2 * 3600) {
			if err := up.RefreshToken(a); err == nil {
				a.BackfillRealm()
				_ = a.SaveAtomic()
			}
		}

		var catName = "-"
		var currentStatus = "未知"
		var action = "无动作"
		var rewardEarned int64 = 0

		// 1. 查猫档案（若遇 401 自动强制刷新并重试一次）
		buddy, bErr := up.BuddyInfo(a)
		if bErr != nil && a.RefreshTokenValue() != "" {
			if rerr := up.RefreshToken(a); rerr == nil {
				a.BackfillRealm()
				_ = a.SaveAtomic()
				buddy, bErr = up.BuddyInfo(a)
			}
		}
		if bErr != nil {
			action = "查猫失败: " + bErr.Error()
		} else if buddy == nil {
			// 无猫：尝试领养；若未满足 5 次对话门槛，自动补发 5 次活跃对话后再领养
			err := up.AdoptBuddyWithUnlock(a)
			if err == nil {
				catName = "新领养"
				action = "领养成功(+300)"
				rewardEarned += 300
				adoptN++
			} else {
				catName = "未领养"
				action = "领养跳过: " + logfmt.Truncate(err.Error(), 24)
			}
		} else {
			catName = buddy.Name
			if catName == "" {
				catName = "已有猫"
			}
		}

		// 2. 查旅行状态
		ts, tsErr := up.TravelStatus(a)
		if tsErr != nil {
			currentStatus = "查询失败"
		} else if ts != nil {
			currentStatus = ts.State
			switch ts.State {
			case "arrived":
				currentStatus = "已到站"
				if ts.RecordID > 0 {
					rw, cErr := up.TravelClaim(a, ts.RecordID)
					if cErr == nil {
						action = fmt.Sprintf("领奖成功(+%d)", rw)
						rewardEarned += rw
						claimN++
					} else {
						action = "领奖失败: " + cErr.Error()
					}
				}
			case "idle":
				currentStatus = "空闲"
				if !ts.DailyLimitReached {
					if dErr := up.TravelDepart(a, 4); dErr == nil {
						action = "已派出旅行"
						departN++
					} else {
						action = "派出失败: " + dErr.Error()
					}
				} else {
					action = "今日已达上限"
					skipN++
				}
			case "traveling":
				currentStatus = "旅行中"
				action = "等待归来"
				skipN++
			}
		}

		totalReward += rewardEarned

		// 查询最新余额
		finalCreditsStr := "-"
		if rem, qerr := up.UserResource(a); qerr == nil {
			finalCreditsStr = fmt.Sprintf("%d", rem)
			if h.cfg.Pool != nil {
				h.cfg.Pool.SetCredits(a.UID, rem)
			}
		}

		rewardCol := "-"
		if rewardEarned > 0 {
			rewardCol = fmt.Sprintf("+%d", rewardEarned)
		}

		sb.WriteString(fmt.Sprintf("%-28s | %-12s | %-10s | %-10s | %-16s | %-8s | %-8s\n",
			logfmt.Truncate(a.UID, 28), logfmt.Truncate(nick, 12), logfmt.Truncate(catName, 10),
			logfmt.Truncate(currentStatus, 10), logfmt.Truncate(action, 16), rewardCol, finalCreditsStr))
	}

	sb.WriteString(fmt.Sprintf("\n总计处理: %d | 派出旅行: %d | 领奖成功: %d | 领养成功: %d | 跳过/待归: %d | 本轮到账积分: +%d\n",
		departN+claimN+adoptN+skipN, departN, claimN, adoptN, skipN, totalReward))



	writeJSON(w, http.StatusOK, map[string]any{
		"success":           true,
		"total_new_credits": totalReward,
		"output":            sb.String(),
	})
}

// onboardNewAccountAsync 新账号入池后异步同步实时余额，并为国内版无猫账号自动解锁并领养第一只猫。
func (h *Handler) onboardNewAccountAsync(a *auth.Auth) {
	if h == nil || h.cfg.Upstream == nil || a == nil || a.AccessTokenValue() == "" {
		return
	}
	go func(acct *auth.Auth) {
		up := h.cfg.Upstream
		if rem, expiring, _, _, err := up.ResourceSummary(acct); err == nil && h.cfg.Pool != nil {
			h.cfg.Pool.SetCreditsDetailed(acct.UID, rem, expiring)
		}
		if acct.IsGlobal() {
			return
		}
		buddy, err := up.BuddyInfo(acct)
		if err != nil || buddy != nil {
			return
		}
		if err := up.AdoptBuddyWithUnlock(acct); err == nil {
			log.Printf("INFO: [onboard] auto adopted buddy for new account %s (+300)", logfmt.Label(acct.UID, acct.Nickname))
			if rem, expiring, _, _, qerr := up.ResourceSummary(acct); qerr == nil && h.cfg.Pool != nil {
				h.cfg.Pool.SetCreditsDetailed(acct.UID, rem, expiring)
			}
		}
	}(a)
}

func (h *Handler) resolveTargetAccounts(authDir string, user *usermgr.User, targetUID string) []*auth.Auth {
	files, _ := auth.LoadAuthFiles(authDir)
	seen := make(map[string]bool)
	var out []*auth.Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if !h.isAccountVisibleTo(a, user) {
			continue
		}
		if targetUID != "" && a.UID != targetUID && auth.CleanRawUID(a.UID) != auth.CleanRawUID(targetUID) {
			continue
		}
		if h.cfg.Pool != nil {
			if live := h.cfg.Pool.AuthByUID(a.UID); live != nil {
				if live.FilePath == "" {
					live.FilePath = f
				}
				a = live
			}
		}
		if seen[a.UID] {
			continue
		}
		seen[a.UID] = true
		out = append(out, a)
	}
	// 若磁盘尚未落盘（例如纯内存测试池），兜底从 Pool 补充
	if len(out) == 0 && h.cfg.Pool != nil {
		for _, st := range h.cfg.Pool.List() {
			a := h.cfg.Pool.AuthByUID(st.UID)
			if a == nil || !h.isAccountVisibleTo(a, user) {
				continue
			}
			if targetUID != "" && a.UID != targetUID && auth.CleanRawUID(a.UID) != auth.CleanRawUID(targetUID) {
				continue
			}
			out = append(out, a)
		}
	}
	return out
}

// handleActionTasks 手动触发单账号或全部可控账号的成长任务、最新活动与自动领猫，并同步返回执行日志与到账增量。
func (h *Handler) handleActionTasks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "请求参数解析失败"})
			return
		}
	}
	req.UID = strings.TrimSpace(req.UID)

	// 兼容仅配置了 RunTasksFunc 且未传 UID 的旧调用/单测路径
	if req.UID == "" && h.cfg.RunTasksAccountsFunc == nil && h.cfg.RunTasksFunc != nil {
		go h.cfg.RunTasksFunc()
		msg := `======================================================================
★ 全量成长任务与最新活动自动化流水线已成功触发并在后台执行！
======================================================================
已自动为您排程覆盖以下 8 大活动与日常积分任务：
  1. [买得好专家]         expert_use_maidehao        (+50 积分)
  2. [抖音乐园互动]       expert_use_douyinyin       (+50 积分)
  3. [商家宣传达人]       expert_use_shangjia_xuanchuan (+50 积分)
  4. [博汪汪宠物专家]     expert_use_bowangwang      (+50 积分)
  5. [无畏契约拼豆图纸]   WorkBuddy_Valorant         (+50 积分)
  6. [微信公众号订阅]     wb_wechat_oa_subscribe_task (+50 积分)
  7. [夜猫子灵感对话]     black_cat (GLM-5.2)        (+30 积分，时段窗口判定)
  8. [开学季与常规日常]   school_open_day & daily chat

任务已在后台平稳执行（自动做账号限速防风控），完成后积分将自动注入您的账号池！`
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"output":  msg,
		})
		return
	}

	user := h.getWebSessionUser(r)
	authDir := h.getAuthDir()
	accts := h.resolveTargetAccounts(authDir, user, req.UID)
	if len(accts) == 0 {
		msg := "未找到任何可控账号凭证"
		if req.UID != "" {
			msg = "未找到指定账号或无权限操作"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   msg,
			"output":  msg,
		})
		return
	}

	up := h.cfg.Upstream
	if up == nil {
		up = upstream.New()
		up.GlobalEnabled = true
	}

	var cnAccts []*auth.Auth
	var targets []string
	var globalSkipped []*auth.Auth
	beforeCredits := make(map[string]int64, len(accts))

	for _, a := range accts {
		if a.IsGlobal() {
			globalSkipped = append(globalSkipped, a)
			continue
		}
		if a.NeedsRefresh(2*time.Hour) && a.RefreshTokenValue() != "" {
			if err := up.RefreshToken(a); err == nil {
				a.BackfillRealm()
				_ = a.SaveAtomic()
			}
		}
		if rem, _, err := up.UserResourceDetailed(a, 0); err == nil {
			beforeCredits[a.UID] = rem
		} else if h.cfg.Pool != nil {
			if st, ok := h.cfg.Pool.Status(a.UID); ok {
				beforeCredits[a.UID] = st.Credits
			}
		}
		cnAccts = append(cnAccts, a)
		if a.FilePath != "" {
			if abs, err := filepath.Abs(a.FilePath); err == nil {
				targets = append(targets, abs)
			} else {
				targets = append(targets, filepath.Base(a.FilePath))
			}
		} else {
			targets = append(targets, a.UID)
		}
	}

	if len(cnAccts) == 0 {
		var sb strings.Builder
		sb.WriteString("国际版账号无国内版成长任务体系，已跳过：\n")
		for _, a := range globalSkipped {
			sb.WriteString(fmt.Sprintf("  - %s (%s)\n", a.UID, a.Nickname))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success":           true,
			"total_new_credits": 0,
			"output":            sb.String(),
		})
		return
	}

	var scriptOut string
	var runErr error
	if h.cfg.RunTasksAccountsFunc != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		scriptOut, runErr = h.cfg.RunTasksAccountsFunc(ctx, targets, authDir)
	} else if h.cfg.RunTasksFunc != nil {
		h.cfg.RunTasksFunc()
		scriptOut = "[RunTasksFunc 已执行]\n"
	} else {
		runErr = errors.New("未配置任务执行器 RunTasksAccountsFunc")
	}

	// 兜底确保无猫 CN 账号自动补对话解锁并领养，并同步实时余额
	var sb strings.Builder
	sb.WriteString("=== 手动成长任务与最新活动执行报告 ===\n")
	sb.WriteString(fmt.Sprintf("%-28s | %-12s | %-10s | %-10s | %-10s\n", "UID", "昵称", "猫猫状态", "本轮到账", "最新积分"))
	sb.WriteString("-----------------------------+--------------+------------+------------+-----------\n")

	var totalNew int64
	for _, a := range cnAccts {
		nick := a.Nickname
		if nick == "" {
			nick = "-"
		}
		catState := "-"
		if buddy, berr := up.BuddyInfo(a); berr == nil {
			if buddy == nil {
				if aerr := up.AdoptBuddyWithUnlock(a); aerr == nil {
					catState = "新领养"
				} else {
					catState = "未领养"
				}
			} else {
				catState = buddy.Name
				if catState == "" {
					catState = "已有猫"
				}
			}
		}
		after := beforeCredits[a.UID]
		if rem, buckets, qerr := up.UserResourceDetailed(a, 0); qerr == nil {
			after = rem
			if h.cfg.Pool != nil {
				h.cfg.Pool.SetCreditsDetailed(a.UID, rem, buckets.Expiring)
			}
		}
		delta := after - beforeCredits[a.UID]
		if delta < 0 {
			delta = 0
		}
		totalNew += delta
		deltaStr := "-"
		if delta > 0 {
			deltaStr = fmt.Sprintf("+%d", delta)
		}
		sb.WriteString(fmt.Sprintf("%-28s | %-12s | %-10s | %-10s | %-10d\n",
			logfmt.Truncate(a.UID, 28), logfmt.Truncate(nick, 12), logfmt.Truncate(catState, 10), deltaStr, after))
	}
	for _, a := range globalSkipped {
		nick := a.Nickname
		if nick == "" {
			nick = "-"
		}
		sb.WriteString(fmt.Sprintf("%-28s | %-12s | %-10s | %-10s | %-10s\n",
			logfmt.Truncate(a.UID, 28), logfmt.Truncate(nick, 12), "SKIP(国际)", "-", "-"))
	}
	sb.WriteString(fmt.Sprintf("\n国内版处理: %d | 国际版跳过: %d | 本轮净增积分: +%d\n",
		len(cnAccts), len(globalSkipped), totalNew))
	if runErr != nil {
		sb.WriteString(fmt.Sprintf("脚本执行异常: %v\n", runErr))
	}
	if strings.TrimSpace(scriptOut) != "" {
		sb.WriteString("\n--- task_runner 详细输出 ---\n")
		sb.WriteString(scriptOut)
		if !strings.HasSuffix(scriptOut, "\n") {
			sb.WriteString("\n")
		}
	}

	resp := map[string]any{
		"success":           runErr == nil,
		"total_new_credits": totalNew,
		"output":            sb.String(),
	}
	if runErr != nil {
		resp["error"] = runErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}
