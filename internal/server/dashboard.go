package server

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usermgr"
)

//go:embed dashboard.html
var dashboardHTML string

func (h *Handler) RegisterWebUI() {
	h.mux.HandleFunc("GET /{$}", h.handleDashboardHTML)
	h.mux.HandleFunc("POST /ui/auth/login", h.handleWebLogin)
	h.mux.HandleFunc("POST /ui/auth/logout", h.handleWebLogout)
	h.mux.HandleFunc("POST /ui/auth/register", h.handleWebRegister)

	h.mux.HandleFunc("GET /ui/data", h.withWebAuth(h.handleDashboardData))
	h.mux.HandleFunc("POST /ui/oauth/start", h.withWebAuth(h.handleOAuthStart))
	h.mux.HandleFunc("POST /ui/oauth/poll", h.withWebAuth(h.handleOAuthPoll))
	h.mux.HandleFunc("POST /ui/oauth/import", h.withWebAuth(h.handleOAuthImport))
	h.mux.HandleFunc("POST /ui/account/admin", h.withWebAuth(h.handleAccountAdmin))
	h.mux.HandleFunc("POST /ui/action/signin", h.withWebAuth(h.handleActionSignin))
	h.mux.HandleFunc("POST /ui/action/trial", h.withWebAuth(h.handleActionTrial))
	h.mux.HandleFunc("POST /ui/action/delete", h.withWebAuth(h.handleActionDelete))
	h.mux.HandleFunc("POST /ui/config/apikey", h.withWebAuth(h.handleConfigAPIKey))
	h.mux.HandleFunc("POST /ui/config/password", h.withWebAuth(h.handleConfigPassword))
	h.mux.HandleFunc("GET /ui/action/backup", h.withWebAuth(h.handleActionBackup))
	h.mux.HandleFunc("GET /ui/system/update/check", h.withWebAuth(h.handleSystemUpdateCheck))
	h.mux.HandleFunc("POST /ui/system/update/do", h.withWebAuth(h.handleSystemUpdateDo))

	// 用户中心与多用户管理端点
	h.mux.HandleFunc("POST /ui/user/apikey/reset", h.withWebAuth(h.handleUserAPIKeyReset))
	h.mux.HandleFunc("GET /ui/admin/users", h.withWebAuth(h.handleAdminListUsers))
	h.mux.HandleFunc("POST /ui/admin/user/toggle", h.withWebAuth(h.handleAdminToggleUser))
	h.mux.HandleFunc("POST /ui/admin/user/role", h.withWebAuth(h.handleAdminSetUserRole))
	h.mux.HandleFunc("POST /ui/admin/user/delete", h.withWebAuth(h.handleAdminDeleteUser))
	h.mux.HandleFunc("POST /ui/admin/system/allow_register", h.withWebAuth(h.handleAdminSetAllowRegister))
}

func (h *Handler) handleDashboardHTML(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dashboardHTML))
}

