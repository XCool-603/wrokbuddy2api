package server

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

//go:embed dashboard.html
var dashboardHTML string

func (h *Handler) RegisterWebUI() {
	h.mux.HandleFunc("GET /{$}", h.handleDashboardHTML)
	h.mux.HandleFunc("GET /ui/data", h.handleDashboardData)
	h.mux.HandleFunc("POST /ui/oauth/start", h.handleOAuthStart)
	h.mux.HandleFunc("POST /ui/oauth/poll", h.handleOAuthPoll)
	h.mux.HandleFunc("POST /ui/oauth/import", h.handleOAuthImport)
	h.mux.HandleFunc("POST /ui/account/admin", h.handleAccountAdmin)
	h.mux.HandleFunc("POST /ui/action/signin", h.handleActionSignin)
	h.mux.HandleFunc("POST /ui/action/trial", h.handleActionTrial)
	h.mux.HandleFunc("POST /ui/action/delete", h.handleActionDelete)
	h.mux.HandleFunc("POST /ui/config/apikey", h.handleConfigAPIKey)
	h.mux.HandleFunc("GET /ui/action/backup", h.handleActionBackup)
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
		item := AccountItem{
			UID:      a.UID,
			Realm:    a.Realm(),
			Nickname: a.Nickname,
			Filename: filepath.Base(f),
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
		"accounts":   accounts,
		"models":     models,
		"apiKey":     h.GetAPIKey(),
		"recentLogs": GetRecentLogs(),
		"stats":      statsSnap,
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
	_ = os.Remove(target)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
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

	// 持久化到 config.json
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
	dataMap["api_key"] = newKey

	if encoded, err := json.MarshalIndent(dataMap, "", "  "); err == nil {
		_ = os.WriteFile(cfgFile, encoded, 0644)
	}

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