func (h *Handler) handleDashboardData(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}

	user := h.getWebSessionUser(r)

	authDir := "./auths"
	files, _ := auth.LoadAuthFiles(authDir)
	type AccountItem struct {
		UID            string `json:"uid"`
		Realm          string `json:"realm"`
		Nickname       string `json:"nickname"`
		Filename       string `json:"filename"`
		Credits        int64  `json:"credits"`
		Disabled       bool   `json:"disabled"`
		DisabledReason string `json:"disabled_reason"`
		ManualDisabled bool   `json:"manual_disabled"`
		ManualReason   string `json:"manual_reason"`
		Cooling        bool   `json:"cooling"`
		Owner          string `json:"owner"`
		IsMyAccount    bool   `json:"is_my_account"`
	}

	poolList := h.cfg.Pool.List()
	poolMap := make(map[string]pool.Status)
	for _, p := range poolList {
		poolMap[p.UID] = p
	}

	accounts := make([]AccountItem, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}

		// 账号隔离逻辑：
		// 如果当前登录的是普通用户（非 admin），只列出属于该用户自己的账号或公共系统账号
		isMine := false
		if user != nil {
			if a.Owner == user.ID || a.Owner == user.Username {
				isMine = true
			}
			if user.Role != usermgr.RoleAdmin {
				if !isMine && a.Owner != "" && a.Owner != "admin" {
					// 不属于公共且不属于该普通用户，跳过（隔离保密）
					continue
				}
			}
		}

		item := AccountItem{
			UID:         a.UID,
			Realm:       a.Realm(),
			Nickname:    a.Nickname,
			Filename:    filepath.Base(f),
			Owner:       a.Owner,
			IsMyAccount: isMine,
		}
		if p, ok := poolMap[a.UID]; ok {
			item.Credits = p.Credits
			item.Disabled = p.Disabled
			item.DisabledReason = p.DisabledReason
			item.ManualDisabled = p.ManualDisabled
			item.ManualReason = p.ManualReason
			item.Cooling = p.Cooling
		}
		// 若池内积分为 0 或未初始化，尝试直接向上游查询真实积分并回填到账号池
		if item.Credits == 0 && a.AccessTokenValue() != "" && h.cfg.Upstream != nil {
			if remain, _, _, _, err := h.cfg.Upstream.ResourceSummary(a); err == nil {
				item.Credits = remain
				h.cfg.Pool.SetCredits(a.UID, remain)
			}
		}
		accounts = append(accounts, item)
	}

	models := h.modelList()
	statsSnap := MetricsSnapshotOf()
	h.enrichCredits(&statsSnap)

	// APIKey：普通用户显示用户自己的 APIKey，管理员显示全局 APIKey
	displayAPIKey := h.GetAPIKey()
	if user != nil && user.APIKey != "" {
		displayAPIKey = user.APIKey
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service": ServiceName,
		"status": map[string]any{
			"total":           total,
			"healthy":         healthy,
			"cooling":         cooling,
			"disabled":        disabled,
			"in_flight_full":  inFlightFull,
			"sticky_sessions": sticky,
		},
		"accounts":       accounts,
		"models":         models,
		"apiKey":         displayAPIKey,
		"hasPassword":    h.GetWebPassword() != "",
		"currentVersion": CurrentVersion,
		"isDocker":       isDockerEnvironment(),
		"recentLogs":     GetRecentLogs(),
		"stats":          statsSnap,
		"currentUser":    user,
		"allowRegister":  h.cfg.UserMgr != nil && h.cfg.UserMgr.AllowRegister(),
	})
}

var (
	oauthStateMutex sync.Mutex
	oauthStateStore = make(map[string]string) // realm -> state
)

func oauthConfig(realm string) (base, origin string) {
	if realm == "global" {
		return "https://www.workbuddy.ai", "https://www.workbuddy.ai"
	}
	return "https://copilot.tencent.com", "https://www.codebuddy.cn"
}

func oauthHeaders(origin string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")
	}
}

type oauthEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func oauthDoJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env oauthEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func (h *Handler) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Realm != "global" {
		req.Realm = "cn"
	}

	base, origin := oauthConfig(req.Realm)
	client := &http.Client{Timeout: 15 * time.Second}
	headers := oauthHeaders(origin)
	data, _, err := oauthDoJSON(client, http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": fmt.Sprintf("获取授权状态失败: %v", err),
		})
		return
	}

	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "解析上游授权状态失败: 返回数据不完整",
		})
		return
	}

	oauthStateMutex.Lock()
	oauthStateStore[req.Realm] = st.State
	oauthStateMutex.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"url":   strings.TrimSpace(st.AuthURL),
		"realm": req.Realm,
	})
}

func (h *Handler) handleOAuthPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Realm != "global" {
		req.Realm = "cn"
	}

	oauthStateMutex.Lock()
	state := oauthStateStore[req.Realm]
	oauthStateMutex.Unlock()

	if state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "无正在进行的登录请求，请先点击发起登录",
		})
		return
	}

	base, origin := oauthConfig(req.Realm)
	client := &http.Client{Timeout: 15 * time.Second}
	headers := oauthHeaders(origin)

	tokRaw, status, errTok := oauthDoJSON(client, http.MethodGet, base+"/v2/plugin/auth/token?state="+state, headers, nil)
	if errTok != nil {
		if status == 0 || status >= 500 {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": fmt.Sprintf("查询登录状态异常: %v", errTok),
			})
			return
		}
		// 尚在等待登录完成
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"waiting": true,
		})
		return
	}

	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"waiting": true,
		})
		return
	}

	// 成功获取到 token，接着获取账号信息
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(req *http.Request) {
		headers(req)
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := oauthDoJSON(client, http.MethodGet, base+"/v2/plugin/login/account?state="+state, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	uid := acct.UID
	if uid == "" {
		uid = fmt.Sprintf("account_%d", time.Now().Unix())
	}
	nickname := acct.Nickname
	entID := acct.EnterpriseID
	expiresAt := time.Now().Unix() + 7200
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + tok.ExpiresIn
	}

	user := h.getWebSessionUser(r)
	ownerID := ""
	if user != nil {
		ownerID = user.ID
	}

	doc := map[string]any{
		"account": map[string]any{
			"uid":          uid,
			"enterpriseId": entID,
			"nickname":     nickname,
		},
		"auth": map[string]any{
			"accessToken":  tok.AccessToken,
			"refreshToken": tok.RefreshToken,
			"expiresAt":    expiresAt,
			"domain":       tok.Domain,
			"realm":        req.Realm,
		},
	}
	if ownerID != "" {
		doc["owner"] = ownerID
	}
	docBytes, _ := json.MarshalIndent(doc, "", "  ")

	_ = os.MkdirAll("./auths", 0755)
	targetFile := filepath.Join("./auths", fmt.Sprintf("workbuddy-%s.json", uid))
	if err := os.WriteFile(targetFile, docBytes, 0644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": fmt.Sprintf("保存授权文件失败: %v", err),
		})
		return
	}

	// 清理当前状态
	oauthStateMutex.Lock()
	if oauthStateStore[req.Realm] == state {
		delete(oauthStateStore, req.Realm)
	}
	oauthStateMutex.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"uid":      uid,
		"filename": filepath.Base(targetFile),
		"nickname": nickname,
		"realm":    req.Realm,
	})
}

func (h *Handler) handleOAuthImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSON string `json:"json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.JSON) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 凭证内容为空"})
		return
	}

	raw := []byte(strings.TrimSpace(req.JSON))
	a, err := auth.Parse(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("解析凭证失败: %v", err)})
		return
	}

	uid := a.UID
	if uid == "" {
		uid = fmt.Sprintf("account_%d", time.Now().Unix())
	}

	user := h.getWebSessionUser(r)
	if user != nil {
		a.Owner = user.ID
	}

	_ = os.MkdirAll("./auths", 0755)
	targetFile := filepath.Join("./auths", fmt.Sprintf("workbuddy-%s.json", uid))
	a.FilePath = targetFile
	if err := a.SaveAtomic(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("保存失败: %v", err)})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"uid":      uid,
		"filename": filepath.Base(targetFile),
		"realm":    a.Realm(),
		"nickname": a.Nickname,
	})
}

func (h *Handler) handleAccountAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID    string `json:"uid"`
		Action string `json:"action"` // disable, enable, revive
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
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
	authDir := "./auths"
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"output": "未找到任何账号凭证文件 (auths/ 目录为空)",
		})
		return
	}

	up := h.cfg.Upstream
	if up == nil {
		up = upstream.New()
		up.GlobalEnabled = true
	}

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
	authDir := "./auths"
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"output": "未找到任何账号凭证文件 (auths/ 目录为空)",
		})
		return
	}

	up := h.cfg.Upstream
	if up == nil {
		up = upstream.New()
		up.GlobalEnabled = true
	}

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

	target := filepath.Join("auths", req.Filename)
	raw, err := os.ReadFile(target)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "凭证文件不存在"})
		return
	}

	user := h.getWebSessionUser(r)
	if user != nil && user.Role != usermgr.RoleAdmin {
		// 普通用户只能删除自己名下的账号
		if a, err := auth.Parse(raw); err == nil {
			if a.Owner != user.ID && a.Owner != user.Username {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足：只能删除自己绑定的账号"})
				return
			}
		}
	}

	_ = os.Remove(target)
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

func (h *Handler) handleActionBackup(w http.ResponseWriter, r *http.Request) {
	files, err := auth.LoadAuthFiles("./auths")
	if err != nil {
		http.Error(w, "无法读取凭证目录: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=workbuddy2api_auths_backup_%s.zip", time.Now().Format("20060102_150405")))

	zw := zip.NewWriter(w)
	defer zw.Close()

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		base := filepath.Base(f)
		fw, err := zw.Create(base)
		if err != nil {
			continue
		}
		_, _ = fw.Write(data)
	}
}

// webSessionCookieName Web 控制台会话 Cookie 名
const webSessionCookieName = "wb2a_session"

type webSessionInfo struct {
	User      *usermgr.User
	ExpiresAt time.Time
}

// webSessions 存储已登录的 Web 会话 (token -> webSessionInfo)
var webSessions sync.Map

func generateSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *Handler) getWebSessionUser(r *http.Request) *usermgr.User {
	cookie, err := r.Cookie(webSessionCookieName)
	if err == nil && cookie.Value != "" {
		if val, ok := webSessions.Load(cookie.Value); ok {
			if sess, ok := val.(webSessionInfo); ok {
				if time.Now().Before(sess.ExpiresAt) {
					// 刷新最新用户状态（从 UserMgr 获取以防被禁用或被改角色）
					if h.cfg.UserMgr != nil && sess.User != nil {
						if fresh, ok := h.cfg.UserMgr.FindByUsername(sess.User.Username); ok {
							return fresh
						}
					}
					return sess.User
				}
				webSessions.Delete(cookie.Value)
			}
		}
	}

	// 兼容 X-Web-Password 或者是免密单机模式，默认返回管理员
	if h.cfg.UserMgr != nil {
		if admin, ok := h.cfg.UserMgr.FindByUsername("admin"); ok {
			return admin
		}
	}
	return nil
}

func (h *Handler) checkWebAuth(r *http.Request) bool {
	pw := h.GetWebPassword()

	// 1. 支持 Header 鉴权: X-Web-Password
	if customPw := r.Header.Get("X-Web-Password"); customPw != "" && pw != "" {
		if subtle.ConstantTimeCompare([]byte(customPw), []byte(pw)) == 1 {
			return true
		}
	}

	// 2. Cookie 会话校验
	cookie, err := r.Cookie(webSessionCookieName)
	if err == nil && cookie.Value != "" {
		if val, ok := webSessions.Load(cookie.Value); ok {
			if sess, ok := val.(webSessionInfo); ok {
				if time.Now().Before(sess.ExpiresAt) {
					// 检查用户是否被禁用
					if h.cfg.UserMgr != nil && sess.User != nil {
						if fresh, ok := h.cfg.UserMgr.FindByUsername(sess.User.Username); ok && fresh.Disabled {
							webSessions.Delete(cookie.Value)
							return false
						}
					}
					return true
				}
				webSessions.Delete(cookie.Value)
			}
		}
	}

	// 未设密码且未启用多用户注册模式时，单机免密放行
	if pw == "" && (h.cfg.UserMgr == nil) {
		return true
	}
	return false
}

func (h *Handler) withWebAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.checkWebAuth(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":          "需要登录控制台",
				"need_login":     true,
				"has_password":   h.GetWebPassword() != "",
				"allow_register": h.cfg.UserMgr != nil && h.cfg.UserMgr.AllowRegister(),
			})
			return
		}
		next(w, r)
	}
}

func (h *Handler) handleWebLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	req.Password = strings.TrimSpace(req.Password)

	var loggedUser *usermgr.User

	// 1. 如果启用了 UserMgr，优先走多用户体系
	if h.cfg.UserMgr != nil {
		// 如果未传用户名，默认按 admin 登录
		uname := req.Username
		if uname == "" {
			uname = "admin"
		}
		u, err := h.cfg.UserMgr.Authenticate(uname, req.Password)
		if err != nil {
			// 如果尝试 admin 失败，且此时配置了独立的 WebPassword，尝试核验 WebPassword
			pw := h.GetWebPassword()
			if uname == "admin" && pw != "" && subtle.ConstantTimeCompare([]byte(req.Password), []byte(pw)) == 1 {
				if admin, ok := h.cfg.UserMgr.FindByUsername("admin"); ok {
					loggedUser = admin
				}
			}
			if loggedUser == nil {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
				return
			}
		} else {
			loggedUser = u
		}
	} else {
		// 传统单机模式：核验 WebPassword
		pw := h.GetWebPassword()
		if pw != "" && subtle.ConstantTimeCompare([]byte(req.Password), []byte(pw)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "控制台访问密码错误"})
			return
		}
	}

	token := generateSessionToken()
	exp := time.Now().Add(7 * 24 * time.Hour) // 7 天有效期
	webSessions.Store(token, webSessionInfo{
		User:      loggedUser,
		ExpiresAt: exp,
	})

	http.SetCookie(w, &http.Cookie{
		Name:     webSessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"user":    loggedUser,
	})
}

func (h *Handler) handleWebRegister(w http.ResponseWriter, r *http.Request) {
	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未启用用户系统"})
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}

	u, err := h.cfg.UserMgr.Register(req.Username, req.Password)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 注册成功自动创建会话登录
	token := generateSessionToken()
	exp := time.Now().Add(7 * 24 * time.Hour)
	webSessions.Store(token, webSessionInfo{
		User:      u,
		ExpiresAt: exp,
	})

	http.SetCookie(w, &http.Cookie{
		Name:     webSessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"user":    u,
	})
}

func (h *Handler) handleWebLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(webSessionCookieName); err == nil && cookie.Value != "" {
		webSessions.Delete(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     webSessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
	})

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *Handler) handleUserAPIKeyReset(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}

	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未启用用户系统"})
		return
	}

	newKey, err := h.cfg.UserMgr.ResetUserAPIKey(user.Username)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// 如果当前是 admin 用户，同时更新并持久化主全局 APIKey
	if user.Role == usermgr.RoleAdmin || user.Username == "admin" {
		h.SetAPIKey(newKey)
		h.savePersistentSetting("api_key", newKey)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"api_key": newKey,
	})
}

func (h *Handler) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足，仅管理员可访问"})
		return
	}

	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"users": []any{}})
		return
	}

	users := h.cfg.UserMgr.ListUsers()
	writeJSON(w, http.StatusOK, map[string]any{
		"users":          users,
		"allow_register": h.cfg.UserMgr.AllowRegister(),
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

func (h *Handler) handleConfigPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体 JSON 解析失败"})
		return
	}

	user := h.getWebSessionUser(r)
	if user != nil && h.cfg.UserMgr != nil {
		// 用户中心修改个人密码
		if err := h.cfg.UserMgr.ChangePassword(user.Username, req.OldPassword, req.NewPassword, false); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
			return
		}
		// 如果是 admin，同步更新 web_password
		if user.Role == usermgr.RoleAdmin || user.Username == "admin" {
			newPw := strings.TrimSpace(req.NewPassword)
			h.SetWebPassword(newPw)
			h.savePersistentSetting("web_password", newPw)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success":      true,
			"has_password": true,
		})
		return
	}

	currentPw := h.GetWebPassword()
	// 如果当前已设置了密码，修改时需要核验旧密码
	if currentPw != "" {
		if subtle.ConstantTimeCompare([]byte(req.OldPassword), []byte(currentPw)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "原管理密码不正确"})
			return
		}
	}

	newPw := strings.TrimSpace(req.NewPassword)
	h.SetWebPassword(newPw)
	h.savePersistentSetting("web_password", newPw)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"has_password": newPw != "",
	})
}

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

type githubReleaseResp struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
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
		// 简单版本比较：tag 不相等即提示有更新
		hasUpdate = true
	}

	var downloadURL string
	var assetSize int64
	for _, a := range rel.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".exe") {
			downloadURL = a.BrowserDownloadURL
			assetSize = a.Size
			break
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":          true,
		"current_version":  CurrentVersion,
		"latest_version":   latestTag,
		"has_update":       hasUpdate,
		"release_title":    rel.Name,
		"release_notes":    rel.Body,
		"release_url":      rel.HTMLURL,
		"download_url":     downloadURL,
		"asset_size":       assetSize,
		"is_docker":        isDockerEnvironment(),
	})
}

var updateLock sync.Mutex

func (h *Handler) handleSystemUpdateDo(w http.ResponseWriter, r *http.Request) {
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

	var targetAssetURL string
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	// 匹配对应的资产文件
	for _, a := range rel.Assets {
		name := strings.ToLower(a.Name)
		if goos == "windows" && strings.HasSuffix(name, ".exe") {
			targetAssetURL = a.BrowserDownloadURL
			break
		} else if goos == "linux" {
			if goarch == "arm64" && strings.Contains(name, "linux-arm64") {
				targetAssetURL = a.BrowserDownloadURL
				break
			} else if goarch == "amd64" && strings.Contains(name, "linux-amd64") {
				targetAssetURL = a.BrowserDownloadURL
				break
			}
		}
	}

	if targetAssetURL == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": fmt.Sprintf("最新 Release 中未找到适用于当前系统平台 (%s/%s) 的安装包", goos, goarch),
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

	// 1. 下载新版本到临时文件 .new
	newExePath := currentExe + ".new"
	downloadClient := &http.Client{Timeout: 5 * time.Minute}
	req, _ := http.NewRequest("GET", targetAssetURL, nil)
	req.Header.Set("User-Agent", "workbuddy2api-updater")
	resp, err := downloadClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "下载更新包失败: " + err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": fmt.Sprintf("下载更新包 HTTP 状态异常: %d", resp.StatusCode),
		})
		return
	}

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
			// 在 Docker 容器中作为 PID 1 或主进程，通过退出并由 restart: unless-stopped 策略拉起新版本
			if h.cfg.StopFunc != nil {
				h.cfg.StopFunc()
			}
			os.Exit(0)
			return
		}

		// 本地桌面或独立进程模式：启动新进程并退出旧进程
		cmd := exec.Command(currentExe, os.Args[1:]...)
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


